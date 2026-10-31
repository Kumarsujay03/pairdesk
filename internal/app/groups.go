package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

// GroupView is a group with online count.
type GroupView struct {
	Group   model.Group
	Members []model.User
	Online  int
	Mine    bool
}

// Groups lists my groups.
func (a *App) Groups() []GroupView {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []GroupView
	for _, g := range a.groups {
		out = append(out, GroupView{Group: g, Mine: g.Owner == a.me.ID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group.Name < out[j].Group.Name })
	return out
}

// GroupByName finds a group I belong to.
func (a *App) GroupByName(name string) (model.Group, bool) {
	name = strings.TrimPrefix(strings.ToLower(name), "#")
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, g := range a.groups {
		if strings.ToLower(g.Name) == name {
			return g, true
		}
	}
	return model.Group{}, false
}

// CreateGroup makes a group and wraps a fresh key for myself.
func (a *App) CreateGroup(ctx context.Context, name string) (*Conversation, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	if name == "" || len(name) > 40 {
		return nil, errors.New("group name must be 1-40 chars")
	}
	gk, err := crypto.NewGroupKey()
	if err != nil {
		return nil, err
	}
	self, err := crypto.Pairwise(a.id, a.id.Public())
	if err != nil {
		return nil, err
	}
	// Wrap for self with a placeholder group id; backend assigns the real id,
	// so we re-wrap after creation.
	g, err := a.be.CreateGroup(ctx, name, mustWrap(self, "pending", gk))
	if err != nil {
		return nil, err
	}
	// The backend assigned the id; rewrap under it and bump the epoch so the
	// placeholder wrap can never be used.
	g.KeyEpoch++
	if err := a.be.RotateGroupKey(ctx, g.ID, g.KeyEpoch, []model.GroupMember{{GroupID: g.ID, UserID: a.me.ID, WrappedKey: mustWrap(self, g.ID, gk), KeyEpoch: g.KeyEpoch, WrappedBy: a.me.ID}}); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.groups[g.ID] = g
	a.groupKeys[g.ID] = gk
	a.mu.Unlock()
	c := a.ensureConv(model.ConvGroup, g.ID)
	a.emit(Event{Kind: EvGroupChanged, GroupID: g.ID})
	return c, nil
}

func mustWrap(pw [crypto.KeySize]byte, gid string, gk [crypto.KeySize]byte) []byte {
	w, err := crypto.WrapKey(pw, gid, gk)
	if err != nil {
		panic(err)
	}
	return w
}

// loadGroupKey unwraps my member key for a group.
func (a *App) loadGroupKey(ctx context.Context, g model.Group) error {
	ms, err := a.be.ListGroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.UserID != a.me.ID {
			continue
		}
		var pw [crypto.KeySize]byte
		if m.WrappedBy == a.me.ID {
			pw, err = crypto.Pairwise(a.id, a.id.Public())
		} else {
			pw, err = a.pairwiseKey(ctx, m.WrappedBy)
		}
		if err != nil {
			return err
		}
		gk, err := crypto.UnwrapKey(pw, g.ID, m.WrappedKey)
		if err != nil {
			return fmt.Errorf("unwrap: %w", err)
		}
		a.mu.Lock()
		a.groupKeys[g.ID] = gk
		a.mu.Unlock()
		return nil
	}
	return errors.New("not a member")
}

