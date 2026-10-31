package memory

import (
	"context"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Connect opens this client's event channel. Calling it again returns the
// same channel. Events are delivered for everything the user is party to;
// see Store.deliver and the emit helpers next to each table.
func (c *Client) Connect(context.Context) (<-chan rendezvous.Event, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := c.registered(); err != nil {
		return nil, err
	}
	if c.events == nil {
		c.events = make(chan rendezvous.Event, eventBuffer)
		c.watching = map[string]bool{}
	}
	return c.events, nil
}

// WatchPresence subscribes to the given users. Only accepted friends (or,
// for the admin, anyone) are accepted; the rest are silently ignored, as
// the private presence channel's join policy would. A snapshot of each
// accepted user's current state is pushed immediately.
func (c *Client) WatchPresence(_ context.Context, userIDs []string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return err
	}
	if c.events == nil {
		return rendezvous.ErrNotConnected
	}
	for _, id := range userIDs {
		if s.users[id] == nil {
			continue
		}
		if id != me.ID && me.Role != model.RoleAdmin && !s.areFriends(me.ID, id) {
			continue
		}
		c.watching[id] = true
		c.push(rendezvous.Event{Kind: rendezvous.EvPresence, UserID: id, Online: s.online[id]})
	}
	return nil
}

// SetOnline announces the caller's presence to everyone watching them.
func (c *Client) SetOnline(_ context.Context, online bool) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return err
	}
	me.LastSeen = s.now()
	if online {
		s.online[me.ID] = true
	} else {
		delete(s.online, me.ID)
	}
	s.deliverPresence(me.ID, online)
	return nil
}

// SendSignal relays an opaque blob to one user. From is forced to the
// caller; a recipient who is not connected simply misses it.
func (c *Client) SendSignal(_ context.Context, sig rendezvous.Signal) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return err
	}
	if s.users[sig.To] == nil {
		return rendezvous.ErrNotFound
	}
	sig.From = me.ID
	sig.Payload = append([]byte(nil), sig.Payload...)
	s.deliver(rendezvous.Event{Kind: rendezvous.EvSignal, Signal: &sig}, only(sig.To))
	return nil
}

// Close stops delivery and closes the channel. The client stays usable
// for plain calls; Connect reopens a fresh channel.
func (c *Client) Close() error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.events != nil {
		close(c.events)
		c.events = nil
		c.watching = nil
	}
	return nil
}
