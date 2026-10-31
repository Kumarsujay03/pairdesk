package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

var ctx = context.Background()

// fixture is a store with a movable clock plus helpers to sign users in.
type fixture struct {
	t     *testing.T
	store *Store
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, store: New(), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	f.store.SetClock(func() time.Time { return f.now })
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// login signs in with the test OTP but does not register.
func (f *fixture) login(email string) rendezvous.Backend {
	f.t.Helper()
	c := f.store.Client()
	if _, err := c.VerifyOTP(ctx, email, TestOTP); err != nil {
		f.t.Fatalf("VerifyOTP(%s): %v", email, err)
	}
	return c
}

// user signs in and registers username. The first call makes the admin.
func (f *fixture) user(name string) (rendezvous.Backend, model.User) {
	f.t.Helper()
	c := f.login(name + "@example.test")
	u, err := c.Register(ctx, name, "pk-"+name, "")
	if err != nil {
		f.t.Fatalf("Register(%s): %v", name, err)
	}
	return c, u
}

// befriend completes a request from a to b.
func (f *fixture) befriend(a, b rendezvous.Backend) {
	f.t.Helper()
	if err := a.SendFriendRequest(ctx, b.Session().UserID); err != nil {
		f.t.Fatalf("SendFriendRequest: %v", err)
	}
	if err := b.RespondFriendRequest(ctx, a.Session().UserID, true); err != nil {
		f.t.Fatalf("RespondFriendRequest: %v", err)
	}
}

func connect(t *testing.T, c rendezvous.Backend) <-chan rendezvous.Event {
	t.Helper()
	ch, err := c.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return ch
}

// nextEvent returns the next event of the given kind, skipping others.
func nextEvent(t *testing.T, ch <-chan rendezvous.Event, kind rendezvous.EventKind) rendezvous.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %s event delivered", kind)
		}
	}
}

func noEvent(t *testing.T, ch <-chan rendezvous.Event) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func wantErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}

func dm(from rendezvous.Backend, to string) model.Message {
	return model.Message{
		ConvType:   model.ConvDM,
		ConvID:     model.DMConvID(from.Session().UserID, to),
		Ciphertext: []byte("ct"),
		Nonce:      []byte("n"),
	}
}

// --- auth ---

func TestAuth(t *testing.T) {
	f := newFixture(t)
	c := f.store.Client()
	if c.Session() != nil {
		t.Fatal("fresh client has a session")
	}
	_, err := c.Me(ctx)
	wantErr(t, err, rendezvous.ErrUnauthorized)

	_, err = c.VerifyOTP(ctx, "a@example.test", "123456")
	wantErr(t, err, rendezvous.ErrUnauthorized)

	s1, err := c.VerifyOTP(ctx, "A@example.test", TestOTP)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := f.store.Client().VerifyOTP(ctx, "a@example.test ", TestOTP)
	if s1.UserID != s2.UserID {
		t.Fatal("same email should map to the same user id")
	}

	r := f.store.Client()
	rs, err := r.Restore(ctx, s1.RefreshToken)
	if err != nil || rs.UserID != s1.UserID {
		t.Fatalf("Restore: %v (%+v)", err, rs)
	}
	_, err = f.store.Client().Restore(ctx, s1.RefreshToken)
	wantErr(t, err, rendezvous.ErrUnauthorized) // old token retired

	if err := r.SignOut(ctx); err != nil || r.Session() != nil {
		t.Fatal("SignOut did not clear the session")
	}
	_, err = f.store.Client().Restore(ctx, rs.RefreshToken)
	wantErr(t, err, rendezvous.ErrUnauthorized)
}

// --- registration ---

func TestFirstUserIsAdmin(t *testing.T) {
	f := newFixture(t)
	_, admin := f.user("sujay03")
	if admin.Role != model.RoleAdmin || !admin.Approved {
		t.Fatalf("first user: %+v", admin)
	}
	c, second := f.user("priya")
	if second.Role != model.RoleUser {
		t.Fatalf("second user role: %s", second.Role)
	}
	st, _ := c.Settings(ctx)
	if st.AdminPubKey != "pk-sujay03" {
		t.Fatalf("AdminPubKey = %q", st.AdminPubKey)
	}
	_, err := c.Register(ctx, "again", "pk", "")
	wantErr(t, err, rendezvous.ErrConflict)
}

