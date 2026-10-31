package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Conversation is the decrypted view of one DM or group.
type Conversation struct {
	Type     model.ConvType
	ID       string
	Msgs     []Msg
	Unread   int
	LastRead int64
	loaded   bool
}

// Title renders the conversation's name.
func (a *App) ConvTitle(c *Conversation) string {
	if c.Type == model.ConvGroup {
		a.mu.RLock()
		g, ok := a.groups[c.ID]
		a.mu.RUnlock()
		if ok {
			return "#" + g.Name
		}
		return "#?"
	}
	parts := strings.SplitN(c.ID, ":", 2)
	other := parts[0]
	if other == a.me.ID && len(parts) > 1 {
		other = parts[1]
	}
	return a.Name(other)
}

// Other returns the peer id of a DM conversation.
func (a *App) ConvPeer(c *Conversation) string {
	if c.Type != model.ConvDM {
		return ""
	}
	parts := strings.SplitN(c.ID, ":", 2)
	if parts[0] == a.me.ID && len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}

func (a *App) ensureConv(t model.ConvType, id string) *Conversation {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.convs[id]
	if !ok {
		c = &Conversation{Type: t, ID: id}
		a.convs[id] = c
	}
	return c
}

// Conversations lists conversations, most recent activity first.
func (a *App) Conversations() []*Conversation {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Conversation, 0, len(a.convs))
	for _, c := range a.convs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := int64(0), int64(0)
		if n := len(out[i].Msgs); n > 0 {
			li = out[i].Msgs[n-1].At
		}
		if n := len(out[j].Msgs); n > 0 {
			lj = out[j].Msgs[n-1].At
		}
		if li != lj {
			return li > lj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Conv returns one conversation by id.
func (a *App) Conv(id string) *Conversation { a.mu.RLock(); defer a.mu.RUnlock(); return a.convs[id] }

// OpenDM returns (creating if needed) the DM with a username. Non-admins may
// only DM friends; the admin may DM anyone.
func (a *App) OpenDM(ctx context.Context, name string) (*Conversation, error) {
	u, err := a.UserByName(ctx, name)
	if errors.Is(err, rendezvous.ErrNotFound) {
		return nil, fmt.Errorf("no user named %q", name)
	}
	if err != nil {
		return nil, err
	}
	if u.ID == a.me.ID {
		return nil, errors.New("talking to yourself is free, but not here")
	}
	if !a.IsFriend(u.ID) && !a.IsAdmin() && u.Role != model.RoleAdmin {
		return nil, fmt.Errorf("%s is not your friend yet; /add %s first", u.Username, u.Username)
	}
	c := a.ensureConv(model.ConvDM, model.DMConvID(a.me.ID, u.ID))
	if err := a.LoadHistory(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadHistory pulls the latest page once.
func (a *App) LoadHistory(ctx context.Context, c *Conversation) error {
	a.mu.RLock()
	loaded := c.loaded
	a.mu.RUnlock()
	if loaded {
		return nil
	}
	ms, err := a.be.ListMessages(ctx, rendezvous.MessageQuery{ConvType: c.Type, ConvID: c.ID, Limit: 200})
	if err != nil {
		return err
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].CreatedAt.Before(ms[j].CreatedAt) })
	a.mu.Lock()
	c.loaded = true
	seen := map[string]bool{}
	for _, m := range c.Msgs {
		seen[m.ID] = true
	}
	a.mu.Unlock()
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.Sender)
	}
	_ = a.cacheUsers(ctx, ids)
	for _, m := range ms {
		if seen[m.ID] {
			continue
		}
		d := a.decrypt(m)
		a.mu.Lock()
		c.Msgs = append(c.Msgs, d)
		a.mu.Unlock()
		a.index(d)
	}
	a.mu.Lock()
	sort.SliceStable(c.Msgs, func(i, j int) bool { return c.Msgs[i].At < c.Msgs[j].At })
	a.mu.Unlock()
	return nil
}

// MarkRead clears unread on a conversation.
func (a *App) MarkRead(c *Conversation) {
	a.mu.Lock()
	c.Unread = 0
	if n := len(c.Msgs); n > 0 {
		c.LastRead = c.Msgs[n-1].At
	}
	a.mu.Unlock()
}

type wire struct {
	K  string `json:"k"`           // text | reaction | me | system | file | typing
	T  string `json:"t,omitempty"` // body
	R  string `json:"r,omitempty"` // reacts-to message id
	V  int    `json:"v"`           // format version
}

// Send encrypts and sends a text message.
func (a *App) Send(ctx context.Context, c *Conversation, text string) error {
	return a.sendWire(ctx, c, wire{K: "text", T: text, V: 1})
}

// SendKind sends a message of an arbitrary kind (file manifests, seeds).
func (a *App) SendKind(ctx context.Context, c *Conversation, kind, text string) error {
	return a.sendWire(ctx, c, wire{K: kind, T: text, V: 1})
}

// SendMe sends a /me action.
func (a *App) SendMe(ctx context.Context, c *Conversation, text string) error {
	return a.sendWire(ctx, c, wire{K: "me", T: text, V: 1})
}

// React sends a reaction to the last message by someone else.
func (a *App) React(ctx context.Context, c *Conversation, emoji string) error {
	a.mu.RLock()
	var target string
	for i := len(c.Msgs) - 1; i >= 0; i-- {
		if c.Msgs[i].Kind == "text" || c.Msgs[i].Kind == "me" {
			target = c.Msgs[i].ID
			break
		}
	}
	a.mu.RUnlock()
	if target == "" {
		return errors.New("nothing to react to")
	}
	return a.sendWire(ctx, c, wire{K: "reaction", T: emoji, R: target, V: 1})
}

