package app

import (
	"context"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous/memory"
	"github.com/Kumarsujay03/pairdesk/internal/store"
)

type harness struct {
	t     *testing.T
	store *memory.Store
	ctx   context.Context
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, store: memory.New(), ctx: context.Background()}
}

// user boots an App through the full first-run flow.
func (h *harness) user(email, name string) *App {
	h.t.Helper()
	dir := h.t.TempDir()
	st, err := store.Open(store.Paths{Config: dir + "/c", State: dir + "/s", Cache: dir + "/k"})
	if err != nil {
		h.t.Fatal(err)
	}
	a := New(Options{Backend: h.store.Client(), Store: st})
	if err := a.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if a.Phase() != PhaseNeedLogin {
		h.t.Fatalf("%s: phase %v", name, a.Phase())
	}
	if err := a.RequestLogin(h.ctx, email); err != nil {
		h.t.Fatal(err)
	}
	if err := a.VerifyOTP(h.ctx, memory.TestOTP); err != nil {
		h.t.Fatal(err)
	}
	if a.Phase() != PhaseNeedUsername {
		h.t.Fatalf("%s: phase %v", name, a.Phase())
	}
	if err := a.Register(h.ctx, name, ""); err != nil {
		h.t.Fatal(err)
	}
	return a
}

func waitEvent(t *testing.T, a *App, kind EventKind) Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-a.Events():
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", kind)
		}
	}
}

func waitPhase(t *testing.T, a *App, p Phase) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-a.Events():
			if e.Kind == EvPhase && e.Phase == p {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for phase %v (now %v)", p, a.Phase())
		}
	}
}

func drain(a *App) {
	for {
		select {
		case <-a.Events():
		default:
			return
		}
	}
}

func befriend(t *testing.T, h *harness, a, b *App) {
	t.Helper()
	if err := a.AddFriend(h.ctx, b.Me().Username); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, b, EvFriendRequest)
	if err := b.RespondFriend(h.ctx, a.Me().Username, true); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, a, EvFriendAccepted)
	waitEvent(t, b, EvFriendAccepted)
	if !a.IsFriend(b.Me().ID) || !b.IsFriend(a.Me().ID) {
		t.Fatal("friendship not symmetric")
	}
}

func TestFirstUserIsAdminAndReady(t *testing.T) {
	h := newHarness(t)
	admin := h.user("sujay@example.com", "sujay03")
	if admin.Phase() != PhaseReady || !admin.IsAdmin() {
		t.Fatalf("phase %v admin %v", admin.Phase(), admin.IsAdmin())
	}
	if admin.Me().PubKey != admin.Identity().Public().String() {
		t.Fatal("pubkey not published")
	}
	u := h.user("arjun@example.com", "arjun")
	if u.IsAdmin() {
		t.Fatal("second user must not be admin")
	}
}

func TestDMEncryptedEndToEnd(t *testing.T) {
	h := newHarness(t)
	admin := h.user("a@x", "sujay03")
	arjun := h.user("b@x", "arjun")
	priya := h.user("c@x", "priya")
	drain(admin)
	drain(arjun)
	drain(priya)

	// Strangers cannot DM.
	if _, err := arjun.OpenDM(h.ctx, "priya"); err == nil {
		t.Fatal("stranger DM allowed")
	}
	befriend(t, h, arjun, priya)

	c, err := arjun.OpenDM(h.ctx, "priya")
	if err != nil {
		t.Fatal(err)
	}
	if err := arjun.Send(h.ctx, c, "hello priya"); err != nil {
		t.Fatal(err)
	}
	e := waitEvent(t, priya, EvMessage)
	if e.Msg.Text != "hello priya" || !e.Msg.Decrypted || e.Msg.Mine {
		t.Fatalf("got %+v", e.Msg)
	}
	// Server only holds ciphertext.
	raw, err := h.store.Client().ListMessages(h.ctx, rendezvous.MessageQuery{})
	if err == nil {
		for _, m := range raw {
			if string(m.Ciphertext) == "hello priya" {
				t.Fatal("plaintext stored")
			}
		}
	}
	// Admin can DM a stranger; the stranger can reply.
	drain(arjun)
	c2, err := admin.OpenDM(h.ctx, "arjun")
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Send(h.ctx, c2, "hi from admin"); err != nil {
		t.Fatal(err)
	}
	e = waitEvent(t, arjun, EvMessage)
	if e.Msg.Text != "hi from admin" {
		t.Fatalf("got %q", e.Msg.Text)
	}
	drain(admin)
	if err := arjun.Send(h.ctx, arjun.Conv(c2.ID), "hi back"); err != nil {
		t.Fatal(err)
	}
	e = waitEvent(t, admin, EvMessage)
	if e.Msg.Text != "hi back" {
		t.Fatalf("got %q", e.Msg.Text)
	}
	// Reaction and /me.
	drain(arjun)
	if err := admin.React(h.ctx, c2, "👍"); err != nil {
		t.Fatal(err)
	}
	e = waitEvent(t, arjun, EvMessage)
	if e.Msg.Kind != "reaction" || e.Msg.ReactsTo == "" {
		t.Fatalf("reaction %+v", e.Msg)
	}
}