func TestRegistrationModes(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fixture, admin rendezvous.Backend) (invite string)
		want  error
		check func(t *testing.T, u model.User)
	}{
		{name: "open", setup: func(*fixture, rendezvous.Backend) string { return "" }},
		{
			name: "closed",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "registration", model.RegClosed)
				return ""
			},
			want: rendezvous.ErrRegClosed,
		},
		{
			name: "invite valid",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "registration", model.RegInvite)
				inv, _ := a.CreateInvite(ctx, 1, nil)
				return inv.Code
			},
		},
		{
			name: "invite unknown",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "registration", model.RegInvite)
				return "NOPE"
			},
			want: rendezvous.ErrBadInvite,
		},
		{
			name: "invite exhausted",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "registration", model.RegInvite)
				inv, _ := a.CreateInvite(ctx, 1, nil)
				f.login("x@example.test").Register(ctx, "someone", "pk", inv.Code)
				return inv.Code
			},
			want: rendezvous.ErrBadInvite,
		},
		{
			name: "invite expired",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "registration", model.RegInvite)
				exp := f.now.Add(time.Hour)
				inv, _ := a.CreateInvite(ctx, 5, &exp)
				f.advance(2 * time.Hour)
				return inv.Code
			},
			want: rendezvous.ErrBadInvite,
		},
		{
			name: "invite revoked",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "registration", model.RegInvite)
				inv, _ := a.CreateInvite(ctx, 5, nil)
				a.RevokeInvite(ctx, inv.Code)
				return inv.Code
			},
			want: rendezvous.ErrBadInvite,
		},
		{
			name: "cap reached",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "user_cap", 1)
				return ""
			},
			want: rendezvous.ErrRegClosed,
		},
		{
			name: "cap not reached",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "user_cap", 2)
				return ""
			},
		},
		{
			name: "require approval",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "require_approval", true)
				return ""
			},
			check: func(t *testing.T, u model.User) {
				if u.Approved {
					t.Fatal("user should await approval")
				}
			},
		},
		{
			name: "maintenance",
			setup: func(f *fixture, a rendezvous.Backend) string {
				a.SetSetting(ctx, "maintenance", true)
				return ""
			},
			want: rendezvous.ErrMaintenance,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			admin, _ := f.user("sujay03")
			code := tc.setup(f, admin)
			u, err := f.login("new@example.test").Register(ctx, "newbie", "pk", code)
			if tc.want != nil {
				wantErr(t, err, tc.want)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if u.Role != model.RoleUser || u.Username != "newbie" {
				t.Fatalf("registered %+v", u)
			}
			if tc.check != nil {
				tc.check(t, u)
			} else if !u.Approved {
				t.Fatal("user should be approved")
			}
		})
	}
}

func TestUsernameRules(t *testing.T) {
	f := newFixture(t)
	f.user("sujay03")
	tests := []struct {
		name string
		want error
	}{
		{"priya", nil},
		{"SUJAY03", rendezvous.ErrUsernameTaken},
		{"Priya", rendezvous.ErrUsernameTaken},
		{"ab", ErrInvalidUsername},
		{"has space", ErrInvalidUsername},
		{"has-dash", ErrInvalidUsername},
		{"abcdefghijklmnopqrstu", ErrInvalidUsername},
		{"ok_name_1", nil},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := f.login(fmt.Sprintf("u%d@example.test", i))
			avail, err := c.CheckUsername(ctx, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if avail != (tc.want == nil) {
				t.Fatalf("CheckUsername(%q) = %v", tc.name, avail)
			}
			_, err = c.Register(ctx, tc.name, "pk", "")
			wantErr(t, err, tc.want)
		})
	}
}

