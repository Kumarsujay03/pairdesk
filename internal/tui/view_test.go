package tui

import (
	"github.com/Kumarsujay03/pairdesk/internal/admin"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous/memory"
	"github.com/Kumarsujay03/pairdesk/internal/store"
)

// newTestApp registers a user against a shared in-memory backend and
// returns a ready app. The first user registered becomes the admin.
func newTestApp(t *testing.T, mem *memory.Store, name string) *app.App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Paths{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state"), Cache: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	p := st.Prefs()
	p.ReducedMotion = true
	if err := st.SavePrefs(p); err != nil {
		t.Fatal(err)
	}
	a := app.New(app.Options{Backend: mem.Client(), Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Phase() != app.PhaseNeedLogin {
		t.Fatalf("phase after Start = %v, want NeedLogin", a.Phase())
	}
	if err := a.RequestLogin(ctx, name+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyOTP(ctx, memory.TestOTP); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(ctx, name, ""); err != nil {
		t.Fatal(err)
	}
	if a.Phase() != app.PhaseReady {
		t.Fatalf("phase after Register = %v, want Ready", a.Phase())
	}
	t.Cleanup(a.Close)
	return a
}

// newTestUI builds a model at 100x30 and feeds it every event queued so far.
func newTestUI(t *testing.T, a *app.App) *ui {
	t.Helper()
	m := newModel(context.Background(), a, Options{Version: "test"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drain(m, a)
	return m
}

// drain feeds queued app events into the model without blocking.
func drain(m *ui, a *app.App) {
	for {
		select {
		case e := <-a.Events():
			m.Update(evMsg{e})
		default:
			return
		}
	}
}

// waitFor pumps events until the view contains want or the deadline passes.
func waitFor(t *testing.T, m *ui, a *app.App, want string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if v := m.View(); strings.Contains(v, want) {
			return v
		}
		select {
		case e := <-a.Events():
			m.Update(evMsg{e})
		case <-deadline:
			t.Fatalf("view never contained %q:\n%s", want, m.View())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func keys(m *ui, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func press(m *ui, k tea.KeyType) { m.Update(tea.KeyMsg{Type: k}) }

func TestLoginScreenRenders(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(store.Paths{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state"), Cache: filepath.Join(dir, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(app.Options{Backend: memory.New().Client(), Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), a, Options{Version: "test", Demo: true, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drain(m, a)
	v := m.View()
	for _, want := range []string{bannerLines[0], "log in", "000000", "device key phrase"} {
		if !strings.Contains(v, want) {
			t.Errorf("login view lacks %q:\n%s", want, v)
		}
	}
	// Too-small terminals get a clear message instead of a broken layout.
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if !strings.Contains(m.View(), "at least 80x24") {
		t.Errorf("small terminal message missing")
	}
}

func TestMainViewWithEvents(t *testing.T) {
	mem := memory.New()
	admin := newTestApp(t, mem, "sujay03")
	arjun := newTestApp(t, mem, "arjun")
	m := newTestUI(t, admin)

	v := m.View()
	for _, want := range []string{"friends", "groups", "tab panes", "ctrl+c quit", "/help", "sujay03", "nobody yet"} {
		if !strings.Contains(v, want) {
			t.Errorf("main view lacks %q:\n%s", want, v)
		}
	}

	// Become friends through the real backend and let the events flow.
	ctx := context.Background()
	if err := admin.AddFriend(ctx, "arjun"); err != nil {
		t.Fatal(err)
	}
	if err := arjun.RespondFriend(ctx, "sujay03", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, admin, "arjun is now your friend")
	v = waitFor(t, m, admin, "● arjun")
	if strings.Contains(v, "nobody yet") {
		t.Errorf("friend list still empty:\n%s", v)
	}

	// A message from arjun shows up with the sender name and text.
	dm, err := arjun.OpenDM(ctx, "sujay03")
	if err != nil {
		t.Fatal(err)
	}
	if err := arjun.Send(ctx, dm, "hello from arjun, this is a reasonably long line that should wrap neatly"); err != nil {
		t.Fatal(err)
	}
	v = waitFor(t, m, admin, "hello from arjun")
	if !strings.Contains(v, "dm · arjun") {
		t.Errorf("conversation title missing:\n%s", v)
	}

	// Slash command output lands in the chat; /me renders in italics path.
	keys(m, "/help")
	press(m, tea.KeyEnter)
	if v = m.View(); !strings.Contains(v, "/theme <name>") {
		t.Errorf("/help output missing:\n%s", v)
	}

	// Pinned announcement bar and esc to dismiss.
	m.Update(evMsg{app.Event{Kind: app.EvAnnouncement, Text: "maintenance at midnight"}})
	if v = m.View(); !strings.Contains(v, "maintenance at midnight") {
		t.Errorf("announcement missing:\n%s", v)
	}
	press(m, tea.KeyEsc)
	if v = m.View(); strings.Contains(v, "maintenance at midnight") {
		t.Errorf("announcement not dismissed:\n%s", v)
	}

	// Presence: arjun comes online, dot fills.
	m.Update(evMsg{app.Event{Kind: app.EvPresence, UserID: arjun.Me().ID, Online: true}})
	_ = m.View()

	// Access card with sudo option; "s" answers approve + ask tier.
	replied := make(chan [2]string, 1)
	m.Update(evMsg{app.Event{Kind: app.EvCard, Card: &app.Card{
		ID: "c1", Kind: "access", From: arjun.Me().ID, FromName: "arjun", Scope: model.SessionBoard,
		Text: "arjun wants to board your machine", ExpiresAt: time.Now().Add(30 * time.Second).UnixNano(),
		Reply: func(accept bool, tier model.SudoTier) {
			replied <- [2]string{map[bool]string{true: "accept", false: "deny"}[accept], string(tier)}
		},
	}}})
	v = m.View()
	for _, want := range []string{"access request · board", "[a]pprove", "[s] approve with sudo", "wants to board"} {
		if !strings.Contains(v, want) {
			t.Errorf("card lacks %q:\n%s", want, v)
		}
	}
	keys(m, "s")
	select {
	case r := <-replied:
		if r[0] != "accept" || r[1] != string(model.SudoAsk) {
			t.Errorf("card reply = %v, want accept/ask", r)
		}
	case <-time.After(time.Second):
		t.Fatal("card reply never called")
	}
	if v = m.View(); !strings.Contains(v, "approved") {
		t.Errorf("approve flash missing:\n%s", v)
	}

	// Key-change event synthesises a card.
	m.Update(evMsg{app.Event{Kind: app.EvKeyChanged, UserID: arjun.Me().ID, Text: "arjun"}})
	if v = m.View(); !strings.Contains(v, "device key changed") || !strings.Contains(v, "[a]ccept new key") {
		t.Errorf("keychange card missing:\n%s", v)
	}

	// Sending a message shows it dimmed as pending until the echo arrives.
	press(m, tea.KeyEsc) // nothing to clear; harmless
	keys(m, "hi arjun")
	press(m, tea.KeyEnter)
	if len(m.pending) != 1 {
		t.Errorf("pending = %d, want 1", len(m.pending))
	}
}

func TestNonAdminCommands(t *testing.T) {
	mem := memory.New()
	_ = newTestApp(t, mem, "sujay03") // admin
	user := newTestApp(t, mem, "priya")
	m := newTestUI(t, user)
	keys(m, "/users")
	press(m, tea.KeyEnter)
	if v := m.View(); !strings.Contains(v, "only the admin can do that") {
		t.Errorf("admin gate missing:\n%s", v)
	}
	keys(m, "/wat")
	press(m, tea.KeyEnter)
	if v := m.View(); !strings.Contains(v, "unknown command /wat") {
		t.Errorf("unknown command line missing:\n%s", v)
	}
	keys(m, "/board sujay03")
	press(m, tea.KeyEnter)
	if v := m.View(); !strings.Contains(v, "only the admin can do that") {
		t.Errorf("board gate missing:\n%s", v)
	}
	keys(m, "/logs")
	press(m, tea.KeyEnter)
	if v := m.View(); !strings.Contains(v, "not available in this build") {
		t.Errorf("nil hook message missing:\n%s", v)
	}
	// Tab moves focus to the left pane; the status bar still shows hints.
	press(m, tea.KeyTab)
	if m.pane != paneLeft {
		t.Errorf("tab did not switch panes")
	}
	if v := m.View(); !strings.Contains(v, "tab panes") {
		t.Errorf("status hints missing")
	}
}

func TestSuggestionStripAndTabComplete(t *testing.T) {
	mem := memory.New()
	admin := newTestApp(t, mem, "sujay03")
	user := newTestApp(t, mem, "priya")

	// A user typing "/" sees a strip without admin commands; tab completes.
	m := newTestUI(t, user)
	keys(m, "/ad")
	v := m.View()
	if !strings.Contains(v, "/add <user>") {
		t.Fatalf("suggestion strip missing:\n%s", v)
	}
	if strings.Contains(v, "/admin") {
		t.Fatalf("user shown admin command:\n%s", v)
	}
	press(m, tea.KeyTab)
	if got := m.input.Value(); got != "/add " {
		t.Fatalf("tab completed to %q", got)
	}
	if m.pane != paneChat {
		t.Fatalf("tab switched panes while completing")
	}
	keys(m, "x")
	if len(m.sugs) != 0 {
		t.Fatalf("strip should clear once arguments start: %v", m.sugs)
	}

	// The admin sees /admin subcommands and tab cycles through them.
	am := newTestUI(t, admin)
	keys(am, "/admin re")
	if v := am.View(); !strings.Contains(v, "/admin reg open|closed|invite") {
		t.Fatalf("admin subcommand strip missing:\n%s", v)
	}
	press(am, tea.KeyTab)
	press(am, tea.KeyTab)
	if got := am.input.Value(); got != "/admin reset-key " {
		t.Fatalf("second tab = %q", got)
	}
}

func TestSuggestionKeysKeepInputVisible(t *testing.T) {
	mem := memory.New()
	admin := newTestApp(t, mem, "sujay03")
	m := newTestUI(t, admin)
	keys(m, "/")
	v := m.View()
	if !strings.Contains(v, "> /") {
		t.Fatalf("input line hidden while the strip is open:\n%s", v)
	}
	if h := strings.Count(v, "\n") + 1; h > 30 {
		t.Fatalf("view grew to %d rows", h)
	}
	// down twice then up lands on the second row.
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	press(m, tea.KeyUp)
	if m.sugIdx != 1 {
		t.Fatalf("sugIdx = %d", m.sugIdx)
	}
	// Enter on a command with arguments fills the line and keeps the input.
	keys(m, "ad")
	if got := m.sugs[m.sugIdx].Text; got != "/add " {
		t.Fatalf("expected /add selected, got %q", got)
	}
	press(m, tea.KeyEnter)
	if got := m.input.Value(); got != "/add " {
		t.Fatalf("enter should fill /add, got %q", got)
	}
	// Enter on an argument-less command runs it straight away.
	press(m, tea.KeyEsc)
	keys(m, "/frie")
	press(m, tea.KeyEnter)
	if got := m.input.Value(); got != "" {
		t.Fatalf("input not cleared after running /friends: %q", got)
	}
	if v := m.View(); !strings.Contains(v, "no friends") && !strings.Contains(v, "friends") {
		t.Fatalf("/friends did not run:\n%s", v)
	}
}

func TestVersionInStatusBarAndUpdateNotice(t *testing.T) {
	mem := memory.New()
	admin := newTestApp(t, mem, "sujay03")
	m := newTestUI(t, admin)
	if v := m.View(); !strings.Contains(v, "vtest") {
		t.Fatalf("version missing from status bar:\n%s", v)
	}
	if m.checkUpdate() != nil {
		t.Fatal("dev/test builds must not check GitHub")
	}
	m.Update(updateMsg{tag: "v9.9.9"})
	v := m.View()
	if !strings.Contains(v, "v9.9.9 available · /update") {
		t.Fatalf("update notice missing from status bar:\n%s", v)
	}
	if !strings.Contains(v, "run /update") {
		t.Fatalf("update note missing from chat:\n%s", v)
	}
	for _, c := range []struct {
		tag, cur string
		want     bool
	}{{"v0.1.4", "0.1.3", true}, {"v0.1.3", "0.1.3", false}, {"v0.1.2", "0.1.3", false}, {"v1.0.0", "0.9.9", true}, {"v0.1.4", "abc1234", false}} {
		if got := newerVersion(c.tag, c.cur); got != c.want {
			t.Errorf("newerVersion(%q,%q)=%v", c.tag, c.cur, got)
		}
	}
}

func TestBootScreenShowsProgressThenReady(t *testing.T) {
	mem := memory.New()
	ready := newTestApp(t, mem, "sujay03") // registers and reaches ready
	st := ready.Store()
	// A fresh App on the same store and backend resumes the saved session.
	a2 := app.New(app.Options{Backend: mem.Client(), Store: st})
	if err := a2.Prepare(); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), a2, Options{Version: "test", Boot: a2.Resume})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if v := m.View(); !strings.Contains(v, "starting") {
		t.Fatalf("boot screen missing:\n%s", v)
	}
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no commands")
	}
	m.Update(evMsg{app.Event{Kind: app.EvBootStep, Text: "signing in as x@y"}})
	if v := m.View(); !strings.Contains(v, "signing in as x@y") {
		t.Fatalf("boot step not shown:\n%s", v)
	}
	m.Update(bootDoneMsg{err: errors.New("no network")})
	if v := m.View(); !strings.Contains(v, "could not start: no network") || !strings.Contains(v, "r retry") {
		t.Fatalf("boot error not shown:\n%s", v)
	}
}

func TestArrowKeysScrollChatWhenStripClosed(t *testing.T) {
	mem := memory.New()
	admin := newTestApp(t, mem, "sujay03")
	m := newTestUI(t, admin)
	keys(m, "/help")
	press(m, tea.KeyEnter)
	if !m.vp.AtBottom() {
		t.Fatal("expected to start at the bottom")
	}
	top := m.vp.YOffset
	press(m, tea.KeyUp)
	if m.vp.YOffset >= top {
		t.Fatalf("up did not scroll: %d -> %d", top, m.vp.YOffset)
	}
	press(m, tea.KeyDown)
	if m.vp.YOffset != top {
		t.Fatalf("down did not scroll back: %d", m.vp.YOffset)
	}
	// With the strip open the arrows move the highlight instead.
	keys(m, "/")
	press(m, tea.KeyDown)
	if m.sugIdx != 1 {
		t.Fatalf("strip should own the arrows: idx=%d", m.sugIdx)
	}
}

func TestDailyDigestAndRegStatus(t *testing.T) {
	mem := memory.New()
	admin := newTestApp(t, mem, "sujay03")
	m := newTestUI(t, admin)
	if v := m.View(); !strings.Contains(v, "reg open") {
		t.Fatalf("registration mode missing from the admin status bar:\n%s", v)
	}
	// The ready event already consumed today's digest; make it a new day.
	p := admin.Store().Prefs()
	p.LastOpened = time.Time{}
	_ = admin.Store().SavePrefs(p)
	cmd := m.digestCmd()
	if cmd == nil {
		t.Fatal("first open of the day should produce a digest")
	}
	m.Update(cmd())
	v := m.View()
	for _, want := range []string{"today's digest", "no unread messages", "registration: open · approval: off"} {
		if !strings.Contains(v, want) {
			t.Fatalf("digest lacks %q:\n%s", want, v)
		}
	}
	if m.digestCmd() != nil {
		t.Fatal("second open on the same day must not repeat the digest")
	}
	user := newTestApp(t, mem, "priya")
	um := newTestUI(t, user)
	if v := um.View(); strings.Contains(v, "reg open") {
		t.Fatalf("user status bar shows the registration mode:\n%s", v)
	}
}

func TestStartupPasskeyGatesBoot(t *testing.T) {
	mem := memory.New()
	ready := newTestApp(t, mem, "sujay03")
	st := ready.Store()
	lock := admin.NewLockAt(filepath.Join(t.TempDir(), "app.pass"))
	if err := lock.Set("", "open-sesame"); err != nil {
		t.Fatal(err)
	}
	a2 := app.New(app.Options{Backend: mem.Client(), Store: st})
	if err := a2.Prepare(); err != nil {
		t.Fatal(err)
	}
	booted := make(chan struct{}, 1)
	m := newModel(context.Background(), a2, Options{Version: "test", AppLock: lock, Boot: func(ctx context.Context) error {
		err := a2.Resume(ctx)
		booted <- struct{}{}
		return err
	}})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if cmd := m.Init(); cmd != nil {
		// Init must not start Boot while locked; drain any batch to be sure.
		select {
		case <-booted:
			t.Fatal("boot ran before the passkey was entered")
		default:
		}
	}
	if v := m.View(); !strings.Contains(v, "locked") || !strings.Contains(v, "passkey") {
		t.Fatalf("lock screen missing:\n%s", v)
	}
	keys(m, "wrong-one")
	press(m, tea.KeyEnter)
	if v := m.View(); !strings.Contains(v, "wrong passphrase") {
		t.Fatalf("wrong passkey not reported:\n%s", v)
	}
	keys(m, "open-sesame")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.locked || cmd == nil {
		t.Fatal("correct passkey did not unlock and start boot")
	}
	// Run the returned batch far enough to see Boot invoked.
	runCmds(t, cmd, booted)
}

// runCmds executes a command tree until the boot signal arrives.
func runCmds(t *testing.T, cmd tea.Cmd, booted chan struct{}) {
	t.Helper()
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			if c == nil {
				continue
			}
			go c()
		}
	}
	select {
	case <-booted:
	case <-time.After(3 * time.Second):
		t.Fatal("boot never ran after unlocking")
	}
}
