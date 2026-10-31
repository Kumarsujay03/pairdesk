// Package app is PairDesk's application layer. It owns the backend
// connection, the device identity, key material and the decrypted view of
// conversations, and exposes plain methods the TUI calls. Nothing in here
// renders anything.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/notify"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
	"github.com/Kumarsujay03/pairdesk/internal/search"
	"github.com/Kumarsujay03/pairdesk/internal/store"
)

// Version is set by the linker.
var Version = "0.0.0-dev"

var usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

// Phase is where the user is in the startup flow.
type Phase int

const (
	PhaseBoot Phase = iota
	PhaseNeedLogin
	PhaseNeedOTP
	PhaseRegClosed
	PhaseNeedUsername
	PhasePendingApproval
	PhaseBanned
	PhaseMaintenance
	PhaseUpdateRequired
	PhaseReady
)

// AdminBootstrapper is implemented by backends that need a device token for
// admin calls (Supabase). The memory backend does not.
type AdminBootstrapper interface {
	BootstrapAdminDevice(ctx context.Context, deviceID, pubkey string) (string, error)
	SetAdminToken(string)
}

// Options configure the app.
type Options struct {
	Backend rendezvous.Backend
	Store   *store.Store
	Logger  *slog.Logger
}

// App is the single application object.
type App struct {
	be     rendezvous.Backend
	st     *store.Store
	log    *slog.Logger
	id     *crypto.Identity
	me     model.User
	login  store.Login
	phase  Phase
	events chan Event
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.RWMutex
	users     map[string]model.User     // id -> user (cache)
	byName    map[string]string         // username -> id
	friends   map[string]model.Friendship
	online    map[string]bool
	settings  model.Settings
	convs     map[string]*Conversation  // conv id -> decrypted conversation
	groups    map[string]model.Group
	groupKeys map[string][crypto.KeySize]byte
	pairwise  map[string][crypto.KeySize]byte
	grants    store.Grants
	typing    map[string]time.Time
	pendingOTPEmail string
	signalH   SignalHandler
	search    *search.Index
	notifier  *notify.Notifier
	focused   func(convID string) bool
	onRevoke  func(userID string)
}

// New builds an App. Call Start next.
func New(o Options) *App {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		be: o.Backend, st: o.Store, log: o.Logger,
		events:    make(chan Event, 256),
		ctx:       ctx, cancel: cancel,
		users:     map[string]model.User{},
		byName:    map[string]string{},
		friends:   map[string]model.Friendship{},
		online:    map[string]bool{},
		convs:     map[string]*Conversation{},
		groups:    map[string]model.Group{},
		groupKeys: map[string][crypto.KeySize]byte{},
		pairwise:  map[string][crypto.KeySize]byte{},
		typing:    map[string]time.Time{},
	}
}

// Events is the stream the UI consumes.
func (a *App) Events() <-chan Event { return a.events }

// Emit publishes an event to the UI (used by the session layer).
func (a *App) Emit(e Event) { a.emit(e) }

func (a *App) emit(e Event) {
	select {
	case a.events <- e:
	default:
		a.log.Warn("event dropped", "kind", e.Kind)
	}
}

// Phase reports the startup phase.
func (a *App) Phase() Phase { a.mu.RLock(); defer a.mu.RUnlock(); return a.phase }
func (a *App) setPhase(p Phase) {
	a.mu.Lock()
	changed := a.phase != p
	a.phase = p
	a.mu.Unlock()
	if changed {
		a.emit(Event{Kind: EvPhase, Phase: p})
	}
}

// Me returns the current user row.
func (a *App) Me() model.User { a.mu.RLock(); defer a.mu.RUnlock(); return a.me }

// IsAdmin reports whether the current user is the admin.
func (a *App) IsAdmin() bool { return a.Me().Role == model.RoleAdmin }

// IsHelper reports helper or admin.
func (a *App) IsHelperOrAdmin() bool {
	r := a.Me().Role
	return r == model.RoleAdmin || r == model.RoleHelper
}

// Logger exposes the app logger.
func (a *App) Logger() *slog.Logger { return a.log }