func TestUnapprovedUserIsForbidden(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	admin.SetSetting(ctx, "require_approval", true)
	c, u := f.user("priya")
	_, err := c.UserByName(ctx, "sujay03")
	wantErr(t, err, rendezvous.ErrForbidden)
	if _, err := c.Me(ctx); err != nil {
		t.Fatalf("Me should work while pending: %v", err)
	}
	if err := admin.SetUserFlag(ctx, u.ID, "approved", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserByName(ctx, "sujay03"); err != nil {
		t.Fatalf("after approval: %v", err)
	}
}

func TestUserLookups(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	c, p := f.user("priya")
	_, err := c.ListUsers(ctx)
	wantErr(t, err, rendezvous.ErrForbidden)
	all, err := admin.ListUsers(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListUsers: %v %d", err, len(all))
	}
	got, err := c.UserByName(ctx, "Sujay03")
	if err != nil || got.ID != a.ID {
		t.Fatalf("UserByName: %v", err)
	}
	if _, err := c.UserByID(ctx, "nope"); !errors.Is(err, rendezvous.ErrNotFound) {
		t.Fatalf("UserByID unknown: %v", err)
	}
	many, _ := c.UsersByIDs(ctx, []string{p.ID, "nope", a.ID})
	if len(many) != 2 || many[0].ID != p.ID {
		t.Fatalf("UsersByIDs: %+v", many)
	}
}

// --- friends ---

func TestFriendRequestFlow(t *testing.T) {
	f := newFixture(t)
	f.user("sujay03")
	priya, p := f.user("priya")
	arjun, a := f.user("arjun")
	third, _ := f.user("third")
	chA := connect(t, arjun)

	wantErr(t, priya.SendFriendRequest(ctx, p.ID), rendezvous.ErrForbidden)
	wantErr(t, priya.SendFriendRequest(ctx, "nope"), rendezvous.ErrNotFound)

	if err := priya.SendFriendRequest(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, chA, rendezvous.EvFriendship)
	if ev.Friendship.Status != model.FriendPending || ev.Friendship.Requester != p.ID {
		t.Fatalf("event %+v", ev.Friendship)
	}
	wantErr(t, priya.SendFriendRequest(ctx, a.ID), rendezvous.ErrConflict)
	wantErr(t, arjun.SendFriendRequest(ctx, p.ID), rendezvous.ErrConflict)

	// only the addressee responds
	wantErr(t, priya.RespondFriendRequest(ctx, a.ID, true), rendezvous.ErrForbidden)
	wantErr(t, third.RespondFriendRequest(ctx, p.ID, true), rendezvous.ErrNotFound)

	if err := arjun.RespondFriendRequest(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	fs, _ := priya.ListFriendships(ctx)
	if len(fs) != 1 || fs[0].Status != model.FriendAccepted {
		t.Fatalf("friendships %+v", fs)
	}
	wantErr(t, arjun.RespondFriendRequest(ctx, p.ID, true), rendezvous.ErrForbidden) // not pending

	if err := arjun.RemoveFriend(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	fs, _ = priya.ListFriendships(ctx)
	if len(fs) != 0 {
		t.Fatalf("friendships after removal %+v", fs)
	}
	wantErr(t, priya.RemoveFriend(ctx, a.ID), rendezvous.ErrNotFound)
}

func TestFriendDenialCooldown(t *testing.T) {
	f := newFixture(t)
	f.user("sujay03")
	priya, p := f.user("priya")
	arjun, a := f.user("arjun")

	if err := priya.SendFriendRequest(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := arjun.RespondFriendRequest(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	wantErr(t, priya.SendFriendRequest(ctx, a.ID), rendezvous.ErrConflict)
	f.advance(23 * time.Hour)
	wantErr(t, priya.SendFriendRequest(ctx, a.ID), rendezvous.ErrConflict)
	f.advance(time.Hour + time.Second)
	if err := priya.SendFriendRequest(ctx, a.ID); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	fs, _ := arjun.ListFriendships(ctx)
	if len(fs) != 1 || fs[0].Status != model.FriendPending {
		t.Fatalf("friendships %+v", fs)
	}
}

// --- messages ---

func TestDMAuthorization(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	arjun, ar := f.user("arjun")
	stranger, s := f.user("stranger")
	_, pe := f.user("pending")
	f.befriend(priya, arjun)
	priya.SendFriendRequest(ctx, pe.ID)

	tests := []struct {
		name string
		from rendezvous.Backend
		msg  model.Message
		want error
	}{
		{"friends", priya, dm(priya, ar.ID), nil},
		{"friends reverse", arjun, dm(arjun, p.ID), nil},
		{"admin to stranger", admin, dm(admin, s.ID), nil},
		{"stranger to admin", stranger, dm(stranger, a.ID), nil},
		{"strangers", priya, dm(priya, s.ID), rendezvous.ErrForbidden},
		{"pending only", priya, dm(priya, pe.ID), rendezvous.ErrForbidden},
		{"not a party", stranger, dm(priya, ar.ID), rendezvous.ErrForbidden},
		{"non-canonical id", priya, model.Message{ConvType: model.ConvDM, ConvID: reversedDM(p.ID, ar.ID)}, rendezvous.ErrForbidden},
		{"unknown other", priya, dm(priya, "nope"), rendezvous.ErrForbidden},
		{"unknown conv type", priya, model.Message{ConvType: "x", ConvID: "y"}, rendezvous.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.msg.Sender = "spoofed"
			got, err := tc.from.SendMessage(ctx, tc.msg)
			wantErr(t, err, tc.want)
			if err == nil && got.Sender != tc.from.Session().UserID {
				t.Fatalf("sender %q not forced to session user", got.Sender)
			}
		})
	}

	// Listing follows the same rule.
	if _, err := stranger.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: model.DMConvID(p.ID, ar.ID)}); !errors.Is(err, rendezvous.ErrForbidden) {
		t.Fatalf("stranger ListMessages: %v", err)
	}
	msgs, err := arjun.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: model.DMConvID(p.ID, ar.ID)})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("ListMessages: %v, %d rows", err, len(msgs))
	}
}

func TestMessageEventsAndPaging(t *testing.T) {
	f := newFixture(t)
	f.user("sujay03")
	priya, p := f.user("priya")
	arjun, a := f.user("arjun")
	other, _ := f.user("other")
	f.befriend(priya, arjun)
	chA, chO := connect(t, arjun), connect(t, other)
	drain(chA)

	conv := model.DMConvID(p.ID, a.ID)
	for i := 0; i < 5; i++ {
		f.advance(time.Second)
		if _, err := priya.SendMessage(ctx, dm(priya, a.ID)); err != nil {
			t.Fatal(err)
		}
	}
	ev := nextEvent(t, chA, rendezvous.EvMessage)
	if ev.Message.ConvID != conv || ev.Message.Sender != p.ID {
		t.Fatalf("event %+v", ev.Message)
	}
	noEvent(t, chO)

	all, _ := arjun.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: conv})
	last2, _ := arjun.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: conv, Limit: 2})
	if len(all) != 5 || len(last2) != 2 || last2[1].ID != all[4].ID || last2[0].ID != all[3].ID {
		t.Fatalf("paging: all=%d last2=%d", len(all), len(last2))
	}
	before, _ := arjun.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: conv, Before: all[2].CreatedAt})
	after, _ := arjun.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: conv, After: all[2].CreatedAt})
	if len(before) != 2 || len(after) != 2 {
		t.Fatalf("before=%d after=%d", len(before), len(after))
	}
}

func drain(ch <-chan rendezvous.Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, _ := f.user("priya")
	admin.SetSetting(ctx, "msgs_per_min", 2)

	for i := 0; i < 2; i++ {
		if _, err := priya.SendMessage(ctx, dm(priya, a.ID)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	_, err := priya.SendMessage(ctx, dm(priya, a.ID))
	wantErr(t, err, rendezvous.ErrRateLimited)
	// Limits are per user: the admin still sends.
	if _, err := admin.SendMessage(ctx, dm(admin, priya.Session().UserID)); err != nil {
		t.Fatalf("admin send: %v", err)
	}
	f.advance(61 * time.Second)
	if _, err := priya.SendMessage(ctx, dm(priya, a.ID)); err != nil {
		t.Fatalf("after window: %v", err)
	}
	admin.SetSetting(ctx, "msgs_per_min", 0)
	for i := 0; i < 10; i++ {
		if _, err := priya.SendMessage(ctx, dm(priya, a.ID)); err != nil {
			t.Fatalf("unlimited send %d: %v", i, err)
		}
	}
}

func TestMutedCannotSend(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	admin.SetUserFlag(ctx, p.ID, "muted", true)
	_, err := priya.SendMessage(ctx, dm(priya, a.ID))
	wantErr(t, err, rendezvous.ErrForbidden)
	if _, err := priya.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: model.DMConvID(p.ID, a.ID)}); err != nil {
		t.Fatalf("muted users can still read: %v", err)
	}
}

