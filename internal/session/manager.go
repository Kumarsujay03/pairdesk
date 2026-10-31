// Package session orchestrates remote sessions end to end: consent cards,
// standing grants, the authenticated P2P link, PTY hosting, screen share,
// sudo policy, kill switch, timeouts and the host-owned session log.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/admin"
	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/p2p"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
	"github.com/Kumarsujay03/pairdesk/internal/sessionlog"
	"github.com/Kumarsujay03/pairdesk/internal/store"
	"github.com/Kumarsujay03/pairdesk/internal/term"
)

const (
	cardTimeout    = 60 * time.Second
	replyTimeout   = 75 * time.Second
	controlExpiry  = 30 * time.Minute
	connectTimeout = 45 * time.Second
)

// sessionIDRe is the only shape a session id may have; ids are used in
// file paths and map keys, so anything else from the wire is dropped.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16}$`)

var DefaultSTUN = []string{"stun:stun.l.google.com:19302", "stun:stun.cloudflare.com:3478"}

var (
	ErrNotAllowed = errors.New("only the admin or a helper can request access")
	ErrNotFriend  = errors.New("they must be your friend first")
	ErrDenied     = errors.New("request denied")
	ErrTimeout    = errors.New("no answer in time")
	ErrBusy       = errors.New("a session is already running")
)

// TermUI hands the real terminal to a function (the TUI releases and
// restores it around the call).
type TermUI func(fn func(stdin io.Reader, stdout io.Writer) error) error

// Manager runs sessions for one PairDesk instance.
type Manager struct {
	app    *app.App
	lock   *admin.Lock
	log    *slog.Logger
	TermUI TermUI
	STUN   []string

	mu       sync.Mutex
	sessions map[string]*Session
	sigs     map[string]chan rendezvous.Signal
	pending  map[string]chan cardReply // visitor side waits here
	seedMap  map[string]seedEntry
	inflight map[string]int
}

// Session is one live or finished session.
type Session struct {
	ID       string
	Kind     model.SessionKind
	Control  bool
	Peer     model.User
	Host     bool
	Via      model.SessionVia
	Sudo     model.SudoTier
	Started  time.Time
	State    string
	conn     *p2p.Conn
	host     *term.Host
	writer   *sessionlog.Writer
	cancel   context.CancelFunc
	stopOnce sync.Once
	reason   string
	smu      sync.Mutex
}

func (s *Session) setState(st string) { s.smu.Lock(); s.State = st; s.smu.Unlock() }
func (s *Session) getState() string   { s.smu.Lock(); defer s.smu.Unlock(); return s.State }
func (s *Session) setReason(r string) { s.smu.Lock(); s.reason = r; s.smu.Unlock() }
func (s *Session) getReason() string  { s.smu.Lock(); defer s.smu.Unlock(); return s.reason }

// New wires the manager to the app's signaling.
func New(a *app.App, lock *admin.Lock, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{app: a, lock: lock, log: log, STUN: DefaultSTUN, sessions: map[string]*Session{}, sigs: map[string]chan rendezvous.Signal{}, pending: map[string]chan cardReply{}}
	a.SetSignalHandler(m.onSignal)
	a.SetRevokeHook(m.onRevoke)
	return m
}

// --- signaling plumbing ------------------------------------------------------

type signaler struct {
	m  *Manager
	id string
	ch chan rendezvous.Signal
}

func (s *signaler) Send(ctx context.Context, sig rendezvous.Signal) error {
	return s.m.app.Backend().SendSignal(ctx, sig)
}
func (s *signaler) Recv() <-chan rendezvous.Signal { return s.ch }

func (m *Manager) signalerFor(id string) *signaler {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.sigs[id]
	if !ok {
		ch = make(chan rendezvous.Signal, 64)
		m.sigs[id] = ch
	}
	return &signaler{m: m, id: id, ch: ch}
}

func (m *Manager) dropSignaler(id string) {
	m.mu.Lock()
	delete(m.sigs, id)
	delete(m.pending, id)
	m.mu.Unlock()
}

