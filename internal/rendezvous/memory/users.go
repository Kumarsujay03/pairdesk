package memory

import (
	"context"
	"strings"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// --- registration ---

// CheckUsername reports whether username could be registered right now.
// Invalid names are simply unavailable.
func (c *Client) CheckUsername(_ context.Context, username string) (bool, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := c.signedIn(); err != nil {
		return false, err
	}
	name := strings.ToLower(username)
	return usernameRE.MatchString(name) && s.userByName(name) == nil, nil
}

// Register mirrors the register_username RPC. The first user ever to
// register becomes the admin and bypasses the registration gates; after
// that the mode, cap, invite and approval settings apply.
func (c *Client) Register(_ context.Context, username, pubkey, inviteCode string) (model.User, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := c.signedIn()
	if err != nil {
		return model.User{}, err
	}
	if existing := s.users[id]; existing != nil {
		if existing.Banned {
			return model.User{}, rendezvous.ErrBanned
		}
		return model.User{}, rendezvous.ErrConflict
	}
	name := strings.ToLower(username)
	if !usernameRE.MatchString(name) {
		return model.User{}, ErrInvalidUsername
	}

	first := len(s.users) == 0
	var invite *model.Invite
	if !first {
		if s.settings.Maintenance {
			return model.User{}, rendezvous.ErrMaintenance
		}
		switch s.settings.Registration {
		case model.RegClosed:
			return model.User{}, rendezvous.ErrRegClosed
		case model.RegInvite:
			invite = s.invites[inviteCode]
			if invite == nil || invite.UsesLeft <= 0 ||
				(invite.ExpiresAt != nil && !s.now().Before(*invite.ExpiresAt)) {
				return model.User{}, rendezvous.ErrBadInvite
			}
		}
		if s.settings.UserCap > 0 && len(s.users) >= s.settings.UserCap {
			return model.User{}, rendezvous.ErrRegClosed
		}
	}
	if s.userByName(name) != nil {
		return model.User{}, rendezvous.ErrUsernameTaken
	}

	now := s.now()
	u := &model.User{
		ID:        id,
		Username:  name,
		PubKey:    pubkey,
		Role:      model.RoleUser,
		CreatedAt: now,
		LastSeen:  now,
		Approved:  first || !s.settings.RequireApproval,
	}
	if first {
		u.Role = model.RoleAdmin
		s.settings.AdminPubKey = pubkey
	}
	if invite != nil {
		invite.UsesLeft--
	}
	s.users[id] = u
	return copyUser(u), nil
}

func (c *Client) Me(context.Context) (model.User, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := c.signedIn()
	if err != nil {
		return model.User{}, err
	}
	u := s.users[id]
	if u == nil {
		return model.User{}, rendezvous.ErrNotFound
	}
	if u.Banned {
		return model.User{}, rendezvous.ErrBanned
	}
	return copyUser(u), nil
}

// Settings is readable by every signed-in account, registered or not: a
// fresh install needs to know whether registration is open.
func (c *Client) Settings(context.Context) (model.Settings, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := c.signedIn()
	if err != nil {
		return model.Settings{}, err
	}
	if u := s.users[id]; u != nil && u.Banned {
		return model.Settings{}, rendezvous.ErrBanned
	}
	return s.settings, nil
}

// --- users ---

func (c *Client) UserByName(_ context.Context, username string) (model.User, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := c.caller(false); err != nil {
		return model.User{}, err
	}
	u := s.userByName(strings.ToLower(username))
	if u == nil {
		return model.User{}, rendezvous.ErrNotFound
	}
	return copyUser(u), nil
}

func (c *Client) UserByID(_ context.Context, id string) (model.User, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := c.caller(false); err != nil {
		return model.User{}, err
	}
	u := s.users[id]
	if u == nil {
		return model.User{}, rendezvous.ErrNotFound
	}
	return copyUser(u), nil
}

// UsersByIDs returns the rows that exist, in the order asked; unknown ids
// are skipped.
func (c *Client) UsersByIDs(_ context.Context, ids []string) ([]model.User, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := c.caller(false); err != nil {
		return nil, err
	}
	out := make([]model.User, 0, len(ids))
	for _, id := range ids {
		if u := s.users[id]; u != nil {
			out = append(out, copyUser(u))
		}
	}
	return out, nil
}

// ListUsers is admin only; it is the only call that exposes everyone.
func (c *Client) ListUsers(context.Context) ([]model.User, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := c.admin(); err != nil {
		return nil, err
	}
	out := make([]model.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, copyUser(u))
	}
	sortBy(out, func(a, b model.User) bool { return a.CreatedAt.Before(b.CreatedAt) })
	return out, nil
}

// UpdatePubKey replaces the caller's own device key. When the admin
// rotates, Settings.AdminPubKey follows so clients can re-pin.
func (c *Client) UpdatePubKey(_ context.Context, pubkey string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := c.registered()
	if err != nil {
		return err
	}
	if s.settings.Maintenance && u.Role != model.RoleAdmin {
		return rendezvous.ErrMaintenance
	}
	u.PubKey = pubkey
	if u.Role == model.RoleAdmin {
		s.settings.AdminPubKey = pubkey
		settings := s.settings
		s.deliver(rendezvous.Event{Kind: rendezvous.EvSettings, Settings: &settings}, everyone)
	}
	row := copyUser(u)
	s.deliver(rendezvous.Event{Kind: rendezvous.EvUser, User: &row}, only(u.ID))
	return nil
}
