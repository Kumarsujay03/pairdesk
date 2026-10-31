package memory

import (
	"context"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Grants are hints: clients enforce from their own signed copy. RLS still
// insists the grantor is the caller, the grantee is the admin or a helper,
// and only the grantor can revoke.

// PutGrant inserts, or replaces when g.ID names a grant the caller owns.
func (c *Client) PutGrant(_ context.Context, g model.Grant) (model.Grant, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return model.Grant{}, err
	}
	grantee := s.users[g.Grantee]
	if grantee == nil || (grantee.Role != model.RoleAdmin && grantee.Role != model.RoleHelper) {
		return model.Grant{}, rendezvous.ErrForbidden
	}
	if g.ID != "" {
		if old := s.grants[g.ID]; old != nil && old.Grantor != me.ID {
			return model.Grant{}, rendezvous.ErrForbidden
		}
	} else {
		g.ID = newID()
	}
	row := copyGrant(&g)
	row.Grantor = me.ID
	row.RevokedAt = nil
	if row.CreatedAt.IsZero() {
		row.CreatedAt = s.now()
	}
	s.grants[row.ID] = &row
	s.emitGrant(&row)
	return copyGrant(&row), nil
}

// RevokeGrant marks the row revoked; revoking twice is harmless.
func (c *Client) RevokeGrant(_ context.Context, id string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	g := s.grants[id]
	if g == nil {
		return rendezvous.ErrNotFound
	}
	if g.Grantor != me.ID {
		return rendezvous.ErrForbidden
	}
	if g.RevokedAt == nil {
		t := s.now()
		g.RevokedAt = &t
	}
	s.emitGrant(g)
	return nil
}

// ListGrants returns every grant the caller gave or received, revoked
// ones included.
func (c *Client) ListGrants(context.Context) ([]model.Grant, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return nil, err
	}
	var out []model.Grant
	for _, g := range s.grants {
		if g.Grantor == me.ID || g.Grantee == me.ID {
			out = append(out, copyGrant(g))
		}
	}
	sortBy(out, func(a, b model.Grant) bool { return a.CreatedAt.Before(b.CreatedAt) })
	return out, nil
}

func (s *Store) emitGrant(g *model.Grant) {
	row := copyGrant(g)
	s.deliver(rendezvous.Event{Kind: rendezvous.EvGrant, Grant: &row}, only(g.Grantor, g.Grantee))
}

// --- session logs (metadata only) ---

// LogSession records a remote-access session. The visitor defaults to the
// caller; any approved user may write their own rows.
func (c *Client) LogSession(_ context.Context, l model.SessionLog) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	if l.ID == "" {
		l.ID = newID()
	}
	if l.Visitor == "" {
		l.Visitor = me.ID
	}
	if l.StartedAt.IsZero() {
		l.StartedAt = s.now()
	}
	if existing := s.sessionLogs[l.ID]; existing != nil && existing.Visitor != me.ID && existing.Host != me.ID {
		return rendezvous.ErrForbidden
	}
	s.sessionLogs[l.ID] = &l
	return nil
}

// EndSession stamps EndedAt; either party may.
func (c *Client) EndSession(_ context.Context, id string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	l := s.sessionLogs[id]
	if l == nil {
		return rendezvous.ErrNotFound
	}
	if l.Visitor != me.ID && l.Host != me.ID {
		return rendezvous.ErrForbidden
	}
	if l.EndedAt == nil {
		t := s.now()
		l.EndedAt = &t
	}
	return nil
}