func (m *Manager) onSignal(sig rendezvous.Signal) {
	if !sessionIDRe.MatchString(sig.SessionID) || sig.From == "" {
		m.log.Warn("signal with bad session id dropped", "from", sig.From)
		return
	}
	switch sig.Type {
	case "card":
		m.mu.Lock()
		if m.inflight == nil {
			m.inflight = map[string]int{}
		}
		if m.inflight[sig.From] >= 3 {
			m.mu.Unlock()
			m.log.Warn("too many pending cards from peer", "from", sig.From)
			return
		}
		m.inflight[sig.From]++
		m.mu.Unlock()
		go func() {
			defer func() {
				m.mu.Lock()
				m.inflight[sig.From]--
				m.mu.Unlock()
			}()
			m.onCard(sig)
		}()
		return
	case "card_reply":
		m.mu.Lock()
		ch := m.pending[sig.SessionID]
		m.mu.Unlock()
		if ch != nil {
			var r cardReply
			if pt, err := m.open(sig.From, sig.SessionID, "card_reply", sig.Payload); err == nil && json.Unmarshal(pt, &r) == nil {
				select {
				case ch <- r:
				default:
				}
			}
		}
		return
	}
	m.mu.Lock()
	ch := m.sigs[sig.SessionID]
	m.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- sig:
	default:
		m.log.Warn("signal dropped", "session", sig.SessionID, "type", sig.Type)
	}
}

// card and cardReply are sealed with the pairwise key.
type card struct {
	Kind     model.SessionKind `json:"kind"`
	Control  bool              `json:"control"`
	Via      model.SessionVia  `json:"via"`
	Name     string            `json:"name"`
	Manifest json.RawMessage   `json:"manifest,omitempty"`
	At       int64             `json:"at"`
}

type cardReply struct {
	Accept bool             `json:"accept"`
	Sudo   model.SudoTier   `json:"sudo"`
	Via    model.SessionVia `json:"via"`
	Reason string           `json:"reason,omitempty"`
}

func (m *Manager) pairwise(ctx context.Context, peer string) ([crypto.KeySize]byte, crypto.PublicKey, error) {
	u, err := m.app.User(ctx, peer)
	if err != nil {
		return [crypto.KeySize]byte{}, nil, err
	}
	pk, err := crypto.ParsePublicKey(u.PubKey)
	if err != nil {
		return [crypto.KeySize]byte{}, nil, fmt.Errorf("%s has no device key", u.Username)
	}
	k, err := crypto.Pairwise(m.app.Identity(), pk)
	return k, pk, err
}

// cardAAD binds a card to its session, direction and type so the relay
// cannot reflect or re-label it.
func cardAAD(sessionID, from, to, typ string) []byte {
	return []byte("card:" + sessionID + ":" + from + ":" + to + ":" + typ)
}

func (m *Manager) seal(ctx context.Context, peer, sessionID, typ string, v any) ([]byte, error) {
	k, _, err := m.pairwise(ctx, peer)
	if err != nil {
		return nil, err
	}
	pt, _ := json.Marshal(v)
	nonce, ct, err := crypto.Seal(k, cardAAD(sessionID, m.app.Me().ID, peer, typ), pt)
	if err != nil {
		return nil, err
	}
	return append(nonce, ct...), nil
}

func (m *Manager) open(peer, sessionID, typ string, b []byte) ([]byte, error) {
	k, _, err := m.pairwise(context.Background(), peer)
	if err != nil {
		return nil, err
	}
	if len(b) < crypto.NonceSize {
		return nil, crypto.ErrDecrypt
	}
	return crypto.Open(k, cardAAD(sessionID, peer, m.app.Me().ID, typ), b[:crypto.NonceSize], b[crypto.NonceSize:])
}

func (m *Manager) p2pConfig(id string) p2p.Config {
	return p2p.Config{Me: m.app.Identity(), MyID: m.app.Me().ID, STUN: m.STUN, Signaler: m.signalerFor(id), Logger: m.log}
}

// dialWithTimeout bounds connection setup without cancelling the context
// the live connection keeps using afterwards.
func dialWithTimeout(ctx context.Context, f func(context.Context) (*p2p.Conn, error)) (*p2p.Conn, error) {
	dctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(connectTimeout, cancel)
	conn, err := f(dctx)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		<-conn.Done()
		cancel()
	}()
	return conn, nil
}