// --- groups ---

func TestGroupMembership(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	owner, o := f.user("owner")
	friend, fr := f.user("friend")
	stranger, s := f.user("stranger")
	f.befriend(owner, friend)
	chF := connect(t, friend)

	g, err := owner.CreateGroup(ctx, "team", []byte("wrapped-owner"))
	if err != nil || g.Owner != o.ID || g.KeyEpoch != 1 {
		t.Fatalf("CreateGroup: %v %+v", err, g)
	}
	members, _ := owner.ListGroupMembers(ctx, g.ID)
	if len(members) != 1 || string(members[0].WrappedKey) != "wrapped-owner" {
		t.Fatalf("members after create: %+v", members)
	}

	add := func(by rendezvous.Backend, who string) error {
		return by.AddGroupMember(ctx, model.GroupMember{GroupID: g.ID, UserID: who, WrappedKey: []byte("w")})
	}
	wantErr(t, add(owner, s.ID), rendezvous.ErrForbidden)   // not a friend
	wantErr(t, add(friend, fr.ID), rendezvous.ErrForbidden) // not the owner
	wantErr(t, add(owner, "nope"), rendezvous.ErrForbidden) // unknown user
	wantErr(t, owner.AddGroupMember(ctx, model.GroupMember{GroupID: "nope", UserID: fr.ID}), rendezvous.ErrNotFound)
	if err := add(owner, fr.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, add(owner, fr.ID), rendezvous.ErrConflict)

	ev := nextEvent(t, chF, rendezvous.EvGroup)
	if ev.Group.ID != g.ID {
		t.Fatalf("group event %+v", ev.Group)
	}
	ev = nextEvent(t, chF, rendezvous.EvGroupMember)
	if ev.GroupMember.UserID != fr.ID || ev.GroupMember.WrappedBy != o.ID || ev.GroupMember.KeyEpoch != 1 {
		t.Fatalf("member event %+v", ev.GroupMember)
	}

	// visibility
	gs, _ := friend.ListGroups(ctx)
	if len(gs) != 1 {
		t.Fatalf("friend ListGroups: %+v", gs)
	}
	gs, _ = stranger.ListGroups(ctx)
	if len(gs) != 0 {
		t.Fatalf("stranger ListGroups: %+v", gs)
	}
	gs, _ = admin.ListGroups(ctx)
	if len(gs) != 1 {
		t.Fatalf("admin ListGroups: %+v", gs)
	}
	_, err = stranger.ListGroupMembers(ctx, g.ID)
	wantErr(t, err, rendezvous.ErrForbidden)

	// group messages need membership
	gm := model.Message{ConvType: model.ConvGroup, ConvID: g.ID, Ciphertext: []byte("ct")}
	if _, err := friend.SendMessage(ctx, gm); err != nil {
		t.Fatalf("member send: %v", err)
	}
	_, err = stranger.SendMessage(ctx, gm)
	wantErr(t, err, rendezvous.ErrForbidden)
	_, err = admin.SendMessage(ctx, gm)
	wantErr(t, err, rendezvous.ErrForbidden) // admin is not a reader

	// removal: stranger cannot, owner cannot leave, member can leave
	wantErr(t, stranger.RemoveGroupMember(ctx, g.ID, fr.ID), rendezvous.ErrForbidden)
	wantErr(t, owner.RemoveGroupMember(ctx, g.ID, o.ID), rendezvous.ErrForbidden)
	if err := friend.RemoveGroupMember(ctx, g.ID, fr.ID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	ev = nextEvent(t, chF, rendezvous.EvGroupMember)
	if !ev.Deleted || ev.GroupMember.UserID != fr.ID {
		t.Fatalf("leave event %+v", ev)
	}
	if err := add(owner, fr.ID); err != nil {
		t.Fatal(err)
	}
	if err := owner.RemoveGroupMember(ctx, g.ID, fr.ID); err != nil {
		t.Fatalf("owner removes: %v", err)
	}
	wantErr(t, owner.RemoveGroupMember(ctx, g.ID, fr.ID), rendezvous.ErrNotFound)

	// transfer and delete
	wantErr(t, owner.TransferGroup(ctx, g.ID, fr.ID), rendezvous.ErrForbidden) // not a member
	add(owner, fr.ID)
	wantErr(t, friend.TransferGroup(ctx, g.ID, fr.ID), rendezvous.ErrForbidden)
	if err := admin.TransferGroup(ctx, g.ID, fr.ID); err != nil {
		t.Fatalf("admin transfer: %v", err)
	}
	wantErr(t, owner.DeleteGroup(ctx, g.ID), rendezvous.ErrForbidden) // no longer owner
	if err := admin.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatalf("admin dissolve: %v", err)
	}
	ev = nextEvent(t, chF, rendezvous.EvGroup)
	for !ev.Deleted {
		ev = nextEvent(t, chF, rendezvous.EvGroup)
	}
	if _, err := friend.ListGroupMembers(ctx, g.ID); !errors.Is(err, rendezvous.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestGroupKeyRotation(t *testing.T) {
	f := newFixture(t)
	f.user("sujay03")
	owner, o := f.user("owner")
	friend, fr := f.user("friend")
	f.befriend(owner, friend)
	g, _ := owner.CreateGroup(ctx, "team", []byte("k1-owner"))
	owner.AddGroupMember(ctx, model.GroupMember{GroupID: g.ID, UserID: fr.ID, WrappedKey: []byte("k1-friend")})
	chF := connect(t, friend)

	wrap := func(ids ...string) []model.GroupMember {
		var ms []model.GroupMember
		for _, id := range ids {
			ms = append(ms, model.GroupMember{GroupID: g.ID, UserID: id, WrappedKey: []byte("k2-" + id)})
		}
		return ms
	}
	tests := []struct {
		name    string
		by      rendezvous.Backend
		epoch   int
		members []model.GroupMember
		want    error
	}{
		{"not owner", friend, 2, wrap(o.ID, fr.ID), rendezvous.ErrForbidden},
		{"stale epoch", owner, 1, wrap(o.ID, fr.ID), rendezvous.ErrConflict},
		{"missing member", owner, 2, wrap(o.ID), rendezvous.ErrConflict},
		{"non-member", owner, 2, wrap(o.ID, fr.ID, "nope"), rendezvous.ErrForbidden},
		{"unknown group", owner, 2, nil, rendezvous.ErrNotFound},
		{"ok", owner, 2, wrap(o.ID, fr.ID), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gid := g.ID
			if tc.name == "unknown group" {
				gid = "nope"
			}
			wantErr(t, tc.by.RotateGroupKey(ctx, gid, tc.epoch, tc.members), tc.want)
		})
	}
	gs, _ := owner.ListGroups(ctx)
	if gs[0].KeyEpoch != 2 {
		t.Fatalf("epoch %d", gs[0].KeyEpoch)
	}
	members, _ := friend.ListGroupMembers(ctx, g.ID)
	for _, m := range members {
		if m.KeyEpoch != 2 || string(m.WrappedKey) != "k2-"+m.UserID || m.WrappedBy != o.ID {
			t.Fatalf("member after rotation %+v", m)
		}
	}
	ev := nextEvent(t, chF, rendezvous.EvGroup)
	if ev.Group.KeyEpoch != 2 {
		t.Fatalf("group event %+v", ev.Group)
	}
	ev = nextEvent(t, chF, rendezvous.EvGroupMember)
	if ev.GroupMember.KeyEpoch != 2 {
		t.Fatalf("member event %+v", ev.GroupMember)
	}
}

// --- grants ---

func TestGrantRules(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	helper, h := f.user("helper")
	plain, pl := f.user("plain")
	admin.SetUserRole(ctx, h.ID, model.RoleHelper)
	chA := connect(t, admin)

	tests := []struct {
		name    string
		grantee string
		want    error
	}{
		{"to admin", a.ID, nil},
		{"to helper", h.ID, nil},
		{"to plain user", pl.ID, rendezvous.ErrForbidden},
		{"to nobody", "nope", rendezvous.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, err := priya.PutGrant(ctx, model.Grant{Grantor: "spoofed", Grantee: tc.grantee, Scope: model.ScopeBoard, Sudo: model.SudoAsk})
			wantErr(t, err, tc.want)
			if err == nil && (g.Grantor != p.ID || g.ID == "" || g.RevokedAt != nil) {
				t.Fatalf("grant %+v", g)
			}
		})
	}
	ev := nextEvent(t, chA, rendezvous.EvGrant)
	if ev.Grant.Grantor != p.ID || ev.Grant.Grantee != a.ID {
		t.Fatalf("grant event %+v", ev.Grant)
	}

	mine, _ := priya.ListGrants(ctx)
	if len(mine) != 2 {
		t.Fatalf("grantor sees %d grants", len(mine))
	}
	theirs, _ := helper.ListGrants(ctx)
	if len(theirs) != 1 || theirs[0].Grantee != h.ID {
		t.Fatalf("helper sees %+v", theirs)
	}
	none, _ := plain.ListGrants(ctx)
	if len(none) != 0 {
		t.Fatalf("outsider sees %+v", none)
	}

	id := theirs[0].ID
	wantErr(t, helper.RevokeGrant(ctx, id), rendezvous.ErrForbidden) // grantee cannot
	wantErr(t, admin.RevokeGrant(ctx, id), rendezvous.ErrForbidden)  // nor the admin
	wantErr(t, priya.RevokeGrant(ctx, "nope"), rendezvous.ErrNotFound)
	if err := priya.RevokeGrant(ctx, id); err != nil {
		t.Fatal(err)
	}
	theirs, _ = helper.ListGrants(ctx)
	if theirs[0].RevokedAt == nil || !theirs[0].RevokedAt.Equal(f.now) {
		t.Fatalf("not revoked: %+v", theirs[0])
	}
	// Replacing someone else's row by id is refused.
	_, err := plain.PutGrant(ctx, model.Grant{ID: id, Grantee: a.ID})
	wantErr(t, err, rendezvous.ErrForbidden)
}

