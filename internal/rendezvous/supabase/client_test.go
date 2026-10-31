package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

const (
	anon   = "anon-key"
	meID   = "11111111-1111-1111-1111-111111111111"
	themID = "22222222-2222-2222-2222-222222222222"
)

// fake is a minimal GoTrue + PostgREST stand-in that records requests.
type fake struct {
	t  *testing.T
	mu sync.Mutex
	// last seen request details, keyed by path
	reqs []recorded
	// tokens issued by refresh, counted
	refreshes int
}

type recorded struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

func (f *fake) record(r *http.Request) recorded {
	b, _ := io.ReadAll(r.Body)
	rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(b)}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()
	return rec
}

func (f *fake) last(path string) recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].Path == path {
			return f.reqs[i]
		}
	}
	f.t.Fatalf("no request to %s", path)
	return recorded{}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func pgErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"code": code, "message": msg, "details": nil, "hint": nil})
}

func sessionJSON(access, refresh string, expiresIn int64) map[string]any {
	return map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "bearer",
		"expires_in": expiresIn, "expires_at": time.Now().Add(time.Duration(expiresIn) * time.Second).Unix(),
		"user": map[string]any{"id": meID, "email": "me@example.com"},
	}
}

var userRowJSON = map[string]any{
	"id": meID, "username": "sujay", "pubkey": "PK", "role": "admin", "banned": false, "muted": false,
	"approved": true, "created_at": "2026-01-02T03:04:05.123456+00:00", "last_seen": "2026-01-02T03:04:05+00:00",
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	f := &fake{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/v1/otp", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		if !strings.Contains(rec.Body, `"create_user":true`) {
			pgErr(w, 400, "", "missing create_user")
			return
		}
		writeJSON(w, 200, map[string]any{})
	})
	mux.HandleFunc("/auth/v1/verify", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		var b map[string]string
		_ = json.Unmarshal([]byte(rec.Body), &b)
		if b["type"] != "email" || b["token"] != "123456" {
			writeJSON(w, 401, map[string]any{"error": "invalid_grant", "error_description": "bad code"})
			return
		}
		writeJSON(w, 200, sessionJSON("access-1", "refresh-1", 3600))
	})
	mux.HandleFunc("/auth/v1/token", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		if r.URL.Query().Get("grant_type") != "refresh_token" {
			pgErr(w, 400, "", "bad grant")
			return
		}
		f.mu.Lock()
		f.refreshes++
		n := f.refreshes
		f.mu.Unlock()
		if strings.Contains(rec.Body, "dead") {
			writeJSON(w, 401, map[string]any{"error": "invalid_grant"})
			return
		}
		exp := int64(3600)
		if strings.Contains(rec.Body, "short") {
			exp = 30
		}
		writeJSON(w, 200, sessionJSON("access-r"+itoa(n), "refresh-r"+itoa(n), exp))
	})
	mux.HandleFunc("/auth/v1/logout", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("/rest/v1/users", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		if rec.Header.Get("apikey") != anon {
			writeJSON(w, 401, map[string]any{"message": "No API key found in request"})
			return
		}
		if rec.Header.Get("Authorization") == "" {
			writeJSON(w, 401, map[string]any{"message": "JWT required"})
			return
		}
		qv := r.URL.Query()
		switch {
		case r.Method == http.MethodPatch:
			writeJSON(w, 200, []any{userRowJSON})
		case qv.Get("id") == "eq."+meID, qv.Get("username") == "eq.sujay":
			writeJSON(w, 200, []any{userRowJSON})
		case strings.HasPrefix(qv.Get("id"), "in.("):
			writeJSON(w, 200, []any{userRowJSON, userRowJSON})
		case qv.Get("order") == "username.asc":
			if rec.Header.Get("x-pairdesk-admin-token") == "" {
				writeJSON(w, 200, []any{userRowJSON})
				return
			}
			writeJSON(w, 200, []any{userRowJSON, userRowJSON, userRowJSON})
		default:
			writeJSON(w, 200, []any{})
		}
	})
	mux.HandleFunc("/rest/v1/rpc/register_username", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		var b map[string]any
		_ = json.Unmarshal([]byte(rec.Body), &b)
		switch b["name"] {
		case "taken":
			pgErr(w, 409, "23505", `duplicate key value violates unique constraint "users_username_key"`)
		case "closed":
			pgErr(w, 400, "P0001", "registration closed")
		case "badinv":
			pgErr(w, 400, "P0002", "invalid invite")
		case "maint":
			pgErr(w, 400, "P0003", "maintenance")
		case "rate":
			pgErr(w, 400, "P0004", "rate limited")
		case "forb":
			pgErr(w, 403, "42501", "forbidden")
		case "again":
			pgErr(w, 409, "23505", "already registered")
		default:
			row := map[string]any{}
			for k, v := range userRowJSON {
				row[k] = v
			}
			row["username"] = b["name"]
			row["pubkey"] = b["pub"]
			writeJSON(w, 200, row)
		}
	})
	mux.HandleFunc("/rest/v1/rpc/check_username", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		writeJSON(w, 200, !strings.Contains(rec.Body, "taken"))
	})
	mux.HandleFunc("/rest/v1/rpc/send_message", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		var b map[string]any
		_ = json.Unmarshal([]byte(rec.Body), &b)
		writeJSON(w, 200, map[string]any{
			"id": "m1", "conv_type": b["ct"], "conv_id": b["conv"], "sender": meID,
			"ciphertext": b["cipher"], "nonce": b["n"], "key_epoch": b["epoch"],
			"created_at": "2026-01-02T03:04:05.5+00:00",
		})
	})
	mux.HandleFunc("/rest/v1/rpc/send_friend_request", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("/rest/v1/rpc/admin_bootstrap_device", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, "devtoken")
	})
	mux.HandleFunc("/rest/v1/rpc/admin_stats", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		if rec.Header.Get("x-pairdesk-admin-token") == "" {
			writeJSON(w, 200, nil)
			return
		}
		writeJSON(w, 200, map[string]any{"users_total": 3, "msgs_today": 7, "db_bytes": 12345})
	})
	mux.HandleFunc("/rest/v1/friendships", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, []any{map[string]any{
				"requester": themID, "addressee": meID, "status": "pending",
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}})
		case http.MethodPatch:
			if rec.Header.Get("Prefer") != "return=representation" {
				pgErr(w, 400, "", "missing prefer")
				return
			}
			if r.URL.Query().Get("requester") != "eq."+themID || r.URL.Query().Get("addressee") != "eq."+meID {
				writeJSON(w, 200, []any{})
				return
			}
			var b map[string]any
			_ = json.Unmarshal([]byte(rec.Body), &b)
			writeJSON(w, 200, []any{map[string]any{
				"requester": themID, "addressee": meID, "status": b["status"],
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}})
		case http.MethodDelete:
			w.WriteHeader(204)
		}
	})
	mux.HandleFunc("/rest/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, []any{
			map[string]any{"id": "m2", "conv_type": "dm", "conv_id": "a:b", "sender": meID,
				"ciphertext": `\x02`, "nonce": `\x00`, "key_epoch": 1, "created_at": "2026-01-02T00:00:02Z"},
			map[string]any{"id": "m1", "conv_type": "dm", "conv_id": "a:b", "sender": meID,
				"ciphertext": `\x01`, "nonce": `\x00`, "key_epoch": 1, "created_at": "2026-01-02T00:00:01Z"},
		})
	})
	mux.HandleFunc("/rest/v1/instance_settings", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, []any{
			map[string]any{"key": "registration", "value": "invite"},
			map[string]any{"key": "require_approval", "value": true},
			map[string]any{"key": "user_cap", "value": 42},
			map[string]any{"key": "min_version", "value": "1.2.3"},
			map[string]any{"key": "maintenance", "value": false},
			map[string]any{"key": "maintenance_msg", "value": ""},
			map[string]any{"key": "announcement", "value": "hi"},
			map[string]any{"key": "announcement_id", "value": "a1"},
			map[string]any{"key": "msgs_per_min", "value": 30},
			map[string]any{"key": "admin_pubkey", "value": "APK"},
		})
	})
	mux.HandleFunc("/rest/v1/grants", func(w http.ResponseWriter, r *http.Request) {
		rec := f.record(r)
		if r.Method == http.MethodPatch {
			if !strings.Contains(rec.Body, "revoked_at") || r.URL.Query().Get("id") != "eq.g1" {
				writeJSON(w, 200, []any{})
				return
			}
		}
		writeJSON(w, 200, []any{map[string]any{
			"id": "g1", "grantor": meID, "grantee": themID, "scope": "watch", "sudo": "ask",
			"grantee_pubkey": "GPK", "signature": `\xdeadbeef`, "created_at": "2026-01-01T00:00:00Z",
			"revoked_at": nil}})
	})
	mux.HandleFunc("/rest/v1/signals", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(204)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		pgErr(w, 404, "PGRST", "unhandled "+r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func itoa(n int) string { return string(rune('0' + n)) }

func login(t *testing.T, c *Client) rendezvous.Session {
	t.Helper()
	s, err := c.VerifyOTP(context.Background(), "me@example.com", "123456")
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	return s
}

func TestAuthFlow(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{URL: srv.URL, AnonKey: anon})
	ctx := context.Background()

	if err := c.RequestMagicLink(ctx, "me@example.com"); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/auth/v1/otp"); r.Header.Get("apikey") != anon || !strings.Contains(r.Body, "me@example.com") {
		t.Fatalf("otp request wrong: %+v", r)
	}
	if _, err := c.VerifyOTP(ctx, "me@example.com", "000000"); !errors.Is(err, rendezvous.ErrUnauthorized) {
		t.Fatalf("bad code: want ErrUnauthorized, got %v", err)
	}
	if c.Session() != nil {
		t.Fatal("session set after failed verify")
	}
	s := login(t, c)
	if s.UserID != meID || s.AccessToken != "access-1" || s.RefreshToken != "refresh-1" || s.Email != "me@example.com" {
		t.Fatalf("session: %+v", s)
	}
	if s.ExpiresAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("expires_at not kept: %v", s.ExpiresAt)
	}
	if got := c.Session(); got == nil || got.AccessToken != "access-1" {
		t.Fatalf("Session(): %+v", got)
	}

	// Restore with a fresh refresh token.
	c2 := New(Config{URL: srv.URL, AnonKey: anon})
	s2, err := c2.Restore(ctx, "refresh-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s2.AccessToken, "access-r") || s2.UserID != meID {
		t.Fatalf("restore: %+v", s2)
	}
	if _, err := c2.Restore(ctx, "dead"); !errors.Is(err, rendezvous.ErrUnauthorized) {
		t.Fatalf("dead refresh: %v", err)
	}

	if err := c.SignOut(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/auth/v1/logout"); r.Header.Get("Authorization") != "Bearer access-1" {
		t.Fatalf("logout bearer: %q", r.Header.Get("Authorization"))
	}
	if c.Session() != nil {
		t.Fatal("session not cleared")
	}
	if _, err := c.Me(ctx); !errors.Is(err, rendezvous.ErrUnauthorized) {
		t.Fatalf("Me after signout: %v", err)
	}
}