// --- listing / control -----------------------------------------------------------

// Live returns running sessions.
func (m *Manager) Live() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.getState() != "ended" {
			out = append(out, s)
		}
	}
	return out
}

// StopAll is the host's kill switch (and the visitor's /end).
func (m *Manager) StopAll(reason string) int {
	n := 0
	for _, s := range m.Live() {
		s.end(reason)
		n++
	}
	return n
}

// Stop ends one session.
func (m *Manager) Stop(id, reason string) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil {
		s.end(reason)
	}
}

func (m *Manager) onRevoke(userID string) {
	for _, s := range m.Live() {
		if s.Host && s.Peer.ID == userID {
			s.end("grant revoked")
		}
	}
}

func (s *Session) end(reason string) {
	s.stopOnce.Do(func() {
		s.setReason(reason)
		if s.host != nil {
			_ = s.host.Stop()
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
}

func (m *Manager) track(s *Session) {
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
}

func (m *Manager) finish(s *Session, reason string) {
	if s.reason == "" {
		s.setReason(reason)
	}
	s.setState("ended")
	if s.writer != nil {
		_ = s.writer.Seal(m.app.Identity(), s.getReason())
	}
	m.dropSignaler(s.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.app.Backend().EndSession(ctx, s.ID)
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})
	m.app.Notify("session:"+s.ID, "PairDesk", fmt.Sprintf("%s session with %s ended: %s", s.Kind, s.Peer.Username, s.getReason()), "low")
}

func (m *Manager) info(s *Session) *app.SessionInfo {
	return &app.SessionInfo{ID: s.ID, Kind: s.Kind, Peer: s.Peer.ID, PeerName: s.Peer.Username, Host: s.Host, Via: s.Via, State: s.getState(), Error: s.getReason()}
}

func (m *Manager) logsRoot() string { return filepath.Join(m.app.Store().Paths.State, "sessions") }

func (m *Manager) newWriter(s *Session, role string) (*sessionlog.Writer, error) {
	me := m.app.Me()
	meta := sessionlog.Meta{ID: s.ID, Kind: s.Kind, Via: s.Via, Role: role, SudoTier: s.Sudo, Control: s.Control, StartedAt: s.Started}
	if s.Host {
		meta.Host, meta.HostName, meta.Visitor, meta.VisitorName = me.ID, me.Username, s.Peer.ID, s.Peer.Username
	} else {
		meta.Visitor, meta.VisitorName, meta.Host, meta.HostName = me.ID, me.Username, s.Peer.ID, s.Peer.Username
	}
	return sessionlog.NewWriter(filepath.Join(m.logsRoot(), s.ID), meta)
}

// --- visitor side ------------------------------------------------------------------

// Request asks a friend for a session. It blocks until the session ends.
func (m *Manager) Request(ctx context.Context, name string, kind model.SessionKind, control bool) error {
	if !m.app.IsHelperOrAdmin() {
		return ErrNotAllowed
	}
	if m.lock != nil {
		if err := m.lock.Require(); err != nil {
			return err
		}
	}
	peer, err := m.app.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if !m.app.IsFriend(peer.ID) {
		return ErrNotFriend
	}
	if !m.app.Online(peer.ID) {
		return fmt.Errorf("%s is offline", peer.Username)
	}
	for _, s := range m.Live() {
		if s.Peer.ID == peer.ID {
			return ErrBusy
		}
	}
	id := crypto.RandomID(12)
	s := &Session{ID: id, Kind: kind, Control: control, Peer: peer, Host: false, Via: model.ViaCard, Started: time.Now().UTC(), State: "connecting"}
	scope := model.ScopeWatch
	if kind == model.SessionBoard {
		scope = model.ScopeBoard
	}
	if kind == model.SessionWatch || kind == model.SessionBoard {
		if g, _ := m.app.HeldGrantFrom(ctx, peer.ID, scope); g != nil {
			s.Via = model.ViaGrant
		}
	}
	m.track(s)
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})

	reply := make(chan cardReply, 1)
	m.mu.Lock()
	m.pending[id] = reply
	m.mu.Unlock()
	c := card{Kind: kind, Control: control, Via: s.Via, Name: m.app.Me().Username, At: time.Now().Unix()}
	payload, err := m.seal(ctx, peer.ID, id, "card", c)
	if err != nil {
		m.finish(s, err.Error())
		return err
	}
	if err := m.app.Backend().SendSignal(ctx, rendezvous.Signal{SessionID: id, From: m.app.Me().ID, To: peer.ID, Type: "card", Payload: payload}); err != nil {
		m.finish(s, err.Error())
		return err
	}
	var r cardReply
	select {
	case r = <-reply:
	case <-time.After(replyTimeout):
		m.finish(s, "no answer")
		return ErrTimeout
	case <-ctx.Done():
		m.finish(s, "cancelled")
		return ctx.Err()
	}
	if !r.Accept {
		m.finish(s, "denied")
		if r.Reason != "" {
			return fmt.Errorf("%w: %s", ErrDenied, r.Reason)
		}
		return ErrDenied
	}
	s.Via, s.Sudo = r.Via, r.Sudo

	sctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()
	cfg := m.p2pConfig(id)
	cfg.ReceiveVideo = kind == model.SessionScreen
	_, pk, err := m.pairwise(ctx, peer.ID)
	if err != nil {
		m.finish(s, err.Error())
		return err
	}
	conn, err := dialWithTimeout(sctx, func(dctx context.Context) (*p2p.Conn, error) {
		return p2p.Dial(dctx, cfg, p2p.Peer{ID: peer.ID, PubKey: pk}, id)
	})
	if err != nil {
		m.finish(s, "connect failed: "+err.Error())
		return err
	}
	s.conn = conn
	s.setState("live")
	_ = m.app.Backend().LogSession(ctx, model.SessionLog{ID: id, Visitor: m.app.Me().ID, Host: peer.ID, Kind: kind, Via: s.Via, StartedAt: s.Started})
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})

	var runErr error
	switch kind {
	case model.SessionWatch, model.SessionBoard:
		runErr = m.runVisitorTerminal(sctx, s)
	case model.SessionScreen:
		runErr = m.runVisitorScreen(sctx, s)
	default:
		runErr = fmt.Errorf("unsupported kind %s", kind)
	}
	reason := "ended"
	if runErr != nil && !errors.Is(runErr, term.ErrDetached) && !errors.Is(runErr, io.EOF) {
		reason = runErr.Error()
	} else if errors.Is(runErr, term.ErrDetached) {
		reason = "detached"
	}
	s.end(reason)
	m.finish(s, reason)
	if runErr != nil && reason != "detached" {
		return runErr
	}
	return nil
}