func TestGroupsAndRotation(t *testing.T) {
	h := newHarness(t)
	_ = h.user("a@x", "sujay03")
	arjun := h.user("b@x", "arjun")
	priya := h.user("c@x", "priya")
	rahul := h.user("d@x", "rahul")
	for _, a := range []*App{arjun, priya, rahul} {
		drain(a)
	}
	befriend(t, h, arjun, priya)
	befriend(t, h, arjun, rahul)

	g, err := arjun.CreateGroup(h.ctx, "linux-lab")
	if err != nil {
		t.Fatal(err)
	}
	grp, _ := arjun.GroupByName("linux-lab")
	if err := arjun.InviteToGroup(h.ctx, grp, "priya"); err != nil {
		t.Fatal(err)
	}
	if err := arjun.InviteToGroup(h.ctx, grp, "rahul"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, priya, EvGroupInvite)
	waitEvent(t, rahul, EvGroupInvite)
	drain(priya)
	drain(rahul)
	if err := arjun.Send(h.ctx, g, "welcome all"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*App{priya, rahul} {
		e := waitEvent(t, a, EvMessage)
		if e.Msg.Text != "welcome all" {
			t.Fatalf("%s got %q", a.Me().Username, e.Msg.Text)
		}
	}
	// Remove rahul: key rotates, rahul cannot read new messages.
	if err := arjun.RemoveFromGroup(h.ctx, grp, "rahul"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, rahul, EvGroupChanged)
	waitEvent(t, priya, EvGroupChanged)
	drain(priya)
	time.Sleep(100 * time.Millisecond)
	grp, _ = arjun.GroupByName("linux-lab")
	if grp.KeyEpoch != 3 {
		t.Fatalf("epoch %d", grp.KeyEpoch)
	}
	if err := arjun.Send(h.ctx, g, "after rotation"); err != nil {
		t.Fatal(err)
	}
	e := waitEvent(t, priya, EvMessage)
	if e.Msg.Text != "after rotation" || !e.Msg.Decrypted {
		t.Fatalf("priya after rotation: %+v", e.Msg)
	}
	if rahul.Conv(grp.ID) != nil {
		t.Fatal("rahul still has the group")
	}
}

func TestGrantsSignedAndEnforced(t *testing.T) {
	h := newHarness(t)
	admin := h.user("a@x", "sujay03")
	arjun := h.user("b@x", "arjun")
	drain(admin)
	drain(arjun)
	// Grant to a plain user is refused.
	priya := h.user("c@x", "priya")
	befriend(t, h, arjun, priya)
	if err := arjun.Grant(h.ctx, "priya", model.ScopeBoard, model.SudoBlocked, ""); err == nil {
		t.Fatal("grant to non-admin allowed")
	}
	if err := arjun.Grant(h.ctx, "sujay03", model.ScopeBoard, model.SudoAsk, ""); err != nil {
		t.Fatal(err)
	}
	g := arjun.GrantFor(admin.Me().ID, admin.Identity().Public())
	if g == nil || g.Sudo != model.SudoAsk {
		t.Fatalf("grant not enforced locally: %+v", g)
	}
	// Admin sees the hint.
	held, err := admin.HeldGrantFrom(h.ctx, arjun.Me().ID, model.ScopeBoard)
	if err != nil || held == nil {
		t.Fatalf("held %v %v", held, err)
	}
	// Tampered local copy fails verification.
	gr, _ := arjun.Store().Grants()
	gr.Given[0].Sudo = model.SudoAuto
	_ = arjun.Store().SaveGrants(gr)
	arjun.mu.Lock()
	arjun.grants = gr
	arjun.mu.Unlock()
	if arjun.GrantFor(admin.Me().ID, admin.Identity().Public()) != nil {
		t.Fatal("tampered grant accepted")
	}
	// Revoke clears.
	if err := arjun.Revoke(h.ctx, "sujay03", false); err != nil {
		t.Fatal(err)
	}
	held, _ = admin.HeldGrantFrom(h.ctx, arjun.Me().ID, model.ScopeBoard)
	if held != nil {
		t.Fatal("revoke not reflected")
	}
}

func TestAdminControls(t *testing.T) {
	h := newHarness(t)
	admin := h.user("a@x", "sujay03")
	arjun := h.user("b@x", "arjun")
	drain(admin)
	drain(arjun)
	if err := arjun.SetRegistration(h.ctx, model.RegClosed); err != ErrNotAdmin {
		t.Fatalf("non-admin got %v", err)
	}
	if err := admin.SetRegistration(h.ctx, model.RegClosed); err != nil {
		t.Fatal(err)
	}
	// New user hits the wall.
	dir := t.TempDir()
	st, _ := store.Open(store.Paths{Config: dir + "/c", State: dir + "/s", Cache: dir + "/k"})
	late := New(Options{Backend: h.store.Client(), Store: st})
	_ = late.Start(h.ctx)
	_ = late.RequestLogin(h.ctx, "late@x")
	if err := late.VerifyOTP(h.ctx, memory.TestOTP); err != nil {
		t.Fatal(err)
	}
	if late.Phase() != PhaseRegClosed {
		t.Fatalf("phase %v", late.Phase())
	}
	// Invite mode.
	if err := admin.SetRegistration(h.ctx, model.RegInvite); err != nil {
		t.Fatal(err)
	}
	inv, err := admin.NewInvite(h.ctx, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := late.Register(h.ctx, "latecomer", "nope"); err == nil {
		t.Fatal("bad invite accepted")
	}
	if err := late.Register(h.ctx, "latecomer", inv.Code); err != nil {
		t.Fatal(err)
	}
	// Ban.
	if err := admin.SetUserFlag(h.ctx, "latecomer", "banned", true); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, late, EvBanned)
	// Helper promotion + badge + stats + log.
	if err := admin.SetHelper(h.ctx, "arjun", true); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, arjun, EvNotice)
	if !arjun.IsHelperOrAdmin() {
		t.Fatal("helper role not applied")
	}
	if err := admin.Announce(h.ctx, "deploy tonight"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, arjun, EvAnnouncement)
	log, err := admin.AdminLog(h.ctx, 50)
	if err != nil || len(log) < 5 {
		t.Fatalf("admin log %d %v", len(log), err)
	}
	s, err := admin.Stats(h.ctx)
	if err != nil || s.UsersTotal != 3 {
		t.Fatalf("stats %+v %v", s, err)
	}
	users, err := admin.AllUsers(h.ctx)
	if err != nil || len(users) != 3 {
		t.Fatalf("users %d %v", len(users), err)
	}
	if _, err := arjun.AllUsers(h.ctx); err != ErrNotAdmin {
		t.Fatal("helper listed users")
	}
	// Maintenance kicks users to the maintenance phase and back.
	if err := admin.Maintenance(h.ctx, true, "back soon"); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, arjun, PhaseMaintenance)
	if arjun.Phase() != PhaseMaintenance {
		t.Fatalf("phase %v", arjun.Phase())
	}
	_ = admin.Maintenance(h.ctx, false, "")
	waitPhase(t, arjun, PhaseReady)
	if arjun.Phase() != PhaseReady {
		t.Fatalf("phase %v", arjun.Phase())
	}
}