// Identity exposes the device identity to the session layer.
func (a *App) Identity() *crypto.Identity { return a.id }

// Backend exposes the backend to the session layer.
func (a *App) Backend() rendezvous.Backend { return a.be }

// Store exposes the local store.
func (a *App) Store() *store.Store { return a.st }

// Settings returns the cached instance settings.
func (a *App) Settings() model.Settings { a.mu.RLock(); defer a.mu.RUnlock(); return a.settings }

// Start restores state from disk and decides the phase. It never blocks on
// the network for long; failures fall back to PhaseNeedLogin.
func (a *App) Start(ctx context.Context) error {
	id, err := a.st.Identity()
	if errors.Is(err, store.ErrNoIdentity) {
		id, err = a.st.CreateIdentity()
		if err == nil {
			a.emit(Event{Kind: EvKeyForged, Text: id.Public().Phrase()})
		}
	}
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	a.id = id
	a.initExtras()

	if s, err := a.be.Settings(ctx); err == nil {
		a.mu.Lock()
		a.settings = s
		a.mu.Unlock()
	}

	login, err := a.st.Login()
	if err != nil || login.RefreshToken == "" {
		a.setPhase(PhaseNeedLogin)
		return nil
	}
	a.login = login
	if ab, ok := a.be.(AdminBootstrapper); ok && login.AdminToken != "" {
		ab.SetAdminToken(login.AdminToken)
	}
	sess, err := a.be.Restore(ctx, login.RefreshToken)
	if err != nil {
		a.log.Info("restore failed, need login", "err", err)
		a.setPhase(PhaseNeedLogin)
		return nil
	}
	a.login.RefreshToken = sess.RefreshToken
	_ = a.st.SaveLogin(a.login)
	return a.afterAuth(ctx)
}

// RequestLogin sends a magic link / OTP to the email.
func (a *App) RequestLogin(ctx context.Context, email string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	if !strings.Contains(email, "@") {
		return errors.New("that does not look like an email")
	}
	if err := a.be.RequestMagicLink(ctx, email); err != nil {
		return err
	}
	a.mu.Lock()
	a.pendingOTPEmail = email
	a.mu.Unlock()
	a.setPhase(PhaseNeedOTP)
	return nil
}

// VerifyOTP completes login.
func (a *App) VerifyOTP(ctx context.Context, code string) error {
	a.mu.RLock()
	email := a.pendingOTPEmail
	a.mu.RUnlock()
	sess, err := a.be.VerifyOTP(ctx, email, strings.TrimSpace(code))
	if err != nil {
		return err
	}
	a.login = store.Login{Email: email, UserID: sess.UserID, RefreshToken: sess.RefreshToken, DeviceID: a.deviceID()}
	if err := a.st.SaveLogin(a.login); err != nil {
		return err
	}
	return a.afterAuth(ctx)
}

func (a *App) deviceID() string {
	if a.login.DeviceID != "" {
		return a.login.DeviceID
	}
	fp := a.id.Public().Fingerprint()
	return fmt.Sprintf("%x", fp[:8])
}

// afterAuth figures out whether the account has a username yet.
func (a *App) afterAuth(ctx context.Context) error {
	me, err := a.be.Me(ctx)
	switch {
	case errors.Is(err, rendezvous.ErrNotFound):
		s, serr := a.be.Settings(ctx)
		if serr == nil {
			a.mu.Lock()
			a.settings = s
			a.mu.Unlock()
			if s.Registration == model.RegClosed {
				a.setPhase(PhaseRegClosed)
				return nil
			}
		}
		a.setPhase(PhaseNeedUsername)
		return nil
	case err != nil:
		return err
	}
	return a.becomeReady(ctx, me)
}

// CheckUsername validates locally then asks the server.
func (a *App) CheckUsername(ctx context.Context, name string) (bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !usernameRe.MatchString(name) {
		return false, nil
	}
	return a.be.CheckUsername(ctx, name)
}