func (m *Manager) runVisitorTerminal(ctx context.Context, s *Session) error {
	if m.TermUI == nil {
		return errors.New("no terminal available")
	}
	ch, err := s.conn.OpenChannel(ctx, "term", true)
	if err != nil {
		return err
	}
	w, err := m.newWriter(s, "visitor")
	if err != nil {
		return err
	}
	s.writer = w
	if s.Control || s.Kind == model.SessionBoard {
		go m.expire(ctx, s, controlExpiry)
	}
	return m.TermUI(func(stdin io.Reader, stdout io.Writer) error {
		fmt.Fprintf(stdout, "\r\n\x1b[36m● %s session with %s · ctrl+] then q to detach\x1b[0m\r\n", s.Kind, s.Peer.Username)
		tee := io.MultiWriter(stdout, writerFunc(func(b []byte) { _ = w.Output(b) }))
		return term.RunVisitor(ctx, ch, term.VisitorOptions{
			Stdin: stdin, Stdout: tee, ReadOnly: s.Kind == model.SessionWatch,
			OnCommand: func(c term.CommandEvent) {
				_ = w.CommandEnd(sessionlog.Command{Time: c.Time, Cmd: c.Cmd, Cwd: c.Cwd, Exit: c.Exit, Sudo: c.Sudo, Typist: c.Typist})
			},
		})
	})
}

type writerFunc func([]byte)

func (f writerFunc) Write(b []byte) (int, error) { f(b); return len(b), nil }

