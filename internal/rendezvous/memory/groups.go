package memory

import (
	"context"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Groups are invite-only. The owner may only add their own accepted
// friends; anyone may leave; the owner removes, rotates and transfers; the
// admin may dissolve or transfer any group but is never handed a key.

// CreateGroup makes the caller the owner and first member at key epoch 1.
func (c *Client) CreateGroup(_ context.Context, name string, ownerWrappedKey []byte) (model.Group, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return model.Group{}, err
	}
	g := &model.Group{ID: newID(), Name: name, Owner: me.ID, KeyEpoch: 1, CreatedAt: s.now()}
	s.groups[g.ID] = g
	m := &model.GroupMember{
		GroupID:    g.ID,
		UserID:     me.ID,
		WrappedKey: append([]byte(nil), ownerWrappedKey...),
		KeyEpoch:   1,
		WrappedBy:  me.ID,
	}
	s.members[g.ID] = map[string]*model.GroupMember{me.ID: m}
	s.emitGroup(g, false, me.ID)
	s.emitMember(m, false, me.ID)
	return *g, nil
}

// ListGroups returns the groups the caller belongs to. The admin sees
// every group (names and owners only; keys are per member).
func (c *Client) ListGroups(context.Context) ([]model.Group, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return nil, err
	}
	var out []model.Group
	for _, g := range s.groups {
		if me.Role == model.RoleAdmin || s.isMember(g.ID, me.ID) {
			out = append(out, *g)
		}
	}
	sortBy(out, func(a, b model.Group) bool { return a.CreatedAt.Before(b.CreatedAt) })
	return out, nil
}

func (c *Client) ListGroupMembers(_ context.Context, groupID string) ([]model.GroupMember, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return nil, err
	}
	if s.groups[groupID] == nil {
		return nil, rendezvous.ErrNotFound
	}
	if me.Role != model.RoleAdmin && !s.isMember(groupID, me.ID) {
		return nil, rendezvous.ErrForbidden
	}
	var out []model.GroupMember
	for _, m := range s.members[groupID] {
		out = append(out, copyMember(m))
	}
	sortBy(out, func(a, b model.GroupMember) bool { return a.UserID < b.UserID })
	return out, nil
}

// AddGroupMember is the owner's call and only for an accepted friend.
func (c *Client) AddGroupMember(_ context.Context, m model.GroupMember) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	g := s.groups[m.GroupID]
	if g == nil {
		return rendezvous.ErrNotFound
	}
	if g.Owner != me.ID || !s.areFriends(me.ID, m.UserID) {
		return rendezvous.ErrForbidden
	}
	if s.isMember(g.ID, m.UserID) {
		return rendezvous.ErrConflict
	}
	row := copyMember(&m)
	row.WrappedBy = me.ID
	if row.KeyEpoch == 0 {
		row.KeyEpoch = g.KeyEpoch
	}
	s.members[g.ID][row.UserID] = &row
	s.emitGroup(g, false, row.UserID)
	s.emitMember(&row, false, s.memberIDs(g.ID)...)
	return nil
}

// RemoveGroupMember: the owner removes anyone else; anyone else leaves.
// The owner cannot leave without transferring first.
func (c *Client) RemoveGroupMember(_ context.Context, groupID, userID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	g := s.groups[groupID]
	if g == nil {
		return rendezvous.ErrNotFound
	}
	if userID == g.Owner || (me.ID != g.Owner && me.ID != userID) {
		return rendezvous.ErrForbidden
	}
	m := s.members[groupID][userID]
	if m == nil {
		return rendezvous.ErrNotFound
	}
	delete(s.members[groupID], userID)
	s.emitMember(m, true, append(s.memberIDs(groupID), userID)...)
	return nil
}

// RotateGroupKey installs a new epoch. The owner must rewrap for exactly
// the current member set: an unknown user is forbidden, a missing member
// would be locked out and is a conflict, as is a non-increasing epoch.
func (c *Client) RotateGroupKey(_ context.Context, groupID string, epoch int, members []model.GroupMember) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	g := s.groups[groupID]
	if g == nil {
		return rendezvous.ErrNotFound
	}
	if g.Owner != me.ID {
		return rendezvous.ErrForbidden
	}
	if epoch <= g.KeyEpoch {
		return rendezvous.ErrConflict
	}
	seen := map[string]bool{}
	for _, m := range members {
		if !s.isMember(groupID, m.UserID) {
			return rendezvous.ErrForbidden
		}
		seen[m.UserID] = true
	}
	if len(seen) != len(s.members[groupID]) {
		return rendezvous.ErrConflict
	}
	g.KeyEpoch = epoch
	ids := s.memberIDs(groupID)
	for _, m := range members {
		row := copyMember(&m)
		row.GroupID = groupID
		row.KeyEpoch = epoch
		row.WrappedBy = me.ID
		s.members[groupID][row.UserID] = &row
	}
	s.emitGroup(g, false, ids...)
	for _, id := range ids {
		s.emitMember(s.members[groupID][id], false, ids...)
	}
	return nil
}

func (c *Client) DeleteGroup(_ context.Context, groupID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	g := s.groups[groupID]
	if g == nil {
		return rendezvous.ErrNotFound
	}
	if g.Owner != me.ID && me.Role != model.RoleAdmin {
		return rendezvous.ErrForbidden
	}
	ids := s.memberIDs(groupID)
	delete(s.groups, groupID)
	delete(s.members, groupID)
	s.emitGroup(g, true, ids...)
	if me.Role == model.RoleAdmin && g.Owner != me.ID {
		s.logAdmin(me, "group.dissolve", groupID, g.Name)
	}
	return nil
}

// TransferGroup hands ownership to an existing member.
func (c *Client) TransferGroup(_ context.Context, groupID, newOwner string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	g := s.groups[groupID]
	if g == nil {
		return rendezvous.ErrNotFound
	}
	if g.Owner != me.ID && me.Role != model.RoleAdmin {
		return rendezvous.ErrForbidden
	}
	if !s.isMember(groupID, newOwner) {
		return rendezvous.ErrForbidden
	}
	g.Owner = newOwner
	s.emitGroup(g, false, s.memberIDs(groupID)...)
	if me.Role == model.RoleAdmin && me.ID != newOwner {
		s.logAdmin(me, "group.transfer", groupID, newOwner)
	}
	return nil
}

func (s *Store) emitGroup(g *model.Group, deleted bool, to ...string) {
	row := *g
	s.deliver(rendezvous.Event{Kind: rendezvous.EvGroup, Group: &row, Deleted: deleted}, only(to...))
}

func (s *Store) emitMember(m *model.GroupMember, deleted bool, to ...string) {
	row := copyMember(m)
	s.deliver(rendezvous.Event{Kind: rendezvous.EvGroupMember, GroupMember: &row, Deleted: deleted}, only(to...))
}
