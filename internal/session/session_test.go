package session

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous/memory"
	"github.com/Kumarsujay03/pairdesk/internal/sessionlog"
	"github.com/Kumarsujay03/pairdesk/internal/store"
	"github.com/Kumarsujay03/pairdesk/internal/term"
)

type node struct {
	app *app.App
	mgr *Manager
	ev  chan app.Event
	// stdin for the terminal handoff
	in  *io.PipeWriter
	out *bytes.Buffer
	omu sync.Mutex
}

func boot(t *testing.T, ms *memory.Store, email, name string) *node {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Paths{Config: dir + "/c", State: dir + "/s", Cache: dir + "/k"})
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(app.Options{Backend: ms.Client(), Store: st})
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.RequestLogin(ctx, email); err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyOTP(ctx, memory.TestOTP); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(ctx, name, ""); err != nil {
		t.Fatal(err)
	}
	n := &node{app: a, out: &bytes.Buffer{}}
	n.mgr = New(a, nil, nil)
	n.mgr.STUN = nil
	pr, pw := io.Pipe()
	n.in = pw
	n.mgr.TermUI = func(fn func(stdin io.Reader, stdout io.Writer) error) error {
		return fn(pr, writerFunc(func(b []byte) { n.omu.Lock(); n.out.Write(b); n.omu.Unlock() }))
	}
	// Fan events out so tests can wait on them.
	n.ev = make(chan app.Event, 256)
	go func() {
		for e := range a.Events() {
			select {
			case n.ev <- e:
			default:
			}
		}
	}()
	return n
}

func (n *node) output() string { n.omu.Lock(); defer n.omu.Unlock(); return n.out.String() }

func (n *node) wait(t *testing.T, kind app.EventKind, pred func(app.Event) bool, d time.Duration) app.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-n.ev:
			if e.Kind == kind && (pred == nil || pred(e)) {
				return e
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", kind)
		}
	}
}

func befriend(t *testing.T, a, b *node) {
	t.Helper()
	ctx := context.Background()
	if err := a.app.AddFriend(ctx, b.app.Me().Username); err != nil {
		t.Fatal(err)
	}
	b.wait(t, app.EvFriendRequest, nil, 3*time.Second)
	if err := b.app.RespondFriend(ctx, a.app.Me().Username, true); err != nil {
		t.Fatal(err)
	}
	a.wait(t, app.EvFriendAccepted, nil, 3*time.Second)
	b.wait(t, app.EvFriendAccepted, nil, 3*time.Second)
	// presence
	time.Sleep(100 * time.Millisecond)
}

func TestBoardUnderGrantWithLogs(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash required")
	}
	if !term.IsolationAvailable() {
		t.Skip("no sandbox available on this machine (bwrap or user namespaces)")
	}
	ms := memory.New()
	adminN := boot(t, ms, "a@x", "sujay03")
	user := boot(t, ms, "b@x", "arjun")
	befriend(t, adminN, user)
	ctx := context.Background()

	// A plain user cannot request.
	if err := user.mgr.Request(ctx, "sujay03", model.SessionBoard, false); err != ErrNotAllowed {
		t.Fatalf("user request: %v", err)
	}
	// Without a grant, the host sees a card; deny it.
	go func() {
		e := user.wait(t, app.EvCard, func(e app.Event) bool { return e.Card.Kind == "access" }, 10*time.Second)
		e.Card.Reply(false, model.SudoBlocked)
	}()
	if err := adminN.mgr.Request(ctx, "arjun", model.SessionBoard, false); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denial, got %v", err)
	}

	// Grant standing board access (sudo blocked), then board: no card.
	if err := user.app.Grant(ctx, "sujay03", model.ScopeBoard, model.SudoBlocked, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- adminN.mgr.Request(ctx, "arjun", model.SessionBoard, false) }()
	// Host sees the session go live.
	user.wait(t, app.EvSession, func(e app.Event) bool { return e.Session.State == "live" && e.Session.Via == model.ViaGrant }, 20*time.Second)
	sudoAsked := make(chan struct{})
	go func() {
		e := user.wait(t, app.EvCard, func(e app.Event) bool { return e.Card.Kind == "sudo" }, 15*time.Second)
		if e.Card.ReplyWithPassword == nil || !strings.Contains(e.Card.Command, "id") {
			t.Errorf("sudo card malformed: %+v", e.Card)
		}
		e.Card.ReplyWithPassword(false, "")
		close(sudoAsked)
	}()
	go func() {
		time.Sleep(700 * time.Millisecond) // let the shell print its prompt
		adminN.in.Write([]byte("echo pairdesk-ok\n"))
		time.Sleep(700 * time.Millisecond)
		adminN.in.Write([]byte("sudo id\n"))
		select {
		case <-sudoAsked:
		case <-time.After(15 * time.Second):
		}
		time.Sleep(700 * time.Millisecond)
		adminN.in.Write([]byte{0x1d, 'q'}) // detach
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request: %v (output %q)", err, adminN.output())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("session did not end")
	}
	ended := user.wait(t, app.EvSession, func(e app.Event) bool { return e.Session.State == "ended" }, 10*time.Second)
	t.Logf("host end reason: %q", ended.Session.Error)
	if !strings.Contains(adminN.output(), "pairdesk-ok") {
		t.Fatalf("visitor output missing echo: %q", adminN.output())
	}
	if !strings.Contains(strings.ToLower(adminN.output()), "consent") && !strings.Contains(strings.ToLower(adminN.output()), "denied") {
		t.Fatalf("sudo should have been blocked; output: %q", adminN.output())
	}
	// Both sides hold a sealed, verifiable log with the command.
	for _, n := range []*node{user, adminN} {
		entries, err := n.mgr.Logs()
		if err != nil || len(entries) == 0 {
			t.Fatalf("%s: logs %v %d", n.app.Me().Username, err, len(entries))
		}
		e := entries[0]
		r, err := sessionlog.Verify(e.Dir, n.app.Identity().Public())
		if err != nil || !r.ChainOK || !r.SignatureOK || !r.TranscriptOK {
			t.Fatalf("%s: verify %+v %v", n.app.Me().Username, r, err)
		}
		cmds, _ := sessionlog.ReadCommands(e.Dir)
		var sawEcho, sawSudo bool
		for _, c := range cmds {
			if strings.Contains(c.Cmd, "echo pairdesk-ok") {
				sawEcho = true
				out, _ := sessionlog.CommandOutput(e.Dir, c)
				if n.app.Me().Username == "arjun" && !strings.Contains(out, "pairdesk-ok") {
					t.Fatalf("host log output for echo: %q", out)
				}
			}
			if c.Sudo {
				sawSudo = true
			}
		}
		if !sawEcho || !sawSudo {
			t.Fatalf("%s: commands %+v", n.app.Me().Username, cmds)
		}
		st, _ := os.Stat(filepath.Join(e.Dir, "commands.jsonl"))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("log perms %o", st.Mode().Perm())
		}
	}
}