func (m *Manager) expire(ctx context.Context, s *Session, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
		s.end("expired after " + d.String())
	}
}

// --- host side ---------------------------------------------------------------------

func (m *Manager) onCard(sig rendezvous.Signal) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	from, err := m.app.User(ctx, sig.From)
	cancel()
	if err != nil {
		return
	}
	pt, err := m.open(sig.From, sig.SessionID, "card", sig.Payload)
	if err != nil {
		m.log.Warn("card: cannot open", "from", from.Username, "err", err)
		return
	}
	var c card
	if json.Unmarshal(pt, &c) != nil {
		return
	}
	if time.Since(time.Unix(c.At, 0)) > 2*time.Minute {
		return // stale replay
	}
	deny := func(reason string) {
		b, err := m.seal(context.Background(), sig.From, sig.SessionID, "card_reply", cardReply{Accept: false, Reason: reason})
		if err == nil {
			_ = m.app.Backend().SendSignal(context.Background(), rendezvous.Signal{SessionID: sig.SessionID, From: m.app.Me().ID, To: sig.From, Type: "card_reply", Payload: b})
		}
	}
	// Who may ask what. Terminal and screen: admin (with the pinned admin
	// key) or a helper who is my friend. File drop: any friend.
	// Admin status is decided by the key this device pinned, not by the
	// server's role column; a suspect (changed) key never qualifies.
	isAdmin := m.app.IsTrustedAdmin(from) && !m.app.Suspect(from.ID)
	isHelper := from.Role == model.RoleHelper && m.app.IsFriend(from.ID) && !m.app.Suspect(from.ID)
	switch c.Kind {
	case model.SessionWatch, model.SessionBoard, model.SessionScreen:
		if !isAdmin && !isHelper {
			deny("not allowed")
			return
		}
		if !m.app.IsFriend(from.ID) {
			deny("not friends")
			return
		}
	case model.SessionSend:
		if !m.app.IsFriend(from.ID) && !isAdmin {
			deny("not friends")
			return
		}
	default:
		deny("unknown kind")
		return
	}
	for _, s := range m.Live() {
		if s.Host && s.Peer.ID == from.ID && s.Kind != model.SessionSend {
			deny("busy")
			return
		}
	}
	_, pk, err := m.pairwise(ctx, from.ID)
	if err != nil {
		deny("no key")
		return
	}
	s := &Session{ID: sig.SessionID, Kind: c.Kind, Control: c.Control, Peer: from, Host: true, Via: model.ViaCard, Sudo: model.SudoBlocked, Started: time.Now().UTC(), State: "pending"}

	// Standing grant: terminal kinds only, never screen or control.
	if c.Kind == model.SessionWatch || c.Kind == model.SessionBoard {
		if g := m.app.GrantFor(from.ID, pk); g != nil && (c.Kind == model.SessionWatch || g.Scope == model.ScopeBoard) {
			s.Via, s.Sudo = model.ViaGrant, g.Sudo
			m.track(s)
			go m.hostAccept(s, pk)
			return
		}
	}
	if c.Kind == model.SessionSend {
		m.hostFileCard(s, pk, c)
		return
	}
	decided := make(chan cardReply, 1)
	text := fmt.Sprintf("%s is asking to %s", m.app.Name(from.ID), describe(c.Kind, c.Control))
	expires := time.Now().Add(cardTimeout)
	m.app.Emit(app.Event{Kind: app.EvCard, Card: &app.Card{
		ID: s.ID, Kind: "access", From: from.ID, FromName: m.app.Name(from.ID), Text: text, Session: s.ID, Scope: c.Kind, Control: c.Control, ExpiresAt: expires.UnixNano(),
		Reply: func(accept bool, sudo model.SudoTier) {
			select {
			case decided <- cardReply{Accept: accept, Sudo: sudo}:
			default:
			}
		},
	}})
	m.app.Notify("card:"+s.ID, "PairDesk", text, "critical")
	var r cardReply
	select {
	case r = <-decided:
	case <-time.After(cardTimeout):
		deny("timed out")
		m.app.Emit(app.Event{Kind: app.EvNotice, Text: "request from " + from.Username + " timed out"})
		return
	}
	if !r.Accept {
		deny("denied by host")
		return
	}
	if c.Kind == model.SessionScreen {
		r.Sudo = model.SudoBlocked
	}
	if r.Sudo == "" {
		r.Sudo = model.SudoBlocked
	}
	s.Sudo = r.Sudo
	m.track(s)
	go m.hostAccept(s, pk)
}