func TestPresenceAndRestore(t *testing.T) {
	h := newHarness(t)
	admin := h.user("a@x", "sujay03")
	arjun := h.user("b@x", "arjun")
	drain(admin)
	drain(arjun)
	befriend(t, h, admin, arjun)
	drain(admin)
	if !admin.Online(arjun.Me().ID) {
		// presence snapshot may arrive async
		waitEvent(t, admin, EvPresence)
	}
	if !admin.Online(arjun.Me().ID) {
		t.Fatal("arjun should be online")
	}
	arjun.Close()
	e := waitEvent(t, admin, EvPresence)
	if e.UserID != arjun.Me().ID || e.Online {
		t.Fatalf("expected offline, got %+v", e)
	}
	// Restore from saved login without OTP.
	again := New(Options{Backend: h.store.Client(), Store: arjun.Store()})
	if err := again.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	if again.Phase() != PhaseReady || again.Me().Username != "arjun" {
		t.Fatalf("restore phase %v user %q", again.Phase(), again.Me().Username)
	}
	if !again.Identity().Public().Equal(arjun.Identity().Public()) {
		t.Fatal("identity changed across restart")
	}
}

func TestSearchIndexesDecryptedHistory(t *testing.T) {
	h := newHarness(t)
	_ = h.user("a@x", "sujay03")
	arjun := h.user("b@x", "arjun")
	priya := h.user("c@x", "priya")
	drain(arjun)
	drain(priya)
	befriend(t, h, arjun, priya)
	c, _ := arjun.OpenDM(h.ctx, "priya")
	_ = arjun.Send(h.ctx, c, "the nginx config is broken")
	waitEvent(t, priya, EvMessage)
	hits, err := priya.Search("nginx", "", 10)
	if err != nil || len(hits) != 1 || hits[0].Sender != arjun.Me().ID {
		t.Fatalf("hits %+v %v", hits, err)
	}
	hits, _ = arjun.Search("broken", "", 10)
	if len(hits) != 1 {
		t.Fatal("sender side not indexed")
	}
}