// --- admin ---

func TestAdminOnly(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	user, u := f.user("priya")
	admin.RegisterAdminDevice(ctx, "dev-1", "pk")
	exp := f.now.Add(time.Hour)

	calls := []struct {
		name string
		call func(c rendezvous.Backend) error
	}{
		{"ListUsers", func(c rendezvous.Backend) error { _, err := c.ListUsers(ctx); return err }},
		{"SetSetting", func(c rendezvous.Backend) error { return c.SetSetting(ctx, "announcement", "hi") }},
		{"CreateInvite", func(c rendezvous.Backend) error { _, err := c.CreateInvite(ctx, 1, &exp); return err }},
		{"ListInvites", func(c rendezvous.Backend) error { _, err := c.ListInvites(ctx); return err }},
		{"SetUserFlag", func(c rendezvous.Backend) error { return c.SetUserFlag(ctx, u.ID, "muted", false) }},
		{"SetUserRole", func(c rendezvous.Backend) error { return c.SetUserRole(ctx, u.ID, model.RoleHelper) }},
		{"RenameUser", func(c rendezvous.Backend) error { return c.RenameUser(ctx, u.ID, "priya2") }},
		{"ResetUserKey", func(c rendezvous.Backend) error { return c.ResetUserKey(ctx, u.ID) }},
		{"ListAdminLog", func(c rendezvous.Backend) error { _, err := c.ListAdminLog(ctx, 10); return err }},
		{"Stats", func(c rendezvous.Backend) error { _, err := c.Stats(ctx); return err }},
		{"ListAdminDevices", func(c rendezvous.Backend) error { _, err := c.ListAdminDevices(ctx); return err }},
		{"RegisterAdminDevice", func(c rendezvous.Backend) error { return c.RegisterAdminDevice(ctx, "dev-2", "pk2") }},
		{"ApproveAdminDevice", func(c rendezvous.Backend) error { return c.ApproveAdminDevice(ctx, "dev-2") }},
		{"KickUser", func(c rendezvous.Backend) error { return c.KickUser(ctx, u.ID) }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, tc.call(user), rendezvous.ErrForbidden)
			if err := tc.call(admin); err != nil {
				t.Fatalf("as admin: %v", err)
			}
		})
	}
	log, err := admin.ListAdminLog(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	// One entry per /admin call above except ListUsers, which is a plain
	// read; +1 for the device registered in setup, +1 for this listing.
	if want := len(calls) - 1 + 2; len(log) != want {
		t.Fatalf("admin log has %d entries, want %d", len(log), want)
	}
	if log[0].Action != "log.list" || log[len(log)-1].Action != "device.register" {
		t.Fatalf("log order: first=%s last=%s", log[0].Action, log[len(log)-1].Action)
	}
	tail, _ := admin.ListAdminLog(ctx, 3)
	if len(tail) != 3 {
		t.Fatalf("limit ignored: %d", len(tail))
	}
	wantErr(t, admin.RevokeInvite(ctx, "nope"), rendezvous.ErrNotFound)
	wantErr(t, admin.SetUserRole(ctx, u.ID, model.RoleAdmin), rendezvous.ErrForbidden) // no elevation
	wantErr(t, admin.RenameUser(ctx, u.ID, "sujay03"), rendezvous.ErrUsernameTaken)
	if err := admin.SetUserFlag(ctx, u.ID, "bogus", true); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := admin.SetSetting(ctx, "bogus", 1); err == nil {
		t.Fatal("unknown setting accepted")
	}
	if err := admin.SetSetting(ctx, "user_cap", "ten"); err == nil {
		t.Fatal("mistyped setting accepted")
	}
}

