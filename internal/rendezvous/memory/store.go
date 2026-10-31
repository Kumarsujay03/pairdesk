// Package memory is an in-memory rendezvous.Backend. It exists for tests
// and for an offline demo mode, and it doubles as the readable reference for
// the authorization rules the hosted database enforces with Row Level
// Security: every ErrForbidden here corresponds to a policy there.
//
// One Store plays the role of the database and the realtime relay. Each
// Client returned by Store.Client is one signed-in user talking to it, with
// its own Session and its own event channel.
package memory

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// ErrInvalidUsername is returned by Register and RenameUser when the name
// is not 3-20 characters of [a-z0-9_].
var ErrInvalidUsername = errors.New("memory: invalid username")

// TestOTP is the one-time code VerifyOTP accepts for any email.
const TestOTP = "000000"

// eventBuffer is the capacity of each client's event channel. A client
// that falls this far behind loses events instead of blocking the store.
const eventBuffer = 256

// denyCooldown is how long a denied requester must wait before asking
// the same person again.
const denyCooldown = 24 * time.Hour

var usernameRE = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

// Store holds all instance state. It is safe for concurrent use.
type Store struct {
	mu  sync.Mutex
	now func() time.Time

	settings model.Settings

	// auth
	emails  map[string]string // email -> auth user id
	refresh map[string]string // refresh token -> auth user id

	users       map[string]*model.User       // user id -> row
	friendships map[string]*model.Friendship // pair key (model.DMConvID) -> row
	messages    []model.Message
	groups      map[string]*model.Group
	members     map[string]map[string]*model.GroupMember // group id -> user id -> row
	grants      map[string]*model.Grant
	invites     map[string]*model.Invite
	devices     map[string]*model.AdminDevice
	adminLog    []model.AdminLogEntry
	sessionLogs map[string]*model.SessionLog
	online      map[string]bool
	msgTimes    map[string][]time.Time // per-user send timestamps for rate limiting

	clients map[*Client]struct{}
}

// New returns an empty instance with open registration and no limits.
func New() *Store {
	return &Store{
		now:         time.Now,
		settings:    model.Settings{Registration: model.RegOpen},
		emails:      map[string]string{},
		refresh:     map[string]string{},
		users:       map[string]*model.User{},
		friendships: map[string]*model.Friendship{},
		groups:      map[string]*model.Group{},
		members:     map[string]map[string]*model.GroupMember{},
		grants:      map[string]*model.Grant{},
		invites:     map[string]*model.Invite{},
		devices:     map[string]*model.AdminDevice{},
		sessionLogs: map[string]*model.SessionLog{},
		online:      map[string]bool{},
		msgTimes:    map[string][]time.Time{},
		clients:     map[*Client]struct{}{},
	}
}

// SetClock replaces the time source. Tests use it to move past cooldowns
// and expiries.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Client returns a new, signed-out client sharing this store.
func (s *Store) Client() rendezvous.Backend {
	return s.newClient()
}

func (s *Store) newClient() *Client {
	c := &Client{store: s}
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	return c
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("memory: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// --- lookups (callers hold s.mu) ---

func (s *Store) userByName(name string) *model.User {
	for _, u := range s.users {
		if u.Username == name {
			return u
		}
	}
	return nil
}

func (s *Store) adminUser() *model.User {
	for _, u := range s.users {
		if u.Role == model.RoleAdmin {
			return u
		}
	}
	return nil
}

func (s *Store) friendship(a, b string) *model.Friendship {
	return s.friendships[model.DMConvID(a, b)]
}

func (s *Store) areFriends(a, b string) bool {
	f := s.friendship(a, b)
	return f != nil && f.Status == model.FriendAccepted
}

func (s *Store) isMember(groupID, userID string) bool {
	_, ok := s.members[groupID][userID]
	return ok
}

func (s *Store) memberIDs(groupID string) []string {
	ids := make([]string, 0, len(s.members[groupID]))
	for id := range s.members[groupID] {
		ids = append(ids, id)
	}
	return ids
}

// canReadConv reports whether user may read or write the conversation.
// This is the messages RLS policy: DM parties must be accepted friends or
// one of them must be the admin; group messages need membership.
func (s *Store) canReadConv(user *model.User, convType model.ConvType, convID string) bool {
	switch convType {
	case model.ConvDM:
		other, ok := dmOther(convID, user.ID)
		if !ok {
			return false
		}
		o := s.users[other]
		if o == nil {
			return false
		}
		return s.areFriends(user.ID, other) ||
			user.Role == model.RoleAdmin || o.Role == model.RoleAdmin
	case model.ConvGroup:
		return s.isMember(convID, user.ID)
	}
	return false
}

// dmOther extracts the other party from a canonical DM conversation id and
// verifies me is a party and the id is in canonical (sorted) form.
func dmOther(convID, me string) (string, bool) {
	for i := 0; i < len(convID); i++ {
		if convID[i] != ':' {
			continue
		}
		a, b := convID[:i], convID[i+1:]
		var other string
		switch me {
		case a:
			other = b
		case b:
			other = a
		default:
			continue
		}
		if model.DMConvID(me, other) == convID {
			return other, true
		}
	}
	return "", false
}

func (s *Store) logAdmin(actor *model.User, action, target, detail string) {
	s.adminLog = append(s.adminLog, model.AdminLogEntry{
		ID:     int64(len(s.adminLog) + 1),
		Actor:  actor.ID,
		Action: action,
		Target: target,
		Detail: detail,
		At:     s.now(),
	})
}

// --- realtime fan-out (callers hold s.mu) ---

// deliver hands ev to every connected client whose user satisfies want.
// Sends never block: a full channel drops the event.
func (s *Store) deliver(ev rendezvous.Event, want func(userID string) bool) {
	for c := range s.clients {
		if c.events == nil || c.session == nil || !want(c.session.UserID) {
			continue
		}
		c.push(ev)
	}
}

func everyone(string) bool { return true }

func only(ids ...string) func(string) bool {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return func(id string) bool {
		_, ok := set[id]
		return ok
	}
}

// deliverPresence tells every client watching userID about its new state.
func (s *Store) deliverPresence(userID string, online bool) {
	ev := rendezvous.Event{Kind: rendezvous.EvPresence, UserID: userID, Online: online}
	for c := range s.clients {
		if c.events != nil && c.watching[userID] {
			c.push(ev)
		}
	}
}

func (s *Store) convParties(convType model.ConvType, convID string) []string {
	if convType == model.ConvGroup {
		return s.memberIDs(convID)
	}
	for i := 0; i < len(convID); i++ {
		if convID[i] == ':' {
			if a, b := convID[:i], convID[i+1:]; s.users[a] != nil && s.users[b] != nil {
				return []string{a, b}
			}
		}
	}
	return nil
}

// --- copies, so events and results never alias store rows ---

func copyUser(u *model.User) model.User { return *u }

func copyMember(m *model.GroupMember) model.GroupMember {
	c := *m
	c.WrappedKey = append([]byte(nil), m.WrappedKey...)
	return c
}

func copyGrant(g *model.Grant) model.Grant {
	c := *g
	c.Signature = append([]byte(nil), g.Signature...)
	if g.RevokedAt != nil {
		t := *g.RevokedAt
		c.RevokedAt = &t
	}
	return c
}

func copyMessage(m model.Message) model.Message {
	m.Ciphertext = append([]byte(nil), m.Ciphertext...)
	m.Nonce = append([]byte(nil), m.Nonce...)
	return m
}
