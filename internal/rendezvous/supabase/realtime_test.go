package supabase

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// wsFake is a Phoenix-speaking realtime server. It replies ok to joins,
// records what it saw, and lets the test inject frames.
type wsFake struct {
	t     *testing.T
	mu    sync.Mutex
	conns []*websocket.Conn
	joins []phxMessage
	sent  []phxMessage
	open  chan *websocket.Conn
	pendingJoins []phxMessage
	joinC chan phxMessage
}

func newWSFake(t *testing.T) *wsFake {
	return &wsFake{t: t, open: make(chan *websocket.Conn, 8), joinC: make(chan phxMessage, 32)}
}

func (f *wsFake) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("apikey") != anon || r.URL.Query().Get("vsn") != "1.0.0" {
		http.Error(w, "bad query", 400)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	f.open <- conn
	ctx := context.Background()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m phxMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		f.mu.Lock()
		f.sent = append(f.sent, m)
		f.mu.Unlock()
		switch m.Event {
		case "phx_join":
			f.mu.Lock()
			f.joins = append(f.joins, m)
			f.mu.Unlock()
			rep := map[string]any{"status": "ok", "response": map[string]any{"postgres_changes": []any{}}}
			b, _ := json.Marshal(rep)
			_ = f.write(conn, phxMessage{Topic: m.Topic, Event: "phx_reply", Payload: b, Ref: m.Ref})
			f.joinC <- m
		case "heartbeat":
			b, _ := json.Marshal(map[string]any{"status": "ok", "response": map[string]any{}})
			_ = f.write(conn, phxMessage{Topic: "phoenix", Event: "phx_reply", Payload: b, Ref: m.Ref})
		}
	}
}

func (f *wsFake) write(conn *websocket.Conn, m phxMessage) error {
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

func (f *wsFake) waitJoin(topic string) phxMessage {
	f.t.Helper()
	// Joins for other topics are kept, not dropped: rejoin order after a
	// reconnect is not deterministic.
	for i, m := range f.pendingJoins {
		if m.Topic == topic {
			f.pendingJoins = append(f.pendingJoins[:i], f.pendingJoins[i+1:]...)
			return m
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-f.joinC:
			if m.Topic == topic {
				return m
			}
			f.pendingJoins = append(f.pendingJoins, m)
		case <-deadline:
			f.t.Fatalf("no join on %s", topic)
		}
	}
}

func (f *wsFake) sentEvents(topic, event string) []phxMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []phxMessage
	for _, m := range f.sent {
		if m.Topic == topic && m.Event == event {
			out = append(out, m)
		}
	}
	return out
}

func waitEvent(t *testing.T, ch <-chan rendezvous.Event, kind rendezvous.EventKind) rendezvous.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("events closed while waiting for %s", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", kind)
		}
	}
}