// Register claims a username.
func (a *App) Register(ctx context.Context, name, invite string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if !usernameRe.MatchString(name) {
		return errors.New("username must be 3-20 chars of a-z, 0-9 or _")
	}
	me, err := a.be.Register(ctx, name, a.id.Public().String(), strings.TrimSpace(invite))
	if err != nil {
		return err
	}
	return a.becomeReady(ctx, me)
}

func (a *App) becomeReady(ctx context.Context, me model.User) error {
	a.mu.Lock()
	a.me = me
	a.users[me.ID] = me
	a.byName[me.Username] = me.ID
	a.login.UserID = me.ID
	a.login.Username = me.Username
	a.mu.Unlock()
	_ = a.st.SaveLogin(a.login)

	if me.Banned {
		a.setPhase(PhaseBanned)
		return nil
	}
	if !me.Approved {
		a.setPhase(PhasePendingApproval)
		go a.waitForApproval()
		return nil
	}
	if me.PubKey == "" || me.PubKey != a.id.Public().String() {
		// Fresh device or admin reset-key: publish our key.
		if err := a.be.UpdatePubKey(ctx, a.id.Public().String()); err != nil {
			return fmt.Errorf("publish key: %w", err)
		}
		a.mu.Lock()
		a.me.PubKey = a.id.Public().String()
		a.mu.Unlock()
	}
	if me.Role == model.RoleAdmin {
		if err := a.ensureAdminDevice(ctx); err != nil {
			a.log.Warn("admin device", "err", err)
		}
	}
	s, err := a.be.Settings(ctx)
	if err == nil {
		a.mu.Lock()
		a.settings = s
		a.mu.Unlock()
		if s.Maintenance && me.Role != model.RoleAdmin {
			a.setPhase(PhaseMaintenance)
			return nil
		}
		if versionLess(Version, s.MinVersion) {
			a.setPhase(PhaseUpdateRequired)
			return nil
		}
	}
	if err := a.loadAll(ctx); err != nil {
		return err
	}
	if err := a.connect(ctx); err != nil {
		return err
	}
	a.setPhase(PhaseReady)
	return nil
}

func (a *App) waitForApproval() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			me, err := a.be.Me(a.ctx)
			if err == nil && me.Approved {
				_ = a.becomeReady(a.ctx, me)
				return
			}
			if err == nil && me.Banned {
				a.setPhase(PhaseBanned)
				return
			}
		}
	}
}

// ensureAdminDevice bootstraps the admin device token on first admin run,
// or registers this device for approval if another admin device exists.
func (a *App) ensureAdminDevice(ctx context.Context) error {
	if a.login.AdminToken != "" {
		return nil
	}
	ab, ok := a.be.(AdminBootstrapper)
	if !ok {
		return a.be.RegisterAdminDevice(ctx, a.deviceID(), a.id.Public().String())
	}
	tok, err := ab.BootstrapAdminDevice(ctx, a.deviceID(), a.id.Public().String())
	if err != nil {
		// Another device already holds the admin token: register for approval.
		if rerr := a.be.RegisterAdminDevice(ctx, a.deviceID(), a.id.Public().String()); rerr != nil && !errors.Is(rerr, rendezvous.ErrConflict) {
			return rerr
		}
		a.emit(Event{Kind: EvNotice, Text: "this device is not yet an approved admin device; approve it from an existing one with /admin devices"})
		return nil
	}
	a.login.AdminToken = tok
	ab.SetAdminToken(tok)
	return a.st.SaveLogin(a.login)
}

// loadAll pulls friends, users, groups, grants and recent messages.
func (a *App) loadAll(ctx context.Context) error {
	fs, err := a.be.ListFriendships(ctx)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	a.mu.Lock()
	for _, f := range fs {
		a.friends[pairKey(f.Requester, f.Addressee)] = f
		ids[f.Other(a.me.ID)] = true
	}
	a.mu.Unlock()
	gs, err := a.be.ListGroups(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		a.mu.Lock()
		a.groups[g.ID] = g
		a.mu.Unlock()
		if err := a.loadGroupKey(ctx, g); err != nil {
			a.log.Warn("group key", "group", g.Name, "err", err)
		}
	}
	grants, _ := a.st.Grants()
	a.mu.Lock()
	a.grants = grants
	a.mu.Unlock()
	if err := a.cacheUsers(ctx, keys(ids)); err != nil {
		return err
	}
	for id := range ids {
		if f := a.friendship(id); f != nil && f.Status == model.FriendAccepted {
			a.ensureConv(model.ConvDM, model.DMConvID(a.me.ID, id))
		}
	}
	for _, g := range gs {
		a.ensureConv(model.ConvGroup, g.ID)
	}
	return nil
}