func TestAutoRefresh(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{URL: srv.URL, AnonKey: anon})
	ctx := context.Background()
	// "short" yields a 30s token: inside the 60s refresh window.
	if _, err := c.Restore(ctx, "short"); err != nil {
		t.Fatal(err)
	}
	before := c.Session().AccessToken
	f.mu.Lock()
	n := f.refreshes
	f.mu.Unlock()
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.refreshes != n+1 {
		t.Fatalf("refreshes: %d want %d", f.refreshes, n+1)
	}
	f.mu.Unlock()
	after := c.Session().AccessToken
	if after == before {
		t.Fatal("token not rotated")
	}
	if r := f.last("/rest/v1/users"); r.Header.Get("Authorization") != "Bearer "+after {
		t.Fatalf("users bearer %q want %q", r.Header.Get("Authorization"), after)
	}
	// Now the token is fresh for an hour: no further refresh.
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshes != n+1 {
		t.Fatalf("unexpected extra refresh: %d", f.refreshes)
	}
}

func TestHeaders(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{URL: srv.URL, AnonKey: anon, AdminToken: "adm-1"})
	ctx := context.Background()
	login(t, c)
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	r := f.last("/rest/v1/users")
	if r.Header.Get("apikey") != anon {
		t.Errorf("apikey: %q", r.Header.Get("apikey"))
	}
	if r.Header.Get("Authorization") != "Bearer access-1" {
		t.Errorf("bearer: %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("x-pairdesk-admin-token") != "adm-1" {
		t.Errorf("admin token: %q", r.Header.Get("x-pairdesk-admin-token"))
	}
	if r.Query != "id=eq."+meID+"&limit=1&select=%2A" {
		t.Errorf("query: %q", r.Query)
	}
	c.SetAdminToken("")
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	if h := f.last("/rest/v1/users").Header.Get("x-pairdesk-admin-token"); h != "" {
		t.Errorf("admin token still sent: %q", h)
	}
	// Bootstrap installs the token it gets back.
	tok, err := c.BootstrapAdminDevice(ctx, "dev1", "PK")
	if err != nil || tok != "devtoken" {
		t.Fatalf("bootstrap: %q %v", tok, err)
	}
	st, err := c.Stats(ctx)
	if err != nil || st.UsersTotal != 3 || st.DBBytes != 12345 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if h := f.last("/rest/v1/rpc/admin_stats").Header.Get("x-pairdesk-admin-token"); h != "devtoken" {
		t.Errorf("admin token after bootstrap: %q", h)
	}
	users, err := c.ListUsers(ctx)
	if err != nil || len(users) != 3 {
		t.Fatalf("ListUsers as admin: %d %v", len(users), err)
	}
}