func TestAdminDevices(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	if err := admin.RegisterAdminDevice(ctx, "laptop", "pk1"); err != nil {
		t.Fatal(err)
	}
	if err := admin.RegisterAdminDevice(ctx, "desktop", "pk2"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, admin.RegisterAdminDevice(ctx, "desktop", "pk2"), rendezvous.ErrConflict)
	devs, _ := admin.ListAdminDevices(ctx)
	byID := map[string]model.AdminDevice{}
	for _, d := range devs {
		byID[d.DeviceID] = d
	}
	if !byID["laptop"].Approved || byID["desktop"].Approved {
		t.Fatalf("first device auto-approved only: %+v", devs)
	}
	wantErr(t, admin.ApproveAdminDevice(ctx, "nope"), rendezvous.ErrNotFound)
	if err := admin.ApproveAdminDevice(ctx, "desktop"); err != nil {
		t.Fatal(err)
	}
	devs, _ = admin.ListAdminDevices(ctx)
	for _, d := range devs {
		if !d.Approved || d.ApprovedAt.IsZero() {
			t.Fatalf("not approved: %+v", d)
		}
	}
}

func TestUserFlagEventsAndKick(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	priya, p := f.user("priya")
	chP := connect(t, priya)

	admin.SetUserFlag(ctx, p.ID, "muted", true)
	ev := nextEvent(t, chP, rendezvous.EvUser)
	if !ev.User.Muted {
		t.Fatalf("user event %+v", ev.User)
	}
	admin.RenameUser(ctx, p.ID, "Priya_K")
	ev = nextEvent(t, chP, rendezvous.EvUser)
	if ev.User.Username != "priya_k" {
		t.Fatalf("renamed to %q", ev.User.Username)
	}
	admin.ResetUserKey(ctx, p.ID)
	ev = nextEvent(t, chP, rendezvous.EvUser)
	if ev.User.PubKey != "" {
		t.Fatal("key not reset")
	}

	refresh := priya.Session().RefreshToken
	if err := admin.KickUser(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, chP, rendezvous.EvDisconnected)
	if priya.Session() != nil {
		t.Fatal("kicked client still has a session")
	}
	_, err := f.store.Client().Restore(ctx, refresh)
	wantErr(t, err, rendezvous.ErrUnauthorized)
}