func describe(k model.SessionKind, control bool) string {
	switch k {
	case model.SessionWatch:
		return "WATCH YOUR TERMINAL (read-only)"
	case model.SessionBoard:
		return "OPEN A SHELL ON YOUR MACHINE"
	case model.SessionScreen:
		if control {
			return "VIEW AND CONTROL YOUR SCREEN"
		}
		return "VIEW YOUR SCREEN"
	}
	return string(k)
}

func (m *Manager) hostAccept(s *Session, pk crypto.PublicKey) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer cancel()
	s.setState("connecting")
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})

	w, err := m.newWriter(s, "host")
	if err != nil {
		m.finish(s, "log: "+err.Error())
		return
	}
	s.writer = w

	cfg := m.p2pConfig(s.ID)
	var stopCapture func()
	if s.Kind == model.SessionScreen {
		stop, err := m.startCapture(ctx, &cfg)
		if err != nil {
			m.replyCard(s, cardReply{Accept: false, Reason: "screen capture failed: " + err.Error()})
			m.finish(s, "capture: "+err.Error())
			return
		}
		stopCapture = stop
	}
	m.replyCard(s, cardReply{Accept: true, Sudo: s.Sudo, Via: s.Via})

	conn, err := dialWithTimeout(ctx, func(dctx context.Context) (*p2p.Conn, error) {
		return p2p.Accept(dctx, cfg, p2p.Peer{ID: s.Peer.ID, PubKey: pk}, s.ID)
	})
	if err != nil {
		if stopCapture != nil {
			stopCapture()
		}
		m.finish(s, "connect failed: "+err.Error())
		return
	}
	s.conn = conn
	s.setState("live")
	_ = m.app.Backend().LogSession(ctx, model.SessionLog{ID: s.ID, Visitor: s.Peer.ID, Host: m.app.Me().ID, Kind: s.Kind, Via: s.Via, StartedAt: s.Started})
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})
	m.app.Notify("live:"+s.ID, "PairDesk", fmt.Sprintf("%s is now %s", s.Peer.Username, liveVerb(s)), "critical")

	// File activity watcher for the whole session.
	home, _ := os.UserHomeDir()
	exclude := []string{m.app.Store().Paths.Config, m.app.Store().Paths.State, m.app.Store().Paths.Cache}
	var watcher *sessionlog.Watcher
	if s.Kind == model.SessionBoard || s.Kind == model.SessionShare {
		watcher, _ = sessionlog.NewWatcher(ctx, w, home, exclude)
	}
	if s.Control {
		go m.expire(ctx, s, controlExpiry)
	}

	switch s.Kind {
	case model.SessionWatch, model.SessionBoard:
		m.hostTerminal(ctx, s, conn, w)
	case model.SessionScreen:
		m.hostScreen(ctx, s, conn)
	}
	if watcher != nil {
		watcher.Close()
	}
	if stopCapture != nil {
		stopCapture()
	}
	m.finish(s, "ended")
}

func liveVerb(s *Session) string {
	switch s.Kind {
	case model.SessionWatch:
		return "watching your terminal"
	case model.SessionBoard:
		return "aboard your terminal"
	case model.SessionScreen:
		if s.Control {
			return "controlling your screen"
		}
		return "viewing your screen"
	}
	return "connected"
}

func (m *Manager) replyCard(s *Session, r cardReply) {
	b, err := m.seal(context.Background(), s.Peer.ID, s.ID, "card_reply", r)
	if err != nil {
		return
	}
	_ = m.app.Backend().SendSignal(context.Background(), rendezvous.Signal{SessionID: s.ID, From: m.app.Me().ID, To: s.Peer.ID, Type: "card_reply", Payload: b})
}