func TestUsersAndRegister(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{URL: srv.URL, AnonKey: anon})
	ctx := context.Background()

	if _, err := c.Register(ctx, "x", "y", ""); !errors.Is(err, rendezvous.ErrUnauthorized) {
		t.Fatalf("register without session: %v", err)
	}
	login(t, c)

	ok, err := c.CheckUsername(ctx, "Sujay")
	if err != nil || !ok {
		t.Fatalf("check: %v %v", ok, err)
	}
	if r := f.last("/rest/v1/rpc/check_username"); r.Body != `{"name":"sujay"}`+"\n" && r.Body != `{"name":"sujay"}` {
		t.Fatalf("check body: %q", r.Body)
	}
	ok, _ = c.CheckUsername(ctx, "taken")
	if ok {
		t.Fatal("taken reported available")
	}

	u, err := c.Register(ctx, "NewUser", "PUB", "inv123")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "newuser" || u.PubKey != "PUB" || u.Role != model.RoleAdmin {
		t.Fatalf("register: %+v", u)
	}
	if r := f.last("/rest/v1/rpc/register_username"); !strings.Contains(r.Body, `"invite":"inv123"`) || !strings.Contains(r.Body, `"pub":"PUB"`) {
		t.Fatalf("register body: %s", r.Body)
	}
	cases := map[string]error{
		"taken":  rendezvous.ErrUsernameTaken,
		"again":  rendezvous.ErrConflict,
		"closed": rendezvous.ErrRegClosed,
		"badinv": rendezvous.ErrBadInvite,
		"maint":  rendezvous.ErrMaintenance,
		"rate":   rendezvous.ErrRateLimited,
		"forb":   rendezvous.ErrForbidden,
	}
	for name, want := range cases {
		_, err := c.Register(ctx, name, "p", "")
		if !errors.Is(err, want) {
			t.Errorf("register %s: got %v want %v", name, err, want)
		}
		if strings.Contains(err.Error(), "access-1") {
			t.Errorf("error leaks token: %v", err)
		}
	}

	me, err := c.Me(ctx)
	if err != nil || me.ID != meID || me.Username != "sujay" {
		t.Fatalf("me: %+v %v", me, err)
	}
	if me.CreatedAt.Year() != 2026 || me.CreatedAt.Nanosecond() != 123456000 {
		t.Fatalf("created_at: %v", me.CreatedAt)
	}
	if _, err := c.UserByName(ctx, "SUJAY"); err != nil {
		t.Fatal(err)
	}
	if q := f.last("/rest/v1/users").Query; !strings.Contains(q, "username=eq.sujay") {
		t.Fatalf("by name query: %s", q)
	}
	if _, err := c.UserByID(ctx, "nope"); !errors.Is(err, rendezvous.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	us, err := c.UsersByIDs(ctx, []string{meID, themID})
	if err != nil || len(us) != 2 {
		t.Fatalf("by ids: %d %v", len(us), err)
	}
	if q := f.last("/rest/v1/users").Query; !strings.Contains(q, "id=in.%28"+meID+"%2C"+themID+"%29") {
		t.Fatalf("in query: %s", q)
	}
	if us, _ := c.UsersByIDs(ctx, nil); us != nil {
		t.Fatal("empty ids should not hit the server")
	}
	if err := c.UpdatePubKey(ctx, "NEWPK"); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/rest/v1/users"); r.Method != http.MethodPatch || r.Body != `{"pubkey":"NEWPK"}` || r.Query != "id=eq."+meID {
		t.Fatalf("update pubkey: %+v", r)
	}

	st, err := c.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := model.Settings{Registration: model.RegInvite, RequireApproval: true, UserCap: 42, MinVersion: "1.2.3",
		Announcement: "hi", AnnouncementID: "a1", MsgsPerMin: 30, AdminPubKey: "APK"}
	if st != want {
		t.Fatalf("settings: %+v", st)
	}
	if _, err := c.Stats(ctx); !errors.Is(err, rendezvous.ErrForbidden) {
		t.Fatalf("stats as non-admin: %v", err)
	}
}