func TestUpdatePubKey(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	priya, _ := f.user("priya")
	chP := connect(t, priya)
	if err := priya.UpdatePubKey(ctx, "pk-new"); err != nil {
		t.Fatal(err)
	}
	me, _ := priya.Me(ctx)
	if me.PubKey != "pk-new" {
		t.Fatalf("pubkey %q", me.PubKey)
	}
	if err := admin.UpdatePubKey(ctx, "pk-admin-2"); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, chP, rendezvous.EvSettings)
	if ev.Settings.AdminPubKey != "pk-admin-2" {
		t.Fatalf("settings event %+v", ev.Settings)
	}
}

func TestSessionLog(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	other, _ := f.user("other")
	l := model.SessionLog{ID: "s1", Visitor: a.ID, Host: p.ID, Kind: model.SessionBoard, Via: model.ViaCard}
	if err := admin.LogSession(ctx, l); err != nil {
		t.Fatal(err)
	}
	wantErr(t, other.EndSession(ctx, "s1"), rendezvous.ErrForbidden)
	wantErr(t, other.LogSession(ctx, l), rendezvous.ErrForbidden)
	wantErr(t, priya.EndSession(ctx, "nope"), rendezvous.ErrNotFound)
	if err := priya.EndSession(ctx, "s1"); err != nil {
		t.Fatalf("host ends: %v", err)
	}
	st, _ := admin.Stats(ctx)
	if st.SessionsWeek != 1 || st.UsersTotal != 3 {
		t.Fatalf("stats %+v", st)
	}
}

// --- realtime ---

func TestPresence(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	priya, p := f.user("priya")
	arjun, a := f.user("arjun")
	stranger, s := f.user("stranger")
	f.befriend(priya, arjun)

	wantErr(t, priya.WatchPresence(ctx, []string{a.ID}), rendezvous.ErrNotConnected)
	chP := connect(t, priya)
	drain(chP)
	arjun.SetOnline(ctx, true)

	// snapshot for the friend; stranger silently ignored
	if err := priya.WatchPresence(ctx, []string{a.ID, s.ID, "nope"}); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, chP, rendezvous.EvPresence)
	if ev.UserID != a.ID || !ev.Online {
		t.Fatalf("snapshot %+v", ev)
	}
	noEvent(t, chP)

	stranger.SetOnline(ctx, true)
	noEvent(t, chP)
	arjun.SetOnline(ctx, false)
	ev = nextEvent(t, chP, rendezvous.EvPresence)
	if ev.UserID != a.ID || ev.Online {
		t.Fatalf("change %+v", ev)
	}

	// the admin may watch anyone
	chA := connect(t, admin)
	drain(chA)
	admin.WatchPresence(ctx, []string{s.ID, p.ID})
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		ev := nextEvent(t, chA, rendezvous.EvPresence)
		seen[ev.UserID] = ev.Online
	}
	if !seen[s.ID] || seen[p.ID] {
		t.Fatalf("admin snapshot %+v", seen)
	}
	st, _ := admin.Stats(ctx)
	if st.UsersOnline != 1 {
		t.Fatalf("online count %d", st.UsersOnline)
	}
}

func TestSignal(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	other, _ := f.user("other")
	chP, chO := connect(t, priya), connect(t, other)

	sig := rendezvous.Signal{SessionID: "sess", From: "spoofed", To: p.ID, Type: "offer", Payload: []byte("blob")}
	if err := admin.SendSignal(ctx, sig); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, chP, rendezvous.EvSignal)
	if ev.Signal.From != a.ID || ev.Signal.Type != "offer" || string(ev.Signal.Payload) != "blob" {
		t.Fatalf("signal %+v", ev.Signal)
	}
	noEvent(t, chO)
	wantErr(t, admin.SendSignal(ctx, rendezvous.Signal{To: "nope"}), rendezvous.ErrNotFound)
}

func TestSettingsBroadcastAndClose(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	priya, _ := f.user("priya")
	chP := connect(t, priya)
	again, _ := priya.Connect(ctx)
	if again != chP {
		t.Fatal("Connect twice should return the same channel")
	}
	admin.SetSetting(ctx, "announcement", "hello")
	ev := nextEvent(t, chP, rendezvous.EvSettings)
	if ev.Settings.Announcement != "hello" {
		t.Fatalf("settings %+v", ev.Settings)
	}
	if err := priya.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-chP; ok {
		t.Fatal("channel not closed")
	}
	// After Close the store must not try to deliver to this client.
	if err := admin.SetSetting(ctx, "announcement", "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := priya.Me(ctx); err != nil {
		t.Fatalf("client unusable after Close: %v", err)
	}
}

