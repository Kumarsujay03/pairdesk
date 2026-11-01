package supabase

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// phxMessage is the Phoenix v1 JSON frame used by Supabase Realtime.
type phxMessage struct {
	Topic   string          `json:"topic"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	Ref     string          `json:"ref"`
	JoinRef string          `json:"join_ref,omitempty"`
}

type channel struct {
	topic    string
	config   map[string]any
	joinRef  string
	joined   bool
	presence map[string]int // presence key -> live metas
}

// realtime owns the websocket, the joined channels and the event stream.
type realtime struct {
	c *Client

	heartbeat  time.Duration
	backoffMin time.Duration
	backoffMax time.Duration

	mu       sync.Mutex
	events   chan rendezvous.Event
	started  bool
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	me       string
	online   bool
	channels map[string]*channel
	conn     *websocket.Conn
	refSeq   uint64

	writeMu sync.Mutex
}

func newRealtime(c *Client) *realtime {
	return &realtime{
		c:          c,
		heartbeat:  25 * time.Second,
		backoffMin: time.Second,
		backoffMax: 30 * time.Second,
		channels:   map[string]*channel{},
	}
}

// wsURL derives the realtime endpoint from the project URL.
func (r *realtime) wsURL() string {
	u := r.c.base
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + u[len("http://"):]
	}
	return u + "/realtime/v1/websocket?apikey=" + r.c.anon + "&vsn=1.0.0"
}

func (r *realtime) push(ev rendezvous.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.events == nil {
		return
	}
	select {
	case r.events <- ev:
	default: // consumer is behind; drop rather than stall the socket
	}
}

func (r *realtime) nextRef() string {
	r.refSeq++
	return strconv.FormatUint(r.refSeq, 10)
}

func (r *realtime) token() string {
	r.c.mu.RLock()
	defer r.c.mu.RUnlock()
	if r.c.session == nil {
		return ""
	}
	return r.c.session.AccessToken
}

// ------------------------------------------------------------ lifecycle

func (r *realtime) connect(ctx context.Context) (<-chan rendezvous.Event, error) {
	me, err := r.c.uid()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, rendezvous.ErrNotConnected
	}
	if r.started {
		ch := r.events
		r.mu.Unlock()
		return ch, nil
	}
	r.started = true
	r.me = me
	r.events = make(chan rendezvous.Event, 256)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.done = make(chan struct{})
	r.c.mu.RLock()
	admin := r.c.adminToken != ""
	r.c.mu.RUnlock()
	r.channels["realtime:db"] = &channel{topic: "realtime:db", config: dbConfig(me, admin), presence: map[string]int{}}
	r.mu.Unlock()

	conn, err := r.dial(ctx)
	if err != nil {
		r.mu.Lock()
		r.started = false
		r.mu.Unlock()
		r.cancel()
		return nil, err
	}
	go r.run(conn)
	go func() { _ = r.c.TouchLastSeen(r.ctx) }()
	return r.events, nil
}

// dbConfig lists the table changes to follow. A normal device follows only
// its own users row; an admin device follows every row so new sign-ups and
// moderation changes arrive live (RLS still limits what is delivered).
func dbConfig(me string, admin bool) map[string]any {
	changes := []map[string]any{}
	for _, t := range []string{"messages", "friendships", "groups", "group_members", "grants", "instance_settings"} {
		changes = append(changes, map[string]any{"event": "*", "schema": "public", "table": t})
	}
	users := map[string]any{"event": "*", "schema": "public", "table": "users", "filter": "id=eq." + me}
	if admin {
		users = map[string]any{"event": "*", "schema": "public", "table": "users"}
	}
	changes = append(changes,
		map[string]any{"event": "*", "schema": "public", "table": "signals", "filter": "to_user=eq." + me},
		users,
	)
	return map[string]any{
		"postgres_changes": changes,
		"broadcast":        map[string]any{"self": false},
		"presence":         map[string]any{"key": ""},
		"private":          false,
	}
}

func presenceConfig(me string) map[string]any {
	return map[string]any{
		"postgres_changes": []any{},
		"broadcast":        map[string]any{"self": false},
		"presence":         map[string]any{"key": me},
		"private":          true,
	}
}

func (r *realtime) dial(ctx context.Context) (*websocket.Conn, error) {
	hc := *r.c.http
	hc.Timeout = 0
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dctx, r.wsURL(), &websocket.DialOptions{HTTPClient: &hc})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, errors.New("supabase: realtime dial failed")
	}
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

// run serves one connection after another until closed.
func (r *realtime) run(conn *websocket.Conn) {
	defer func() {
		r.mu.Lock()
		r.closed = true
		close(r.events)
		r.mu.Unlock()
		close(r.done)
	}()
	for {
		err := r.serve(conn)
		if r.ctx.Err() != nil {
			return
		}
		r.push(rendezvous.Event{Kind: rendezvous.EvDisconnected, Err: err})
		wait := r.backoffMin
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(wait):
			}
			c, derr := r.dial(r.ctx)
			if derr == nil {
				conn = c
				break
			}
			if r.ctx.Err() != nil {
				return
			}
			wait *= 2
			if wait > r.backoffMax {
				wait = r.backoffMax
			}
		}
		r.push(rendezvous.Event{Kind: rendezvous.EvReconnected})
	}
}

// serve runs one connection: joins every channel, heartbeats and reads
// until the socket fails.
func (r *realtime) serve(conn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	defer conn.CloseNow()

	r.mu.Lock()
	r.conn = conn
	for _, ch := range r.channels {
		ch.joined = false
		ch.presence = map[string]int{}
	}
	chans := make([]*channel, 0, len(r.channels))
	for _, ch := range r.channels {
		chans = append(chans, ch)
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.conn == conn {
			r.conn = nil
		}
		for _, ch := range r.channels {
			ch.joined = false
		}
		r.mu.Unlock()
	}()

	for _, ch := range chans {
		if err := r.join(ch); err != nil {
			return err
		}
	}

	go func() {
		t := time.NewTicker(r.heartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.mu.Lock()
				ref := r.nextRef()
				r.mu.Unlock()
				if err := r.send(conn, phxMessage{Topic: "phoenix", Event: "heartbeat", Payload: json.RawMessage(`{}`), Ref: ref}); err != nil {
					conn.CloseNow()
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var m phxMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		r.handle(conn, m)
	}
}

func (r *realtime) send(conn *websocket.Conn, m phxMessage) error {
	if conn == nil {
		return rendezvous.ErrNotConnected
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

// join sends phx_join for ch on the current connection.
func (r *realtime) join(ch *channel) error {
	r.mu.Lock()
	conn := r.conn
	ch.joinRef = r.nextRef()
	ch.joined = false
	payload := map[string]any{"config": ch.config}
	if tok := r.token(); tok != "" {
		payload["access_token"] = tok
	}
	r.mu.Unlock()
	b, _ := json.Marshal(payload)
	return r.send(conn, phxMessage{Topic: ch.topic, Event: "phx_join", Payload: b, Ref: ch.joinRef, JoinRef: ch.joinRef})
}

func (r *realtime) close() error {
	r.mu.Lock()
	if !r.started {
		r.closed = true
		r.mu.Unlock()
		return nil
	}
	cancel, conn, done := r.cancel, r.conn, r.done
	r.mu.Unlock()
	cancel()
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "bye")
	}
	<-done
	return nil
}

// tokenChanged pushes a fresh access token to every joined channel.
func (r *realtime) tokenChanged(tok string) {
	if tok == "" {
		return
	}
	r.mu.Lock()
	conn := r.conn
	var joined []*channel
	for _, ch := range r.channels {
		if ch.joined {
			joined = append(joined, ch)
		}
	}
	r.mu.Unlock()
	if conn == nil {
		return
	}
	b, _ := json.Marshal(map[string]string{"access_token": tok})
	for _, ch := range joined {
		r.mu.Lock()
		ref := r.nextRef()
		r.mu.Unlock()
		_ = r.send(conn, phxMessage{Topic: ch.topic, Event: "access_token", Payload: b, Ref: ref, JoinRef: ch.joinRef})
	}
}

// ---------------------------------------------------------------- inbound

func (r *realtime) handle(conn *websocket.Conn, m phxMessage) {
	switch m.Event {
	case "phx_reply":
		var rep struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(m.Payload, &rep)
		r.mu.Lock()
		ch := r.channels[m.Topic]
		var track bool
		if ch != nil && m.Ref == ch.joinRef {
			ch.joined = rep.Status == "ok"
			track = ch.joined && r.online && m.Topic == "realtime:user:"+r.me
		}
		r.mu.Unlock()
		if rep.Status != "ok" && ch != nil && m.Ref == ch.joinRef {
			r.c.log.Warn("realtime join refused", "topic", m.Topic, "reply", string(m.Payload))
		}
		if track {
			_ = r.sendPresence(conn, true)
		}
	case "phx_error", "phx_close":
		r.mu.Lock()
		ch := r.channels[m.Topic]
		if ch != nil {
			ch.joined = false
		}
		r.mu.Unlock()
		if ch != nil && m.Event == "phx_error" {
			time.AfterFunc(r.backoffMin, func() {
				r.mu.Lock()
				same := r.conn == conn
				r.mu.Unlock()
				if same {
					_ = r.join(ch)
				}
			})
		}
	case "postgres_changes":
		r.handleChange(m.Payload)
	case "presence_state":
		var state map[string]struct {
			Metas []json.RawMessage `json:"metas"`
		}
		if json.Unmarshal(m.Payload, &state) != nil {
			return
		}
		r.mu.Lock()
		ch := r.channels[m.Topic]
		if ch == nil {
			r.mu.Unlock()
			return
		}
		ch.presence = map[string]int{}
		for k, v := range state {
			ch.presence[k] = len(v.Metas)
		}
		r.mu.Unlock()
		r.emitPresence(m.Topic)
	case "presence_diff":
		var diff struct {
			Joins  map[string]struct{ Metas []json.RawMessage } `json:"joins"`
			Leaves map[string]struct{ Metas []json.RawMessage } `json:"leaves"`
		}
		if json.Unmarshal(m.Payload, &diff) != nil {
			return
		}
		r.mu.Lock()
		ch := r.channels[m.Topic]
		if ch == nil {
			r.mu.Unlock()
			return
		}
		for k, v := range diff.Joins {
			ch.presence[k] += len(v.Metas)
		}
		for k, v := range diff.Leaves {
			ch.presence[k] -= len(v.Metas)
			if ch.presence[k] <= 0 {
				delete(ch.presence, k)
			}
		}
		r.mu.Unlock()
		r.emitPresence(m.Topic)
	}
}

func (r *realtime) emitPresence(topic string) {
	if !strings.HasPrefix(topic, "realtime:user:") {
		return
	}
	id := strings.TrimPrefix(topic, "realtime:user:")
	r.mu.Lock()
	ch := r.channels[topic]
	online := false
	if ch != nil {
		for _, n := range ch.presence {
			if n > 0 {
				online = true
				break
			}
		}
	}
	r.mu.Unlock()
	r.push(rendezvous.Event{Kind: rendezvous.EvPresence, UserID: id, Online: online})
}

// handleChange translates a postgres_changes frame into an Event.
func (r *realtime) handleChange(payload json.RawMessage) {
	var pc struct {
		Data struct {
			Type      string          `json:"type"`
			Table     string          `json:"table"`
			Record    json.RawMessage `json:"record"`
			OldRecord json.RawMessage `json:"old_record"`
		} `json:"data"`
	}
	if json.Unmarshal(payload, &pc) != nil {
		return
	}
	deleted := pc.Data.Type == "DELETE"
	rec := pc.Data.Record
	if deleted || len(rec) == 0 {
		rec = pc.Data.OldRecord
	}
	rec = undoubleBytea(rec)
	ev := rendezvous.Event{Deleted: deleted}
	switch pc.Data.Table {
	case "messages":
		var row messageRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		m := row.model()
		ev.Kind, ev.Message = rendezvous.EvMessage, &m
	case "friendships":
		var row friendshipRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		f := row.model()
		ev.Kind, ev.Friendship = rendezvous.EvFriendship, &f
	case "groups":
		var row groupRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		g := row.model()
		ev.Kind, ev.Group = rendezvous.EvGroup, &g
	case "group_members":
		var row memberRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		gm := row.model()
		ev.Kind, ev.GroupMember = rendezvous.EvGroupMember, &gm
	case "grants":
		var row grantRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		g := row.model()
		ev.Kind, ev.Grant = rendezvous.EvGrant, &g
	case "users":
		var row userRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		u := row.model()
		ev.Kind, ev.User, ev.UserID = rendezvous.EvUser, &u, u.ID
	case "instance_settings":
		// A single key changed; deliver the whole flattened table.
		go func() {
			ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
			defer cancel()
			s, err := r.c.Settings(ctx)
			if err != nil {
				return
			}
			r.push(rendezvous.Event{Kind: rendezvous.EvSettings, Settings: &s})
		}()
		return
	case "signals":
		if deleted {
			return
		}
		var row signalRow
		if json.Unmarshal(rec, &row) != nil {
			return
		}
		s := row.model()
		ev.Kind, ev.Signal = rendezvous.EvSignal, &s
		r.push(ev)
		go func() {
			ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
			defer cancel()
			_ = r.c.deleteSignal(ctx, row.ID)
		}()
		return
	default:
		return
	}
	r.push(ev)
}

// --------------------------------------------------------------- presence

func (r *realtime) watchPresence(ids []string) error {
	r.mu.Lock()
	if !r.started || r.closed {
		r.mu.Unlock()
		return rendezvous.ErrNotConnected
	}
	var fresh []*channel
	for _, id := range ids {
		topic := "realtime:user:" + id
		if _, ok := r.channels[topic]; ok {
			continue
		}
		ch := &channel{topic: topic, config: presenceConfig(r.me), presence: map[string]int{}}
		r.channels[topic] = ch
		fresh = append(fresh, ch)
	}
	connected := r.conn != nil
	r.mu.Unlock()
	if !connected {
		return nil // joined on reconnect
	}
	for _, ch := range fresh {
		if err := r.join(ch); err != nil {
			return err
		}
	}
	return nil
}

func (r *realtime) setOnline(online bool) error {
	r.mu.Lock()
	if !r.started || r.closed {
		r.mu.Unlock()
		return rendezvous.ErrNotConnected
	}
	r.online = online
	topic := "realtime:user:" + r.me
	ch := r.channels[topic]
	if ch == nil {
		ch = &channel{topic: topic, config: presenceConfig(r.me), presence: map[string]int{}}
		r.channels[topic] = ch
	}
	conn, joined := r.conn, ch.joined
	r.mu.Unlock()
	if conn == nil {
		return nil
	}
	if !joined {
		// The phx_reply handler tracks once the join is acknowledged.
		return r.join(ch)
	}
	return r.sendPresence(conn, online)
}

func (r *realtime) sendPresence(conn *websocket.Conn, online bool) error {
	r.mu.Lock()
	topic := "realtime:user:" + r.me
	ch := r.channels[topic]
	ref := r.nextRef()
	joinRef := ""
	if ch != nil {
		joinRef = ch.joinRef
	}
	r.mu.Unlock()
	var payload map[string]any
	if online {
		payload = map[string]any{"type": "track", "event": "track",
			"payload": map[string]any{"online_at": r.c.now().UTC().Format(time.RFC3339)}}
	} else {
		payload = map[string]any{"type": "untrack", "event": "untrack"}
	}
	b, _ := json.Marshal(payload)
	return r.send(conn, phxMessage{Topic: topic, Event: "presence", Payload: b, Ref: ref, JoinRef: joinRef})
}

// ------------------------------------------------------ Backend methods

func (c *Client) Connect(ctx context.Context) (<-chan rendezvous.Event, error) {
	return c.rt.connect(ctx)
}

func (c *Client) WatchPresence(_ context.Context, userIDs []string) error {
	return c.rt.watchPresence(userIDs)
}

func (c *Client) SetOnline(_ context.Context, online bool) error {
	return c.rt.setOnline(online)
}