// InviteToGroup wraps the group key for a friend and adds them.
func (a *App) InviteToGroup(ctx context.Context, g model.Group, name string) error {
	if g.Owner != a.me.ID {
		return errors.New("only the group owner can invite")
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if !a.IsFriend(u.ID) {
		return fmt.Errorf("%s must be your friend first", u.Username)
	}
	a.mu.RLock()
	gk, ok := a.groupKeys[g.ID]
	a.mu.RUnlock()
	if !ok {
		return errors.New("no key for this group")
	}
	pw, err := a.pairwiseKey(ctx, u.ID)
	if err != nil {
		return err
	}
	return a.be.AddGroupMember(ctx, model.GroupMember{GroupID: g.ID, UserID: u.ID, WrappedKey: mustWrap(pw, g.ID, gk), KeyEpoch: g.KeyEpoch, WrappedBy: a.me.ID})
}

// RemoveFromGroup removes a member and rotates the key for everyone left.
func (a *App) RemoveFromGroup(ctx context.Context, g model.Group, name string) error {
	if g.Owner != a.me.ID {
		return errors.New("only the group owner can remove")
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if err := a.be.RemoveGroupMember(ctx, g.ID, u.ID); err != nil {
		return err
	}
	return a.rotateGroup(ctx, g)
}

// LeaveGroup leaves; the owner rotates on their side when they notice.
func (a *App) LeaveGroup(ctx context.Context, g model.Group) error {
	if g.Owner == a.me.ID {
		return errors.New("owners cannot leave; /group transfer or /group delete")
	}
	if err := a.be.RemoveGroupMember(ctx, g.ID, a.me.ID); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.groups, g.ID)
	delete(a.groupKeys, g.ID)
	delete(a.convs, g.ID)
	a.mu.Unlock()
	a.emit(Event{Kind: EvGroupChanged, GroupID: g.ID})
	return nil
}

// DeleteGroup removes a group (owner or admin).
func (a *App) DeleteGroup(ctx context.Context, g model.Group) error {
	if g.Owner != a.me.ID && !a.IsAdmin() {
		return errors.New("only the owner or admin can delete")
	}
	if err := a.be.DeleteGroup(ctx, g.ID); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.groups, g.ID)
	delete(a.groupKeys, g.ID)
	delete(a.convs, g.ID)
	a.mu.Unlock()
	a.emit(Event{Kind: EvGroupChanged, GroupID: g.ID})
	return nil
}

// TransferGroup hands ownership to a member.
func (a *App) TransferGroup(ctx context.Context, g model.Group, name string) error {
	if g.Owner != a.me.ID && !a.IsAdmin() {
		return errors.New("only the owner or admin can transfer")
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	return a.be.TransferGroup(ctx, g.ID, u.ID)
}

// rotateGroup generates a new key and rewraps for every remaining member.
func (a *App) rotateGroup(ctx context.Context, g model.Group) error {
	ms, err := a.be.ListGroupMembers(ctx, g.ID)
	if err != nil {
		return err
	}
	gk, err := crypto.NewGroupKey()
	if err != nil {
		return err
	}
	epoch := g.KeyEpoch + 1
	var out []model.GroupMember
	for _, m := range ms {
		var pw [crypto.KeySize]byte
		if m.UserID == a.me.ID {
			pw, err = crypto.Pairwise(a.id, a.id.Public())
		} else {
			pw, err = a.pairwiseKey(ctx, m.UserID)
		}
		if err != nil {
			return err
		}
		out = append(out, model.GroupMember{GroupID: g.ID, UserID: m.UserID, WrappedKey: mustWrap(pw, g.ID, gk), KeyEpoch: epoch, WrappedBy: a.me.ID})
	}
	if err := a.be.RotateGroupKey(ctx, g.ID, epoch, out); err != nil {
		return err
	}
	a.mu.Lock()
	g.KeyEpoch = epoch
	a.groups[g.ID] = g
	a.groupKeys[g.ID] = gk
	a.mu.Unlock()
	return nil
}

// GroupMembers lists members with presence.
func (a *App) GroupMembers(ctx context.Context, g model.Group) ([]Friend, error) {
	ms, err := a.be.ListGroupMembers(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.UserID)
	}
	_ = a.cacheUsers(ctx, ids)
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []Friend
	for _, m := range ms {
		if u, ok := a.users[m.UserID]; ok {
			out = append(out, Friend{User: u, Online: a.online[m.UserID] || m.UserID == a.me.ID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User.Username < out[j].User.Username })
	return out, nil
}