func TestScreenNeverAutoAcceptsAndRevokeEnds(t *testing.T) {
	ms := memory.New()
	adminN := boot(t, ms, "a@x", "sujay03")
	user := boot(t, ms, "b@x", "arjun")
	befriend(t, adminN, user)
	ctx := context.Background()
	if err := user.app.Grant(ctx, "sujay03", model.ScopeBoard, model.SudoAuto, "pw"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	// Screen request still produces a card even with a board grant.
	go func() {
		e := user.wait(t, app.EvCard, func(e app.Event) bool { return e.Card.Kind == "access" && e.Card.Scope == model.SessionScreen }, 10*time.Second)
		e.Card.Reply(false, model.SudoBlocked)
	}()
	if err := adminN.mgr.Request(ctx, "arjun", model.SessionScreen, true); err == nil {
		t.Fatal("screen auto-accepted")
	}
	// Revoke ends a live grant session.
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("bash required")
	}
	if !term.IsolationAvailable() {
		t.Skip("no sandbox available on this machine")
	}
	done := make(chan error, 1)
	go func() { done <- adminN.mgr.Request(ctx, "arjun", model.SessionWatch, false) }()
	user.wait(t, app.EvSession, func(e app.Event) bool { return e.Session.State == "live" }, 20*time.Second)
	if err := user.app.Revoke(ctx, "sujay03", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("revoke did not end the session")
	}
	// The host tears down after the visitor returns; give it a moment.
	deadline := time.Now().Add(5 * time.Second)
	for len(user.mgr.Live()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("session still live after revoke")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFileDropBetweenFriends(t *testing.T) {
	ms := memory.New()
	_ = boot(t, ms, "a@x", "sujay03")
	p := boot(t, ms, "b@x", "priya")
	r := boot(t, ms, "c@x", "rahul")
	befriend(t, p, r)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "big.bin")
	data := bytes.Repeat([]byte("pairdesk"), 100_000) // 800 KB, several chunks
	_ = os.WriteFile(src, data, 0o600)
	home := t.TempDir()
	t.Setenv("HOME", home)
	go func() {
		e := r.wait(t, app.EvCard, func(e app.Event) bool { return e.Card.Kind == "file" }, 10*time.Second)
		e.Card.Reply(true, model.SudoBlocked)
	}()
	if err := p.mgr.Send(ctx, "rahul", src, nil); err != nil {
		t.Fatal(err)
	}
	r.wait(t, app.EvSession, func(e app.Event) bool { return e.Session.State == "ended" && strings.HasPrefix(e.Session.Error, "saved") }, 20*time.Second)
	got, err := os.ReadFile(filepath.Join(home, "Downloads", "pairdesk", "big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received file mismatch: %v", err)
	}
}