func (a *App) connect(ctx context.Context) error {
	ch, err := a.be.Connect(ctx)
	if err != nil {
		return err
	}
	go a.pump(ch)
	_ = a.be.SetOnline(ctx, true)
	var watch []string
	for _, f := range a.Friends() {
		watch = append(watch, f.User.ID)
	}
	if a.IsAdmin() {
		if all, err := a.be.ListUsers(ctx); err == nil {
			a.mu.Lock()
			for _, u := range all {
				a.users[u.ID] = u
				a.byName[u.Username] = u.ID
			}
			a.mu.Unlock()
			watch = watch[:0]
			for _, u := range all {
				if u.ID != a.me.ID {
					watch = append(watch, u.ID)
				}
			}
		}
	}
	if len(watch) > 0 {
		_ = a.be.WatchPresence(ctx, watch)
	}
	go a.heartbeat()
	return nil
}

func (a *App) heartbeat() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			if h, ok := a.be.(interface{ TouchLastSeen(context.Context) error }); ok {
				_ = h.TouchLastSeen(a.ctx)
			}
		}
	}
}

// Close signs out of presence and stops everything.
func (a *App) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.be.SetOnline(ctx, false)
	a.cancel()
	_ = a.be.Close()
}

// SignOut forgets the login but keeps the identity.
func (a *App) SignOut(ctx context.Context) error {
	_ = a.be.SignOut(ctx)
	return a.st.ClearLogin()
}

// --- users ---------------------------------------------------------------

func (a *App) cacheUsers(ctx context.Context, ids []string) error {
	var missing []string
	a.mu.RLock()
	for _, id := range ids {
		if _, ok := a.users[id]; !ok {
			missing = append(missing, id)
		}
	}
	a.mu.RUnlock()
	if len(missing) == 0 {
		return nil
	}
	us, err := a.be.UsersByIDs(ctx, missing)
	if err != nil {
		return err
	}
	a.mu.Lock()
	for _, u := range us {
		a.users[u.ID] = u
		a.byName[u.Username] = u.ID
	}
	a.mu.Unlock()
	for _, u := range us {
		a.pinTOFU(u)
	}
	return nil
}

// pinTOFU records a user's key on first sight and warns on change.
func (a *App) pinTOFU(u model.User) {
	if u.PubKey == "" || u.ID == a.me.ID {
		return
	}
	err := a.st.Pin(store.Pin{UserID: u.ID, Username: u.Username, PubKey: u.PubKey})
	if errors.Is(err, store.ErrKeyChanged) {
		a.emit(Event{Kind: EvKeyChanged, UserID: u.ID, Text: u.Username})
		a.suspendGrantsFor(u.ID)
	}
}

// User looks a user up by id, from cache or the backend.
func (a *App) User(ctx context.Context, id string) (model.User, error) {
	a.mu.RLock()
	u, ok := a.users[id]
	a.mu.RUnlock()
	if ok {
		return u, nil
	}
	u, err := a.be.UserByID(ctx, id)
	if err != nil {
		return u, err
	}
	a.mu.Lock()
	a.users[u.ID] = u
	a.byName[u.Username] = u.ID
	a.mu.Unlock()
	a.pinTOFU(u)
	return u, nil
}

// UserByName resolves a username (exact match only).
func (a *App) UserByName(ctx context.Context, name string) (model.User, error) {
	name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "@"))
	a.mu.RLock()
	id, ok := a.byName[name]
	u := a.users[id]
	a.mu.RUnlock()
	if ok {
		return u, nil
	}
	u, err := a.be.UserByName(ctx, name)
	if err != nil {
		return u, err
	}
	a.mu.Lock()
	a.users[u.ID] = u
	a.byName[u.Username] = u.ID
	a.mu.Unlock()
	a.pinTOFU(u)
	return u, nil
}