func (a *App) sendWire(ctx context.Context, c *Conversation, w wire) error {
	if strings.TrimSpace(w.T) == "" && w.K != "typing" {
		return nil
	}
	if len(w.T) > 8000 && w.K != "file" {
		return errors.New("message too long (8000 chars max)")
	}
	key, epoch, err := a.convKey(ctx, c)
	if err != nil {
		return err
	}
	pt, _ := json.Marshal(w)
	aad := []byte(string(c.Type) + ":" + c.ID)
	nonce, ct, err := crypto.Seal(key, aad, pt)
	if err != nil {
		return err
	}
	m, err := a.be.SendMessage(ctx, model.Message{ConvType: c.Type, ConvID: c.ID, Ciphertext: ct, Nonce: nonce, KeyEpoch: epoch})
	if err != nil {
		if errors.Is(err, rendezvous.ErrRateLimited) {
			return errors.New("slow down: rate limit hit")
		}
		return err
	}
	// Deliver locally right away so the UI does not wait for the echo.
	a.ingest(m, true)
	return nil
}

// convKey returns the symmetric key for a conversation.
func (a *App) convKey(ctx context.Context, c *Conversation) ([crypto.KeySize]byte, int, error) {
	if c.Type == model.ConvDM {
		k, err := a.pairwiseKey(ctx, a.ConvPeer(c))
		return k, 1, err
	}
	a.mu.RLock()
	k, ok := a.groupKeys[c.ID]
	g := a.groups[c.ID]
	a.mu.RUnlock()
	if !ok {
		return k, 0, errors.New("no key for this group yet")
	}
	return k, g.KeyEpoch, nil
}

// pairwiseKey derives (and caches) the DM key with a peer.
func (a *App) pairwiseKey(ctx context.Context, peer string) ([crypto.KeySize]byte, error) {
	a.mu.RLock()
	k, ok := a.pairwise[peer]
	a.mu.RUnlock()
	if ok {
		return k, nil
	}
	u, err := a.User(ctx, peer)
	if err != nil {
		return k, err
	}
	if u.PubKey == "" {
		return k, fmt.Errorf("%s has no device key yet", u.Username)
	}
	pk, err := crypto.ParsePublicKey(u.PubKey)
	if err != nil {
		return k, err
	}
	k, err = crypto.Pairwise(a.id, pk)
	if err != nil {
		return k, err
	}
	a.mu.Lock()
	a.pairwise[peer] = k
	a.mu.Unlock()
	return k, nil
}

// decrypt turns a stored message into a Msg; failures are shown, not hidden.
func (a *App) decrypt(m model.Message) Msg {
	out := Msg{ID: m.ID, ConvType: m.ConvType, ConvID: m.ConvID, Sender: m.Sender, At: m.CreatedAt.UnixNano(), Mine: m.Sender == a.me.ID, Kind: "text"}
	var key [crypto.KeySize]byte
	var err error
	if m.ConvType == model.ConvDM {
		other := m.Sender
		if other == a.me.ID {
			parts := strings.SplitN(m.ConvID, ":", 2)
			other = parts[0]
			if other == a.me.ID && len(parts) > 1 {
				other = parts[1]
			}
		}
		key, err = a.pairwiseKey(a.ctx, other)
	} else {
		a.mu.RLock()
		var ok bool
		key, ok = a.groupKeys[m.ConvID]
		a.mu.RUnlock()
		if !ok {
			err = errors.New("no group key")
		}
	}
	if err != nil {
		out.Text = "⟨cannot decrypt: " + err.Error() + "⟩"
		return out
	}
	pt, err := crypto.Open(key, []byte(string(m.ConvType)+":"+m.ConvID), m.Nonce, m.Ciphertext)
	if err != nil {
		out.Text = "⟨cannot decrypt⟩"
		return out
	}
	var w wire
	if json.Unmarshal(pt, &w) != nil {
		out.Text = "⟨bad message⟩"
		return out
	}
	out.Kind, out.Text, out.ReactsTo, out.Decrypted = w.K, w.T, w.R, true
	return out
}

// ingest adds a message to its conversation and emits an event.
func (a *App) ingest(m model.Message, local bool) {
	_ = a.cacheUsers(a.ctx, []string{m.Sender})
	c := a.ensureConv(m.ConvType, m.ConvID)
	a.mu.Lock()
	for _, e := range c.Msgs {
		if e.ID == m.ID {
			a.mu.Unlock()
			return
		}
	}
	a.mu.Unlock()
	d := a.decrypt(m)
	if d.Kind == "typing" {
		a.mu.Lock()
		a.typing[m.ConvID+"|"+m.Sender] = time.Now()
		a.mu.Unlock()
		a.emit(Event{Kind: EvTyping, UserID: m.Sender, Text: m.ConvID})
		return
	}
	a.mu.Lock()
	c.Msgs = append(c.Msgs, d)
	if !d.Mine && !local {
		c.Unread++
	}
	a.mu.Unlock()
	a.index(d)
	if !local {
		a.maybeNotify(d)
	}
	a.emit(Event{Kind: EvMessage, Msg: &d})
}

// TypingIn reports who is typing in a conversation (last 4 seconds).
func (a *App) TypingIn(convID string) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []string
	for k, t := range a.typing {
		if strings.HasPrefix(k, convID+"|") && time.Since(t) < 4*time.Second {
			out = append(out, strings.TrimPrefix(k, convID+"|"))
		}
	}
	return out
}
