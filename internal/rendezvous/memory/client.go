package memory

import (
	"context"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Client is one signed-in user's view of the Store. Its fields are guarded
// by the store mutex so that fan-out and Close never race.
type Client struct {
	store    *Store
	session  *rendezvous.Session
	events   chan rendezvous.Event // nil until Connect, nil again after Close
	watching map[string]bool       // presence subscriptions
}

var _ rendezvous.Backend = (*Client)(nil)

// push is a non-blocking send; the caller holds store.mu.
func (c *Client) push(ev rendezvous.Event) {
	select {
	case c.events <- ev:
	default:
	}
}

// --- gates shared by every method (callers hold store.mu) ---
//
// The hosted database layers its policies the same way: a session must
// exist (ErrUnauthorized), the row behind it must not be banned
// (ErrBanned), writes are refused during maintenance unless the caller is
// the admin (ErrMaintenance), and most tables additionally require the
// account to be approved (ErrForbidden).

// signedIn returns the user id behind the session, registered or not.
func (c *Client) signedIn() (string, error) {
	if c.session == nil {
		return "", rendezvous.ErrUnauthorized
	}
	return c.session.UserID, nil
}

// registered returns the caller's user row and applies the ban check.
func (c *Client) registered() (*model.User, error) {
	id, err := c.signedIn()
	if err != nil {
		return nil, err
	}
	u := c.store.users[id]
	if u == nil {
		return nil, rendezvous.ErrForbidden
	}
	if u.Banned {
		return nil, rendezvous.ErrBanned
	}
	return u, nil
}

// caller is the gate for ordinary table access. write adds the
// maintenance predicate.
func (c *Client) caller(write bool) (*model.User, error) {
	u, err := c.registered()
	if err != nil {
		return nil, err
	}
	if write && c.store.settings.Maintenance && u.Role != model.RoleAdmin {
		return nil, rendezvous.ErrMaintenance
	}
	if !u.Approved {
		return nil, rendezvous.ErrForbidden
	}
	return u, nil
}

// admin is the gate for /admin commands. Every admin call is a write for
// maintenance purposes, which the admin is exempt from anyway.
func (c *Client) admin() (*model.User, error) {
	u, err := c.caller(true)
	if err != nil {
		return nil, err
	}
	if u.Role != model.RoleAdmin {
		return nil, rendezvous.ErrForbidden
	}
	return u, nil
}

// --- auth ---

// RequestMagicLink only records that the email asked to sign in. In test
// mode the code is always TestOTP.
func (c *Client) RequestMagicLink(_ context.Context, email string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	email = normalizeEmail(email)
	if _, ok := s.emails[email]; !ok {
		s.emails[email] = newID()
	}
	return nil
}

// VerifyOTP accepts TestOTP for any email. The same email always maps to
// the same user id so a user can sign in from several clients.
func (c *Client) VerifyOTP(_ context.Context, email, code string) (rendezvous.Session, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if code != TestOTP {
		return rendezvous.Session{}, rendezvous.ErrUnauthorized
	}
	email = normalizeEmail(email)
	id, ok := s.emails[email]
	if !ok {
		id = newID()
		s.emails[email] = id
	}
	return c.issueSession(id, email), nil
}

// Restore exchanges a refresh token issued earlier for a fresh session.
// The old token is retired.
func (c *Client) Restore(_ context.Context, refreshToken string) (rendezvous.Session, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.refresh[refreshToken]
	if !ok {
		return rendezvous.Session{}, rendezvous.ErrUnauthorized
	}
	delete(s.refresh, refreshToken)
	var email string
	for e, uid := range s.emails {
		if uid == id {
			email = e
		}
	}
	return c.issueSession(id, email), nil
}

func (c *Client) issueSession(userID, email string) rendezvous.Session {
	sess := rendezvous.Session{
		UserID:       userID,
		Email:        email,
		AccessToken:  newID(),
		RefreshToken: newID(),
		ExpiresAt:    c.store.now().Add(time.Hour),
	}
	c.store.refresh[sess.RefreshToken] = userID
	c.session = &sess
	return sess
}

// Session returns a copy of the current session, or nil when signed out.
func (c *Client) Session() *rendezvous.Session {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	if c.session == nil {
		return nil
	}
	sess := *c.session
	return &sess
}

func (c *Client) SignOut(context.Context) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.session != nil {
		delete(s.refresh, c.session.RefreshToken)
		c.session = nil
	}
	return nil
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}