func (m *Manager) hostTerminal(ctx context.Context, s *Session, conn *p2p.Conn, w *sessionlog.Writer) {
	actx, acancel := context.WithTimeout(ctx, 20*time.Second)
	ch, err := conn.AcceptChannel(actx)
	acancel()
	if err != nil {
		s.setReason("no channel: " + err.Error())
		return
	}
	shimDir := filepath.Join(m.app.Store().Paths.Cache, "shim-"+s.ID)
	opts := term.HostOptions{
		ReadOnly:    s.Kind == model.SessionWatch,
		SudoTier:    s.Sudo,
		ShimDir:     shimDir,
		Isolate:     term.IsolateRequired,
		HiddenPaths: []string{m.app.Store().Paths.Config, m.app.Store().Paths.State, m.app.Store().Paths.Cache},
		Env:      append(os.Environ(), "PAIRDESK_SESSION="+s.ID, "PAIRDESK_VISITOR="+s.Peer.Username),
		OnOutput: func(_ time.Time, b []byte) { _ = w.Output(b) },
		OnCommand: func(c term.CommandEvent) {
			_ = w.CommandEnd(sessionlog.Command{Time: c.Time, Cmd: c.Cmd, Cwd: c.Cwd, Exit: c.Exit, Sudo: c.Sudo, Typist: c.Typist})
			if c.Sudo {
				m.app.Notify("sudo:"+s.ID, "PairDesk", fmt.Sprintf("%s ran sudo: %s", s.Peer.Username, c.Cmd), "critical")
			}
		},
		OnShareRequest: func() bool { return m.shareCard(s) },
	}
	switch s.Sudo {
	case model.SudoAuto:
		opts.SudoPassword = func() (string, bool) { return m.app.Store().SudoSecret(m.app.Identity()) }
	case model.SudoBlocked:
		var pw string
		opts.OnSudo = func(cmd string) bool {
			ok, p := m.sudoCard(s, cmd)
			pw = p
			return ok
		}
		opts.SudoPassword = func() (string, bool) {
			p := pw
			pw = ""
			return p, p != ""
		}
	}
	h, err := term.StartHost(ctx, ch, opts)
	if err != nil {
		s.setReason("shell: " + err.Error())
		if errors.Is(err, term.ErrNoIsolation) {
			m.app.Emit(appNotice("session refused: this machine cannot sandbox the visitor shell. Install bubblewrap (bwrap) or enable unprivileged user namespaces."))
		}
		return
	}
	s.host = h
	<-h.Done()
	if err := h.Err(); err != nil && s.reason == "" {
		s.setReason(err.Error())
	}
}

// sudoCard asks the host to approve one sudo command and type their password.
func (m *Manager) sudoCard(s *Session, cmd string) (bool, string) {
	type ans struct {
		ok bool
		pw string
	}
	decided := make(chan ans, 1)
	text := fmt.Sprintf("%s wants to run: sudo %s", s.Peer.Username, cmd)
	m.app.Emit(app.Event{Kind: app.EvCard, Card: &app.Card{
		ID: crypto.RandomID(6), Kind: "sudo", From: s.Peer.ID, FromName: s.Peer.Username, Text: text, Session: s.ID, Command: cmd, ExpiresAt: time.Now().Add(cardTimeout).UnixNano(),
		Reply: func(accept bool, _ model.SudoTier) {
			select {
			case decided <- ans{ok: accept}:
			default:
			}
		},
		ReplyWithPassword: func(accept bool, pw string) {
			select {
			case decided <- ans{ok: accept, pw: pw}:
			default:
			}
		},
	}})
	m.app.Notify("sudo-card:"+s.ID, "PairDesk", text, "critical")
	select {
	case a := <-decided:
		return a.ok, a.pw
	case <-time.After(cardTimeout):
		return false, ""
	}
}

// shareCard asks the host whether to make the terminal shared.
func (m *Manager) shareCard(s *Session) bool {
	decided := make(chan bool, 1)
	m.app.Emit(app.Event{Kind: app.EvCard, Card: &app.Card{
		ID: crypto.RandomID(6), Kind: "share", From: s.Peer.ID, FromName: s.Peer.Username, Text: s.Peer.Username + " wants to share the terminal (both of you type)", Session: s.ID, ExpiresAt: time.Now().Add(cardTimeout).UnixNano(),
		Reply: func(accept bool, _ model.SudoTier) {
			select {
			case decided <- accept:
			default:
			}
		},
	}})
	select {
	case ok := <-decided:
		if ok && m.TermUI != nil {
			go m.hostShareLoop(s)
		}
		return ok
	case <-time.After(cardTimeout):
		return false
	}
}