// Name renders a user id as a display name, honouring the badge rule.
func (a *App) Name(id string) string {
	a.mu.RLock()
	u, ok := a.users[id]
	me := a.me
	settings := a.settings
	a.mu.RUnlock()
	if !ok {
		return id[:min(8, len(id))]
	}
	name := u.Username
	if u.Role == model.RoleAdmin && id != me.ID && settings.AdminPubKey == u.PubKey && a.adminBadgeVisible() {
		name += " [admin]"
	}
	if u.Role == model.RoleHelper {
		name += " [helper]"
	}
	return name
}

func (a *App) adminBadgeVisible() bool { return a.st.Prefs().ShowBadge }

// Online reports presence.
func (a *App) Online(id string) bool { a.mu.RLock(); defer a.mu.RUnlock(); return a.online[id] }

// --- friends -------------------------------------------------------------

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func (a *App) friendship(other string) *model.Friendship {
	a.mu.RLock()
	defer a.mu.RUnlock()
	f, ok := a.friends[pairKey(a.me.ID, other)]
	if !ok {
		return nil
	}
	return &f
}

// IsFriend reports an accepted friendship.
func (a *App) IsFriend(other string) bool {
	f := a.friendship(other)
	return f != nil && f.Status == model.FriendAccepted
}

// Friend is a friend with presence.
type Friend struct {
	User   model.User
	Online bool
}

// Friends lists accepted friends sorted online-first then by name.
func (a *App) Friends() []Friend {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []Friend
	for _, f := range a.friends {
		if f.Status != model.FriendAccepted {
			continue
		}
		id := f.Other(a.me.ID)
		u, ok := a.users[id]
		if !ok {
			continue
		}
		out = append(out, Friend{User: u, Online: a.online[id]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].User.Username < out[j].User.Username
	})
	return out
}

// PendingRequests lists incoming friend requests.
func (a *App) PendingRequests() []model.User {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []model.User
	for _, f := range a.friends {
		if f.Status == model.FriendPending && f.Addressee == a.me.ID {
			if u, ok := a.users[f.Requester]; ok {
				out = append(out, u)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// AddFriend sends a request by username.
func (a *App) AddFriend(ctx context.Context, name string) error {
	u, err := a.UserByName(ctx, name)
	if errors.Is(err, rendezvous.ErrNotFound) {
		return fmt.Errorf("no user named %q", name)
	}
	if err != nil {
		return err
	}
	if u.ID == a.me.ID {
		return errors.New("that is you")
	}
	if a.IsFriend(u.ID) {
		return fmt.Errorf("%s is already your friend", u.Username)
	}
	if err := a.be.SendFriendRequest(ctx, u.ID); err != nil {
		if errors.Is(err, rendezvous.ErrConflict) {
			return fmt.Errorf("you were denied recently; try again later")
		}
		return err
	}
	return nil
}

// RespondFriend accepts or denies a request from a username.
func (a *App) RespondFriend(ctx context.Context, name string, accept bool) error {
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if err := a.be.RespondFriendRequest(ctx, u.ID, accept); err != nil {
		return err
	}
	return nil
}

// RemoveFriend unfriends.
func (a *App) RemoveFriend(ctx context.Context, name string) error {
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	return a.be.RemoveFriend(ctx, u.ID)
}

// --- helpers --------------------------------------------------------------

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// versionLess compares dotted versions; dev builds never require update.
func versionLess(have, min string) bool {
	if strings.Contains(have, "dev") || min == "" || min == "0.0.0" {
		return false
	}
	parse := func(s string) [3]int {
		var v [3]int
		fmt.Sscanf(strings.TrimPrefix(s, "v"), "%d.%d.%d", &v[0], &v[1], &v[2])
		return v
	}
	h, m := parse(have), parse(min)
	for i := range h {
		if h[i] != m[i] {
			return h[i] < m[i]
		}
	}
	return false
}