func TestFullChannelDropsEvents(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	priya, _ := f.user("priya")
	chP := connect(t, priya)
	done := make(chan struct{})
	go func() {
		for i := 0; i < eventBuffer+50; i++ {
			admin.SetSetting(ctx, "announcement", fmt.Sprint(i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("store blocked on a full channel")
	}
	if n := len(chP); n != eventBuffer {
		t.Fatalf("buffered %d events, want %d", n, eventBuffer)
	}
}

func TestUnconnectedClientGetsNoEvents(t *testing.T) {
	f := newFixture(t)
	admin, _ := f.user("sujay03")
	priya, _ := f.user("priya")
	if err := admin.SetSetting(ctx, "announcement", "x"); err != nil {
		t.Fatal(err)
	}
	ch := connect(t, priya)
	noEvent(t, ch)
}

// --- banned and maintenance ---

func TestBanned(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	priya.SetOnline(ctx, true)
	chA := connect(t, admin)
	admin.WatchPresence(ctx, []string{p.ID})
	drain(chA)

	if err := admin.SetUserFlag(ctx, p.ID, "banned", true); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, chA, rendezvous.EvPresence)
	if ev.Online {
		t.Fatal("banned user still online")
	}

	// The kicked client must sign in again, and then every call is ErrBanned.
	priya = f.login("priya@example.test")
	calls := map[string]func() error{
		"Me":                func() error { _, err := priya.Me(ctx); return err },
		"Settings":          func() error { _, err := priya.Settings(ctx); return err },
		"UserByName":        func() error { _, err := priya.UserByName(ctx, "sujay03"); return err },
		"SendFriendRequest": func() error { return priya.SendFriendRequest(ctx, a.ID) },
		"SendMessage":       func() error { _, err := priya.SendMessage(ctx, dm(priya, a.ID)); return err },
		"ListGroups":        func() error { _, err := priya.ListGroups(ctx); return err },
		"ListGrants":        func() error { _, err := priya.ListGrants(ctx); return err },
		"Connect":           func() error { _, err := priya.Connect(ctx); return err },
		"SetOnline":         func() error { return priya.SetOnline(ctx, true) },
		"UpdatePubKey":      func() error { return priya.UpdatePubKey(ctx, "x") },
		"Register":          func() error { _, err := priya.Register(ctx, "fresh", "pk", ""); return err },
		"ListUsers":         func() error { _, err := priya.ListUsers(ctx); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) { wantErr(t, call(), rendezvous.ErrBanned) })
	}
	if err := admin.SetUserFlag(ctx, p.ID, "banned", false); err != nil {
		t.Fatal(err)
	}
	if _, err := priya.Me(ctx); err != nil {
		t.Fatalf("after unban: %v", err)
	}
}

func TestMaintenance(t *testing.T) {
	f := newFixture(t)
	admin, a := f.user("sujay03")
	priya, p := f.user("priya")
	arjun, ar := f.user("arjun")
	f.befriend(priya, arjun)
	g, _ := priya.CreateGroup(ctx, "team", []byte("k"))
	grant, _ := priya.PutGrant(ctx, model.Grant{Grantee: a.ID, Scope: model.ScopeWatch})
	if err := admin.SetSetting(ctx, "maintenance", true); err != nil {
		t.Fatal(err)
	}

	writes := map[string]func() error{
		"SendFriendRequest":    func() error { return priya.SendFriendRequest(ctx, a.ID) },
		"RespondFriendRequest": func() error { return priya.RespondFriendRequest(ctx, a.ID, true) },
		"RemoveFriend":         func() error { return priya.RemoveFriend(ctx, ar.ID) },
		"SendMessage":          func() error { _, err := priya.SendMessage(ctx, dm(priya, ar.ID)); return err },
		"CreateGroup":          func() error { _, err := priya.CreateGroup(ctx, "x", nil); return err },
		"AddGroupMember":       func() error { return priya.AddGroupMember(ctx, model.GroupMember{GroupID: g.ID, UserID: ar.ID}) },
		"RemoveGroupMember":    func() error { return priya.RemoveGroupMember(ctx, g.ID, ar.ID) },
		"RotateGroupKey":       func() error { return priya.RotateGroupKey(ctx, g.ID, 2, nil) },
		"DeleteGroup":          func() error { return priya.DeleteGroup(ctx, g.ID) },
		"TransferGroup":        func() error { return priya.TransferGroup(ctx, g.ID, ar.ID) },
		"PutGrant":             func() error { _, err := priya.PutGrant(ctx, model.Grant{Grantee: a.ID}); return err },
		"RevokeGrant":          func() error { return priya.RevokeGrant(ctx, grant.ID) },
		"LogSession":           func() error { return priya.LogSession(ctx, model.SessionLog{Host: a.ID}) },
		"EndSession":           func() error { return priya.EndSession(ctx, "x") },
		"UpdatePubKey":         func() error { return priya.UpdatePubKey(ctx, "x") },
		"Register":             func() error { _, err := f.login("n@example.test").Register(ctx, "newbie", "pk", ""); return err },
	}
	for name, call := range writes {
		t.Run(name, func(t *testing.T) { wantErr(t, call(), rendezvous.ErrMaintenance) })
	}

	// Reads still work for users; the admin is exempt from everything.
	if _, err := priya.ListMessages(ctx, rendezvous.MessageQuery{ConvType: model.ConvDM, ConvID: model.DMConvID(p.ID, ar.ID)}); err != nil {
		t.Fatalf("read during maintenance: %v", err)
	}
	if _, err := priya.ListFriendships(ctx); err != nil {
		t.Fatalf("read during maintenance: %v", err)
	}
	if _, err := admin.SendMessage(ctx, dm(admin, p.ID)); err != nil {
		t.Fatalf("admin write during maintenance: %v", err)
	}
	if err := admin.SetSetting(ctx, "maintenance", false); err != nil {
		t.Fatal(err)
	}
	if _, err := priya.SendMessage(ctx, dm(priya, ar.ID)); err != nil {
		t.Fatalf("after maintenance: %v", err)
	}
}

// reversedDM builds the DM id with the parties in the wrong order.
func reversedDM(a, b string) string {
	if a < b {
		a, b = b, a
	}
	return a + ":" + b
}