// hostShareLoop lets the host type into the shared shell from their own
// terminal until they detach with ctrl+] q.
func (m *Manager) hostShareLoop(s *Session) {
	_ = m.TermUI(func(stdin io.Reader, stdout io.Writer) error {
		fmt.Fprintf(stdout, "\r\n\x1b[33m● shared terminal with %s · ctrl+] then q to leave\x1b[0m\r\n", s.Peer.Username)
		s.host.SetShared(true)
		defer s.host.SetShared(false)
		done := make(chan struct{})
		go func() {
			defer close(done)
			r := s.host.Transcript()
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					_, _ = stdout.Write(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		buf := make([]byte, 256)
		esc := false
		for {
			select {
			case <-s.host.Done():
				return nil
			default:
			}
			n, err := stdin.Read(buf)
			if err != nil {
				return err
			}
			for _, b := range buf[:n] {
				if esc {
					esc = false
					if b == 'q' {
						return nil
					}
					_ = s.host.Input([]byte{0x1d, b})
					continue
				}
				if b == 0x1d {
					esc = true
					continue
				}
				_ = s.host.Input([]byte{b})
			}
		}
	})
}

// --- logs ------------------------------------------------------------------------

// Store exposes the local store (for preferences).
func (m *Manager) Store() *store.Store { return m.app.Store() }

// Logs lists sessions recorded on this machine.
func (m *Manager) Logs() ([]sessionlog.Entry, error) { return sessionlog.List(m.logsRoot()) }

// Verify checks a session's chain and seal against the key that must have
// signed it: this device's for host copies, the pinned visitor key for
// visitor copies. The key recorded inside the file is never trusted.
func (m *Manager) Verify(dir string) (sessionlog.VerifyResult, error) {
	meta, err := sessionlog.ReadMeta(dir)
	if err != nil {
		return sessionlog.VerifyResult{}, err
	}
	var pub crypto.PublicKey
	switch meta.Role {
	case "host":
		pub = m.app.Identity().Public()
	case "visitor":
		pub = m.app.Identity().Public()
	}
	if pub == nil {
		return sessionlog.VerifyResult{}, errors.New("unknown log role")
	}
	return sessionlog.Verify(dir, pub)
}

// Replay plays a transcript back in the real terminal.
func (m *Manager) Replay(ctx context.Context, dir string, speed float64) error {
	if m.TermUI == nil {
		return errors.New("no terminal")
	}
	if speed <= 0 {
		speed = 1
	}
	return m.TermUI(func(stdin io.Reader, stdout io.Writer) error {
		fmt.Fprintf(stdout, "\r\n\x1b[36m● replay · any key to stop\x1b[0m\r\n")
		stop := make(chan struct{})
		go func() {
			b := make([]byte, 1)
			_, _ = stdin.Read(b)
			close(stop)
		}()
		return sessionlog.Replay(ctx, dir, stdout, speed, stop)
	})
}

// DeleteLog removes a session record and notes the deletion.
func (m *Manager) DeleteLog(dir string) error {
	rel, err := filepath.Rel(m.logsRoot(), dir)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel[0] == '.' {
		return errors.New("not a session directory")
	}
	f, err := os.OpenFile(filepath.Join(m.logsRoot(), "deletions.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err == nil {
		fmt.Fprintf(f, "%s deleted %s\n", time.Now().UTC().Format(time.RFC3339), rel)
		f.Close()
	}
	return os.RemoveAll(dir)
}

// PruneLogs applies the retention preference.
func (m *Manager) PruneLogs() {
	days := m.app.Store().Prefs().LogKeepDays
	if days <= 0 {
		return
	}
	entries, _ := m.Logs()
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, e := range entries {
		if e.Meta.StartedAt.Before(cutoff) {
			_ = m.DeleteLog(e.Dir)
		}
	}
}
