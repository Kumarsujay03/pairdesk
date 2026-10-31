package memory

import (
	"context"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Friendships are one row per unordered pair, keyed like a DM conversation.
// RLS: insert as requester only; update status as addressee only; either
// party may delete.

// SendFriendRequest creates a pending row. A pending or accepted row is a
// conflict, and so is re-asking within denyCooldown of a denial.
func (c *Client) SendFriendRequest(_ context.Context, toUserID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	if toUserID == me.ID {
		return rendezvous.ErrForbidden
	}
	if s.users[toUserID] == nil {
		return rendezvous.ErrNotFound
	}
	now := s.now()
	if f := s.friendship(me.ID, toUserID); f != nil {
		if f.Status != model.FriendDenied || now.Sub(f.UpdatedAt) < denyCooldown {
			return rendezvous.ErrConflict
		}
	}
	f := &model.Friendship{
		Requester: me.ID,
		Addressee: toUserID,
		Status:    model.FriendPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.friendships[model.DMConvID(me.ID, toUserID)] = f
	s.emitFriendship(f, false)
	return nil
}

// RespondFriendRequest is the addressee's call alone.
func (c *Client) RespondFriendRequest(_ context.Context, requesterID string, accept bool) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	f := s.friendship(me.ID, requesterID)
	if f == nil {
		return rendezvous.ErrNotFound
	}
	if f.Addressee != me.ID || f.Requester != requesterID || f.Status != model.FriendPending {
		return rendezvous.ErrForbidden
	}
	f.Status = model.FriendDenied
	if accept {
		f.Status = model.FriendAccepted
	}
	f.UpdatedAt = s.now()
	s.emitFriendship(f, false)
	return nil
}

// RemoveFriend deletes the row whatever its status; either party may.
func (c *Client) RemoveFriend(_ context.Context, userID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return err
	}
	f := s.friendship(me.ID, userID)
	if f == nil {
		return rendezvous.ErrNotFound
	}
	delete(s.friendships, model.DMConvID(me.ID, userID))
	s.emitFriendship(f, true)
	return nil
}

func (c *Client) ListFriendships(context.Context) ([]model.Friendship, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return nil, err
	}
	var out []model.Friendship
	for _, f := range s.friendships {
		if f.Requester == me.ID || f.Addressee == me.ID {
			out = append(out, *f)
		}
	}
	sortBy(out, func(a, b model.Friendship) bool { return a.CreatedAt.Before(b.CreatedAt) })
	return out, nil
}

func (s *Store) emitFriendship(f *model.Friendship, deleted bool) {
	row := *f
	s.deliver(rendezvous.Event{Kind: rendezvous.EvFriendship, Friendship: &row, Deleted: deleted},
		only(f.Requester, f.Addressee))
}