func setupRealtime(t *testing.T) (*fake, *wsFake, *Client) {
	f := &fake{t: t}
	ws := newWSFake(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/realtime/v1/websocket", ws.handler)
	mux.HandleFunc("/auth/v1/verify", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, sessionJSON("access-1", "refresh-1", 3600))
	})
	mux.HandleFunc("/auth/v1/token", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, sessionJSON("access-2", "refresh-2", 3600))
	})
	mux.HandleFunc("/rest/v1/signals", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("/rest/v1/rpc/send_signal", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, 1)
	})
	mux.HandleFunc("/rest/v1/rpc/me_detail", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, userRowJSON)
	})
	mux.HandleFunc("/rest/v1/rpc/touch_last_seen", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("/rest/v1/instance_settings", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, []any{map[string]any{"key": "maintenance", "value": true}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		pgErr(w, 404, "PGRST", "unhandled "+r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New(Config{URL: srv.URL, AnonKey: anon})
	c.rt.heartbeat = 50 * time.Millisecond
	c.rt.backoffMin = 20 * time.Millisecond
	c.rt.backoffMax = 100 * time.Millisecond
	login(t, c)
	return f, ws, c
}

func TestRealtimeEvents(t *testing.T) {
	f, ws, c := setupRealtime(t)
	ctx := context.Background()
	defer c.Close()

	if _, err := New(Config{URL: "http://x", AnonKey: anon}).Connect(ctx); !errors.Is(err, rendezvous.ErrUnauthorized) {
		t.Fatalf("connect without session: %v", err)
	}

	events, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	join := ws.waitJoin("realtime:db")
	var jp struct {
		AccessToken string `json:"access_token"`
		Config      struct {
			PostgresChanges []map[string]string `json:"postgres_changes"`
		} `json:"config"`
	}
	if err := json.Unmarshal(join.Payload, &jp); err != nil {
		t.Fatal(err)
	}
	if jp.AccessToken != "access-1" {
		t.Fatalf("join without token: %s", join.Payload)
	}
	tables := map[string]string{}
	for _, pc := range jp.Config.PostgresChanges {
		tables[pc["table"]] = pc["filter"]
		if pc["event"] != "*" || pc["schema"] != "public" {
			t.Errorf("bad change config: %v", pc)
		}
	}
	for _, tbl := range []string{"messages", "friendships", "groups", "group_members", "grants", "instance_settings", "signals", "users"} {
		if _, ok := tables[tbl]; !ok {
			t.Errorf("table %s not subscribed", tbl)
		}
	}
	if tables["signals"] != "to_user=eq."+meID || tables["users"] != "id=eq."+meID {
		t.Errorf("filters: %v", tables)
	}

	conn := <-ws.open
	// heartbeat
	deadline := time.Now().Add(2 * time.Second)
	for len(ws.sentEvents("phoenix", "heartbeat")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// INSERT on messages
	rec := map[string]any{"id": "m1", "conv_type": "dm", "conv_id": "a:b", "sender": themID,
		"ciphertext": `\xcafe`, "nonce": `\x00`, "key_epoch": 1, "created_at": "2026-01-02T03:04:05.123456"}
	pl, _ := json.Marshal(map[string]any{"data": map[string]any{"type": "INSERT", "table": "messages", "schema": "public", "record": rec, "old_record": nil}, "ids": []int{1}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:db", Event: "postgres_changes", Payload: pl})
	ev := waitEvent(t, events, rendezvous.EvMessage)
	if ev.Deleted || ev.Message == nil || ev.Message.ID != "m1" || string(ev.Message.Ciphertext) != "\xca\xfe" || ev.Message.CreatedAt.Year() != 2026 {
		t.Fatalf("message event: %+v", ev)
	}

	// DELETE on groups: only old_record present.
	pl, _ = json.Marshal(map[string]any{"data": map[string]any{"type": "DELETE", "table": "groups", "schema": "public", "record": nil, "old_record": map[string]any{"id": "g1"}}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:db", Event: "postgres_changes", Payload: pl})
	ev = waitEvent(t, events, rendezvous.EvGroup)
	if !ev.Deleted || ev.Group == nil || ev.Group.ID != "g1" {
		t.Fatalf("group delete: %+v", ev)
	}

	// signals: delivered then deleted server side
	pl, _ = json.Marshal(map[string]any{"data": map[string]any{"type": "INSERT", "table": "signals", "schema": "public",
		"record": map[string]any{"id": 77, "session_id": "s1", "from_user": themID, "to_user": meID, "type": "offer", "payload": `\x0102`}}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:db", Event: "postgres_changes", Payload: pl})
	ev = waitEvent(t, events, rendezvous.EvSignal)
	if ev.Signal == nil || ev.Signal.From != themID || ev.Signal.Type != "offer" || string(ev.Signal.Payload) != "\x01\x02" {
		t.Fatalf("signal: %+v", ev)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		var del *recorded
		for i := range f.reqs {
			if f.reqs[i].Path == "/rest/v1/signals" && f.reqs[i].Method == http.MethodDelete {
				del = &f.reqs[i]
			}
		}
		f.mu.Unlock()
		if del != nil {
			if del.Query != "id=eq.77" {
				t.Fatalf("signal delete query: %s", del.Query)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("signal row not deleted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// settings change triggers a full fetch
	pl, _ = json.Marshal(map[string]any{"data": map[string]any{"type": "UPDATE", "table": "instance_settings", "schema": "public", "record": map[string]any{"key": "maintenance", "value": true}}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:db", Event: "postgres_changes", Payload: pl})
	ev = waitEvent(t, events, rendezvous.EvSettings)
	if ev.Settings == nil || !ev.Settings.Maintenance {
		t.Fatalf("settings: %+v", ev)
	}

	// presence
	if err := c.WatchPresence(ctx, []string{themID}); err != nil {
		t.Fatal(err)
	}
	pj := ws.waitJoin("realtime:user:" + themID)
	var pc struct {
		Config struct {
			Private  bool              `json:"private"`
			Presence map[string]string `json:"presence"`
		} `json:"config"`
	}
	_ = json.Unmarshal(pj.Payload, &pc)
	if !pc.Config.Private || pc.Config.Presence["key"] != meID {
		t.Fatalf("presence join config: %s", pj.Payload)
	}
	pl, _ = json.Marshal(map[string]any{"joins": map[string]any{themID: map[string]any{"metas": []any{map[string]any{"phx_ref": "a", "online_at": "x"}}}}, "leaves": map[string]any{}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:user:" + themID, Event: "presence_diff", Payload: pl})
	ev = waitEvent(t, events, rendezvous.EvPresence)
	if ev.UserID != themID || !ev.Online {
		t.Fatalf("presence join: %+v", ev)
	}
	pl, _ = json.Marshal(map[string]any{"joins": map[string]any{}, "leaves": map[string]any{themID: map[string]any{"metas": []any{map[string]any{"phx_ref": "a"}}}}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:user:" + themID, Event: "presence_diff", Payload: pl})
	ev = waitEvent(t, events, rendezvous.EvPresence)
	if ev.UserID != themID || ev.Online {
		t.Fatalf("presence leave: %+v", ev)
	}
	pl, _ = json.Marshal(map[string]any{themID: map[string]any{"metas": []any{map[string]any{"phx_ref": "b"}}}})
	_ = ws.write(conn, phxMessage{Topic: "realtime:user:" + themID, Event: "presence_state", Payload: pl})
	if ev = waitEvent(t, events, rendezvous.EvPresence); !ev.Online {
		t.Fatalf("presence state: %+v", ev)
	}

	// SetOnline joins my own channel then tracks after the reply.
	if err := c.SetOnline(ctx, true); err != nil {
		t.Fatal(err)
	}
	ws.waitJoin("realtime:user:" + meID)
	deadline = time.Now().Add(2 * time.Second)
	for len(ws.sentEvents("realtime:user:"+meID, "presence")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no presence track")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr := ws.sentEvents("realtime:user:"+meID, "presence")[0]
	if !strings.Contains(string(tr.Payload), `"type":"track"`) || !strings.Contains(string(tr.Payload), "online_at") {
		t.Fatalf("track payload: %s", tr.Payload)
	}
	if err := c.SetOnline(ctx, false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(ws.sentEvents("realtime:user:"+meID, "presence")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no presence untrack")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := ws.sentEvents("realtime:user:"+meID, "presence")[1]; !strings.Contains(string(p.Payload), `"type":"untrack"`) {
		t.Fatalf("untrack payload: %s", p.Payload)
	}

	// token refresh pushes access_token to joined channels
	if _, err := c.Restore(ctx, "refresh-1"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(ws.sentEvents("realtime:db", "access_token")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no access_token push")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p := ws.sentEvents("realtime:db", "access_token")[0]; !strings.Contains(string(p.Payload), "access-2") {
		t.Fatalf("access_token payload: %s", p.Payload)
	}

	// drop the socket: expect disconnected, reconnected and rejoins
	conn.Close(websocket.StatusGoingAway, "bye")
	waitEvent(t, events, rendezvous.EvDisconnected)
	waitEvent(t, events, rendezvous.EvReconnected)
	ws.waitJoin("realtime:db")
	ws.waitJoin("realtime:user:" + themID)
	ws.waitJoin("realtime:user:" + meID)
	<-ws.open

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	closed := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-closed:
			t.Fatal("events not closed")
		}
	}
}

func TestRealtimeNotConnected(t *testing.T) {
	_, _, c := setupRealtime(t)
	if err := c.WatchPresence(context.Background(), []string{themID}); !errors.Is(err, rendezvous.ErrNotConnected) {
		t.Fatalf("watch before connect: %v", err)
	}
	if err := c.SetOnline(context.Background(), true); !errors.Is(err, rendezvous.ErrNotConnected) {
		t.Fatalf("online before connect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