func TestFriendsAndMessages(t *testing.T) {
	f, srv := newFake(t)
	c := New(Config{URL: srv.URL, AnonKey: anon})
	ctx := context.Background()
	login(t, c)

	if err := c.SendFriendRequest(ctx, themID); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/rest/v1/rpc/send_friend_request"); r.Body != `{"target":"`+themID+`"}` {
		t.Fatalf("friend request body: %s", r.Body)
	}
	fs, err := c.ListFriendships(ctx)
	if err != nil || len(fs) != 1 || fs[0].Status != model.FriendPending || fs[0].Other(meID) != themID {
		t.Fatalf("friendships: %+v %v", fs, err)
	}
	if err := c.RespondFriendRequest(ctx, themID, true); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/rest/v1/friendships"); r.Method != http.MethodPatch || !strings.Contains(r.Body, `"status":"accepted"`) {
		t.Fatalf("respond: %+v", r)
	}
	if err := c.RespondFriendRequest(ctx, "other", false); !errors.Is(err, rendezvous.ErrNotFound) {
		t.Fatalf("respond missing: %v", err)
	}
	if err := c.RemoveFriend(ctx, themID); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/rest/v1/friendships"); r.Method != http.MethodDelete || !strings.Contains(r.Query, "or=") {
		t.Fatalf("remove: %+v", r)
	}

	m, err := c.SendMessage(ctx, model.Message{ConvType: model.ConvDM, ConvID: "a:b",
		Ciphertext: []byte{0xde, 0xad}, Nonce: bytes.Repeat([]byte{7}, 24), KeyEpoch: 2})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last("/rest/v1/rpc/send_message")
	if !strings.Contains(r.Body, `"cipher":"\\xdead"`) || !strings.Contains(r.Body, `"epoch":2`) || !strings.Contains(r.Body, `"ct":"dm"`) {
		t.Fatalf("send body: %s", r.Body)
	}
	if m.ID != "m1" || !bytes.Equal(m.Ciphertext, []byte{0xde, 0xad}) || len(m.Nonce) != 24 || m.KeyEpoch != 2 {
		t.Fatalf("send result: %+v", m)
	}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ms, err := c.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: "a:b", After: after, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last("/rest/v1/messages").Query
	for _, want := range []string{"conv_type=eq.dm", "conv_id=eq.a%3Ab", "order=created_at.desc", "limit=10", "created_at=gt.2026-01-01T00%3A00%3A00Z"} {
		if !strings.Contains(q, want) {
			t.Errorf("messages query missing %s: %s", want, q)
		}
	}
	if len(ms) != 2 || ms[0].ID != "m1" || ms[1].ID != "m2" || ms[0].Ciphertext[0] != 1 {
		t.Fatalf("messages order: %+v", ms)
	}

	g, err := c.PutGrant(ctx, model.Grant{Grantee: themID, Scope: model.ScopeWatch, Sudo: model.SudoAsk, GranteePubKey: "GPK", Signature: []byte{0xde, 0xad, 0xbe, 0xef}})
	if err != nil || g.ID != "g1" || !bytes.Equal(g.Signature, []byte{0xde, 0xad, 0xbe, 0xef}) || g.RevokedAt != nil {
		t.Fatalf("put grant: %+v %v", g, err)
	}
	if r := f.last("/rest/v1/grants"); !strings.Contains(r.Body, `"signature":"\\xdeadbeef"`) || !strings.Contains(r.Body, `"grantor":"`+meID+`"`) {
		t.Fatalf("grant body: %s", r.Body)
	}
	if err := c.RevokeGrant(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeGrant(ctx, "g9"); !errors.Is(err, rendezvous.ErrNotFound) {
		t.Fatalf("revoke missing: %v", err)
	}
	if err := c.SendSignal(ctx, rendezvous.Signal{SessionID: "s1", To: themID, Type: "offer", Payload: []byte{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if r := f.last("/rest/v1/signals"); !strings.Contains(r.Body, `"from_user":"`+meID+`"`) || !strings.Contains(r.Body, `"payload":"\\x0102"`) {
		t.Fatalf("signal body: %s", r.Body)
	}
}

func TestBytea(t *testing.T) {
	var b bytea
	if err := json.Unmarshal([]byte(`"\\xdeadBEEF"`), &b); err != nil || !bytes.Equal(b, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("decode hex: %x %v", []byte(b), err)
	}
	if err := json.Unmarshal([]byte(`"3q2+7w=="`), &b); err != nil || !bytes.Equal(b, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("decode base64: %x %v", []byte(b), err)
	}
	if err := json.Unmarshal([]byte(`null`), &b); err != nil || b != nil {
		t.Fatalf("decode null: %v %v", b, err)
	}
	if err := json.Unmarshal([]byte(`"\\xzz"`), &b); err == nil {
		t.Fatal("bad hex accepted")
	}
	out, err := json.Marshal(bytea{0x00, 0xff})
	if err != nil || string(out) != `"\\x00ff"` {
		t.Fatalf("encode: %s %v", out, err)
	}
	if encodeBytea(nil) != `\x` {
		t.Fatal("empty encode")
	}
	var tv ts
	for _, s := range []string{`"2026-01-02T03:04:05.123456+00:00"`, `"2026-01-02T03:04:05.123456"`, `"2026-01-02 03:04:05.123456+00"`} {
		if err := json.Unmarshal([]byte(s), &tv); err != nil || tv.Nanosecond() != 123456000 {
			t.Errorf("ts %s: %v %v", s, tv, err)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"message":"JWT expired"}`, rendezvous.ErrUnauthorized},
		{403, `{"code":"42501","message":"permission denied"}`, rendezvous.ErrForbidden},
		{409, `{"code":"23505","message":"dup"}`, rendezvous.ErrConflict},
		{400, `{"code":"P0001","message":"closed"}`, rendezvous.ErrRegClosed},
		{400, `{"code":"P0002","message":"inv"}`, rendezvous.ErrBadInvite},
		{400, `{"code":"P0003","message":"m"}`, rendezvous.ErrMaintenance},
		{400, `{"code":"P0004","message":"r"}`, rendezvous.ErrRateLimited},
		{406, `{"code":"PGRST116","message":"0 rows"}`, rendezvous.ErrNotFound},
		{404, `not json`, rendezvous.ErrNotFound},
		{429, `{"error_code":"over_request_rate_limit","msg":"slow"}`, rendezvous.ErrRateLimited},
	}
	for _, tc := range cases {
		err := parseError(tc.status, []byte(tc.body))
		if !errors.Is(err, tc.want) {
			t.Errorf("%d %s: got %v want %v", tc.status, tc.body, err, tc.want)
		}
	}
	err := parseError(500, []byte(`{"code":"XX000","message":"boom"}`))
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "XX000" || ae.Message != "boom" {
		t.Fatalf("unmapped: %v", err)
	}
}

func TestSettingsFlatten(t *testing.T) {
	s, err := settingsFromRows([]struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}{{"registration", json.RawMessage(`"closed"`)}, {"maintenance", json.RawMessage(`true`)}, {"unknown", json.RawMessage(`1`)}})
	if err != nil || s.Registration != model.RegClosed || !s.Maintenance {
		t.Fatalf("%+v %v", s, err)
	}
}
