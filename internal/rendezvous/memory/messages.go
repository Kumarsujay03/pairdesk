package memory

import (
	"context"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// SendMessage stores one ciphertext row. The sender is always the session
// user; the conversation must be one the caller is party to (see
// Store.canReadConv); muted users cannot send; Settings.MsgsPerMin is a
// per-user sliding window.
func (c *Client) SendMessage(_ context.Context, m model.Message) (model.Message, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(true)
	if err != nil {
		return model.Message{}, err
	}
	if me.Muted || !s.canReadConv(me, m.ConvType, m.ConvID) {
		return model.Message{}, rendezvous.ErrForbidden
	}
	now := s.now()
	if !s.allowSend(me.ID, now) {
		return model.Message{}, rendezvous.ErrRateLimited
	}
	m = copyMessage(m)
	m.ID = newID()
	m.Sender = me.ID
	m.CreatedAt = now
	s.messages = append(s.messages, m)
	row := copyMessage(m)
	s.deliver(rendezvous.Event{Kind: rendezvous.EvMessage, Message: &row},
		only(s.convParties(m.ConvType, m.ConvID)...))
	return copyMessage(m), nil
}

// allowSend records a send at now and reports whether it was within the
// rate limit. Zero means unlimited.
func (s *Store) allowSend(userID string, now time.Time) bool {
	limit := s.settings.MsgsPerMin
	if limit <= 0 {
		return true
	}
	cutoff := now.Add(-time.Minute)
	kept := s.msgTimes[userID][:0]
	for _, t := range s.msgTimes[userID] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		s.msgTimes[userID] = kept
		return false
	}
	s.msgTimes[userID] = append(kept, now)
	return true
}

// ListMessages returns rows in chronological order. After and Before are
// exclusive bounds when set; Limit keeps the most recent rows of the
// selection, so a client pages backwards by moving Before.
func (c *Client) ListMessages(_ context.Context, q rendezvous.MessageQuery) ([]model.Message, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.caller(false)
	if err != nil {
		return nil, err
	}
	if !s.canReadConv(me, q.ConvType, q.ConvID) {
		return nil, rendezvous.ErrForbidden
	}
	var out []model.Message
	for _, m := range s.messages {
		if m.ConvType != q.ConvType || m.ConvID != q.ConvID {
			continue
		}
		if !q.After.IsZero() && !m.CreatedAt.After(q.After) {
			continue
		}
		if !q.Before.IsZero() && !m.CreatedAt.Before(q.Before) {
			continue
		}
		out = append(out, copyMessage(m))
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}
