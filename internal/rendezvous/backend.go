// Package rendezvous is the only place PairDesk talks to its hosted
// backend. Everything is expressed through Backend so the application can
// run against Supabase in production and against an in-memory fake in
// tests. The backend never sees plaintext chat; callers encrypt first.
package rendezvous

import (
	"context"
	"errors"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
)

var (
	ErrNotFound      = errors.New("rendezvous: not found")
	ErrUnauthorized  = errors.New("rendezvous: unauthorized")
	ErrForbidden     = errors.New("rendezvous: forbidden")
	ErrConflict      = errors.New("rendezvous: conflict")
	ErrRateLimited   = errors.New("rendezvous: rate limited")
	ErrRegClosed     = errors.New("rendezvous: registration closed")
	ErrBadInvite     = errors.New("rendezvous: invalid invite")
	ErrUsernameTaken = errors.New("rendezvous: username taken")
	ErrMaintenance   = errors.New("rendezvous: maintenance mode")
	ErrBanned        = errors.New("rendezvous: banned")
	ErrNotConnected  = errors.New("rendezvous: not connected")
)

// Session is what a login yields. The refresh token is persisted locally so
// the user does not log in on every launch.
type Session struct {
	UserID       string
	Email        string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// EventKind enumerates realtime notifications the backend can push.
type EventKind string

const (
	EvMessage      EventKind = "message"       // Message set
	EvFriendship   EventKind = "friendship"    // Friendship set
	EvPresence     EventKind = "presence"      // UserID + Online set
	EvGroup        EventKind = "group"         // Group set (created/updated/deleted)
	EvGroupMember  EventKind = "group_member"  // GroupMember set
	EvGrant        EventKind = "grant"         // Grant set
	EvSettings     EventKind = "settings"      // Settings set
	EvSignal       EventKind = "signal"        // Signal set
	EvUser         EventKind = "user"          // User set (ban/mute/role change)
	EvDisconnected EventKind = "disconnected"  // realtime link dropped; Err may be set
	EvReconnected  EventKind = "reconnected"
)

type Event struct {
	Kind        EventKind
	Message     *model.Message
	Friendship  *model.Friendship
	Group       *model.Group
	GroupMember *model.GroupMember
	Grant       *model.Grant
	Settings    *model.Settings
	Signal      *Signal
	User        *model.User
	UserID      string
	Online      bool
	Deleted     bool
	Err         error
}

// Signal is an opaque, already-encrypted signaling blob for a P2P session.
type Signal struct {
	SessionID string
	From      string
	To        string
	Type      string // offer|answer|ice|noise1|noise2|bye
	Payload   []byte
}

// MessageQuery selects a page of messages.
type MessageQuery struct {
	ConvType model.ConvType
	ConvID   string
	After    time.Time
	Before   time.Time
	Limit    int
}

// Backend is the full contract. Methods return ErrUnauthorized when there is
// no session and ErrForbidden when RLS rejects the call.
type Backend interface {
	// --- auth ---
	RequestMagicLink(ctx context.Context, email string) error
	VerifyOTP(ctx context.Context, email, code string) (Session, error)
	Restore(ctx context.Context, refreshToken string) (Session, error)
	Session() *Session
	SignOut(ctx context.Context) error

	// --- registration ---
	CheckUsername(ctx context.Context, username string) (available bool, err error)
	Register(ctx context.Context, username, pubkey, inviteCode string) (model.User, error)
	Me(ctx context.Context) (model.User, error)
	Settings(ctx context.Context) (model.Settings, error)

	// --- users ---
	UserByName(ctx context.Context, username string) (model.User, error)
	UserByID(ctx context.Context, id string) (model.User, error)
	UsersByIDs(ctx context.Context, ids []string) ([]model.User, error)
	ListUsers(ctx context.Context) ([]model.User, error) // admin only

	// --- friends ---
	SendFriendRequest(ctx context.Context, toUserID string) error
	RespondFriendRequest(ctx context.Context, requesterID string, accept bool) error
	RemoveFriend(ctx context.Context, userID string) error
	ListFriendships(ctx context.Context) ([]model.Friendship, error)

	// --- messages ---
	SendMessage(ctx context.Context, m model.Message) (model.Message, error)
	ListMessages(ctx context.Context, q MessageQuery) ([]model.Message, error)

	// --- groups ---
	CreateGroup(ctx context.Context, name string, ownerWrappedKey []byte) (model.Group, error)
	ListGroups(ctx context.Context) ([]model.Group, error)
	ListGroupMembers(ctx context.Context, groupID string) ([]model.GroupMember, error)
	AddGroupMember(ctx context.Context, m model.GroupMember) error
	RemoveGroupMember(ctx context.Context, groupID, userID string) error
	RotateGroupKey(ctx context.Context, groupID string, epoch int, members []model.GroupMember) error
	DeleteGroup(ctx context.Context, groupID string) error
	TransferGroup(ctx context.Context, groupID, newOwner string) error

	// --- grants ---
	PutGrant(ctx context.Context, g model.Grant) (model.Grant, error)
	RevokeGrant(ctx context.Context, id string) error
	ListGrants(ctx context.Context) ([]model.Grant, error)

	// --- sessions log (metadata only) ---
	LogSession(ctx context.Context, s model.SessionLog) error
	EndSession(ctx context.Context, id string) error

	// --- admin ---
	SetSetting(ctx context.Context, key string, value any) error
	CreateInvite(ctx context.Context, uses int, expires *time.Time) (model.Invite, error)
	ListInvites(ctx context.Context) ([]model.Invite, error)
	RevokeInvite(ctx context.Context, code string) error
	SetUserFlag(ctx context.Context, userID, flag string, value bool) error // banned|muted|approved
	SetUserRole(ctx context.Context, userID string, role model.Role) error
	RenameUser(ctx context.Context, userID, newName string) error
	ResetUserKey(ctx context.Context, userID string) error
	KickUser(ctx context.Context, userID string) error
	ListAdminLog(ctx context.Context, limit int) ([]model.AdminLogEntry, error)
	Stats(ctx context.Context) (model.Stats, error)
	ListAdminDevices(ctx context.Context) ([]model.AdminDevice, error)
	RegisterAdminDevice(ctx context.Context, deviceID, pubkey string) error
	ApproveAdminDevice(ctx context.Context, deviceID string) error
	UpdatePubKey(ctx context.Context, pubkey string) error

	// --- realtime ---
	// Connect opens the realtime link and starts delivering events for the
	// current user: their conversations, friendships, groups, grants,
	// settings and signaling addressed to them. It is safe to call once.
	Connect(ctx context.Context) (<-chan Event, error)
	// WatchPresence subscribes to the online state of the given users.
	// The backend enforces that only friends or the admin may watch.
	WatchPresence(ctx context.Context, userIDs []string) error
	// SetOnline announces this user's presence.
	SetOnline(ctx context.Context, online bool) error
	// SendSignal relays an encrypted signaling blob to one user.
	SendSignal(ctx context.Context, s Signal) error
	Close() error
}
