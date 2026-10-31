package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/p2p"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
	"github.com/Kumarsujay03/pairdesk/internal/transfer"
)

// fileCard is the manifest plus direction, carried inside card.Manifest.
type fileCard struct {
	Manifest transfer.Manifest `json:"m"`
	Pull     bool              `json:"pull"` // true: requester wants to download from me (I announced as a seed)
}

// seeds are files this device can serve: manifest id -> local path.
var seedsMu sync.Mutex

func (m *Manager) seeds() map[string]seedEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seedMap == nil {
		m.seedMap = map[string]seedEntry{}
	}
	return m.seedMap
}

type seedEntry struct {
	Manifest transfer.Manifest
	Path     string
	Conv     string
}

func (m *Manager) addSeed(e seedEntry) {
	seedsMu.Lock()
	defer seedsMu.Unlock()
	m.seeds()[e.Manifest.ID] = e
}

func (m *Manager) seed(id string) (seedEntry, bool) {
	seedsMu.Lock()
	defer seedsMu.Unlock()
	e, ok := m.seeds()[id]
	return e, ok
}

// DownloadDir is where received files land.
func DownloadDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "pairdesk")
}

// Send offers a file to a friend (or, for the admin, anyone). It blocks
// until the transfer finishes.
func (m *Manager) Send(ctx context.Context, name, path string, progress func(transfer.Progress)) error {
	if m.lock != nil && m.app.IsHelperOrAdmin() {
		if err := m.lock.Require(); err != nil {
			return err
		}
	}
	peer, err := m.app.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if !m.app.IsFriend(peer.ID) && !m.app.IsAdmin() {
		return ErrNotFriend
	}
	if !m.app.Online(peer.ID) {
		return fmt.Errorf("%s is offline", peer.Username)
	}
	path = expand(path)
	man, err := transfer.BuildManifest(path, m.app.Me().ID)
	if err != nil {
		return err
	}
	m.addSeed(seedEntry{Manifest: man, Path: path})
	id := crypto.RandomID(12)
	s := &Session{ID: id, Kind: model.SessionSend, Peer: peer, Started: time.Now().UTC(), State: "connecting", Via: model.ViaCard}
	m.track(s)
	reply := make(chan cardReply, 1)
	m.mu.Lock()
	m.pending[id] = reply
	m.mu.Unlock()
	fc, _ := json.Marshal(fileCard{Manifest: man})
	payload, err := m.seal(ctx, peer.ID, id, "card", card{Kind: model.SessionSend, Name: m.app.Me().Username, Manifest: fc, At: time.Now().Unix()})
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
		m.finish(s, "declined")
		return errors.New("declined")
	}
	return m.serveTo(ctx, s, man, path, progress)
}

func (m *Manager) serveTo(ctx context.Context, s *Session, man transfer.Manifest, path string, progress func(transfer.Progress)) error {
	sctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()
	_, pk, err := m.pairwise(ctx, s.Peer.ID)
	if err != nil {
		m.finish(s, err.Error())
		return err
	}
	conn, err := dialWithTimeout(sctx, func(dctx context.Context) (*p2p.Conn, error) {
		return p2p.Dial(dctx, m.p2pConfig(s.ID), p2p.Peer{ID: s.Peer.ID, PubKey: pk}, s.ID)
	})
	if err != nil {
		m.finish(s, "connect failed: "+err.Error())
		return err
	}
	s.conn = conn
	s.State = "live"
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})
	ch, err := conn.OpenChannel(sctx, "file", true)
	if err != nil {
		m.finish(s, err.Error())
		return err
	}
	err = transfer.Serve(sctx, ch, man, path, progress)
	reason := "sent " + man.Name
	if err != nil {
		reason = "send failed: " + err.Error()
	}
	s.end(reason)
	m.finish(s, reason)
	return err
}

// hostFileCard handles an incoming file offer or a pull from a seed.
func (m *Manager) hostFileCard(s *Session, pk crypto.PublicKey, c card) {
	var fc fileCard
	if json.Unmarshal(c.Manifest, &fc) != nil || fc.Manifest.Validate() != nil {
		m.replyCard(s, cardReply{Accept: false, Reason: "bad manifest"})
		return
	}
	if fc.Pull {
		// Someone wants a file I announced as a seed. Only serve what I
		// explicitly offered.
		e, ok := m.seed(fc.Manifest.ID)
		if !ok {
			m.replyCard(s, cardReply{Accept: false, Reason: "not seeding"})
			return
		}
		m.track(s)
		go func() {
			m.replyCard(s, cardReply{Accept: true})
			ctx, cancel := context.WithCancel(context.Background())
			s.cancel = cancel
			defer cancel()
			conn, err := dialWithTimeout(ctx, func(dctx context.Context) (*p2p.Conn, error) {
				return p2p.Accept(dctx, m.p2pConfig(s.ID), p2p.Peer{ID: s.Peer.ID, PubKey: pk}, s.ID)
			})
			if err != nil {
				m.finish(s, "connect failed")
				return
			}
			s.conn = conn
			s.State = "live"
			actx, acancel := context.WithTimeout(ctx, 20*time.Second)
			ch, err := conn.AcceptChannel(actx)
			acancel()
			if err != nil {
				m.finish(s, "no channel")
				return
			}
			err = transfer.Serve(ctx, ch, e.Manifest, e.Path, nil)
			reason := "seeded " + e.Manifest.Name
			if err != nil {
				reason = "seed failed: " + err.Error()
			}
			s.end(reason)
			m.finish(s, reason)
		}()
		return
	}
	decided := make(chan bool, 1)
	text := fmt.Sprintf("%s wants to send you %s (%s)", m.app.Name(s.Peer.ID), fc.Manifest.Name, humanSize(fc.Manifest.Size))
	m.app.Emit(app.Event{Kind: app.EvCard, Card: &app.Card{
		ID: s.ID, Kind: "file", From: s.Peer.ID, FromName: m.app.Name(s.Peer.ID), Text: text, Session: s.ID, ExpiresAt: time.Now().Add(cardTimeout).UnixNano(),
		Reply: func(accept bool, _ model.SudoTier) {
			select {
			case decided <- accept:
			default:
			}
		},
	}})
	m.app.Notify("file:"+s.ID, "PairDesk", text, "normal")
	select {
	case ok := <-decided:
		if !ok {
			m.replyCard(s, cardReply{Accept: false, Reason: "declined"})
			return
		}
	case <-time.After(cardTimeout):
		m.replyCard(s, cardReply{Accept: false, Reason: "timed out"})
		return
	}
	m.track(s)
	go m.receiveFrom(s, pk, fc.Manifest, nil)
}

func (m *Manager) receiveFrom(s *Session, pk crypto.PublicKey, man transfer.Manifest, extra []p2p.Channel) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer cancel()
	m.replyCard(s, cardReply{Accept: true})
	conn, err := dialWithTimeout(ctx, func(dctx context.Context) (*p2p.Conn, error) {
		return p2p.Accept(dctx, m.p2pConfig(s.ID), p2p.Peer{ID: s.Peer.ID, PubKey: pk}, s.ID)
	})
	if err != nil {
		m.finish(s, "connect failed: "+err.Error())
		return
	}
	s.conn = conn
	s.State = "live"
	m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})
	actx, acancel := context.WithTimeout(ctx, 20*time.Second)
	ch, err := conn.AcceptChannel(actx)
	acancel()
	if err != nil {
		m.finish(s, "no channel")
		return
	}
	dest := filepath.Join(DownloadDir(), safeName(man.Name))
	_ = os.MkdirAll(DownloadDir(), 0o700)
	var last time.Time
	err = transfer.Receive(ctx, ch, man, transfer.ReceiveOptions{Dest: dest, OnProgress: func(p transfer.Progress) {
		if time.Since(last) > 500*time.Millisecond {
			last = time.Now()
			s.reason = fmt.Sprintf("%d/%d chunks", p.Done, p.Total)
			m.app.Emit(app.Event{Kind: app.EvSession, Session: m.info(s)})
		}
	}})
	if errors.Is(err, transfer.ErrExists) {
		dest = uniqueName(dest)
		err = transfer.Receive(ctx, ch, man, transfer.ReceiveOptions{Dest: dest})
	}
	reason := "saved " + dest
	if err != nil {
		reason = "receive failed: " + err.Error()
	} else {
		m.addSeed(seedEntry{Manifest: man, Path: dest})
		if s.writer == nil {
			_ = m.app.Backend().LogSession(ctx, model.SessionLog{ID: s.ID, Visitor: s.Peer.ID, Host: m.app.Me().ID, Kind: model.SessionSend, Via: model.ViaCard, StartedAt: s.Started})
		}
	}
	s.reason = ""
	s.end(reason)
	m.finish(s, reason)
}

// ShareToGroup announces a file in a group; members fetch it from whoever
// has it with Fetch. The manifest travels as a chat message of kind "file".
func (m *Manager) ShareToGroup(ctx context.Context, c *app.Conversation, path string) error {
	path = expand(path)
	man, err := transfer.BuildManifest(path, m.app.Me().ID)
	if err != nil {
		return err
	}
	m.addSeed(seedEntry{Manifest: man, Path: path, Conv: c.ID})
	b, _ := json.Marshal(man)
	return m.app.SendKind(ctx, c, "file", string(b))
}

// Fetch downloads a group-shared file from every online member that has it.
func (m *Manager) Fetch(ctx context.Context, c *app.Conversation, manifestID string, progress func(transfer.Progress)) error {
	var man transfer.Manifest
	seeds := map[string]bool{}
	msgs, _ := m.app.Snapshot(c)
	for _, msg := range msgs {
		switch msg.Kind {
		case "file":
			var mm transfer.Manifest
			if json.Unmarshal([]byte(msg.Text), &mm) == nil && (mm.ID == manifestID || strings.HasPrefix(mm.ID, manifestID)) {
				man = mm
				seeds[msg.Sender] = true
			}
		case "file_seed":
			if strings.TrimSpace(msg.Text) == manifestID {
				seeds[msg.Sender] = true
			}
		}
	}
	if man.ID == "" {
		return errors.New("no such file in this group")
	}
	var chans []p2p.Channel
	var conns []*p2p.Conn
	defer func() {
		for _, cn := range conns {
			_ = cn.Close()
		}
	}()
	for uid := range seeds {
		if uid == m.app.Me().ID || !m.app.Online(uid) {
			continue
		}
		ch, cn, err := m.pullFrom(ctx, uid, man)
		if err != nil {
			m.log.Warn("seed unavailable", "seed", uid, "err", err)
			continue
		}
		chans = append(chans, ch)
		conns = append(conns, cn)
	}
	if len(chans) == 0 {
		return errors.New("no online seed has this file")
	}
	dest := filepath.Join(DownloadDir(), safeName(man.Name))
	_ = os.MkdirAll(DownloadDir(), 0o700)
	tchans := make([]transfer.Channel, len(chans))
	for i, c := range chans {
		tchans[i] = c
	}
	err := transfer.ReceiveMulti(ctx, tchans, man, transfer.ReceiveOptions{Dest: dest, OnProgress: progress})
	if errors.Is(err, transfer.ErrExists) {
		return fmt.Errorf("already downloaded: %s", dest)
	}
	if err != nil {
		return err
	}
	m.addSeed(seedEntry{Manifest: man, Path: dest, Conv: c.ID})
	_ = m.app.SendKind(ctx, c, "file_seed", man.ID)
	m.app.Emit(appNotice("saved " + dest))
	return nil
}

func (m *Manager) pullFrom(ctx context.Context, uid string, man transfer.Manifest) (p2p.Channel, *p2p.Conn, error) {
	peer, err := m.app.User(ctx, uid)
	if err != nil {
		return nil, nil, err
	}
	id := crypto.RandomID(12)
	reply := make(chan cardReply, 1)
	m.mu.Lock()
	m.pending[id] = reply
	m.mu.Unlock()
	defer m.dropSignaler(id)
	fc, _ := json.Marshal(fileCard{Manifest: man, Pull: true})
	payload, err := m.seal(ctx, uid, id, "card", card{Kind: model.SessionSend, Name: m.app.Me().Username, Manifest: fc, At: time.Now().Unix()})
	if err != nil {
		return nil, nil, err
	}
	if err := m.app.Backend().SendSignal(ctx, rendezvous.Signal{SessionID: id, From: m.app.Me().ID, To: uid, Type: "card", Payload: payload}); err != nil {
		return nil, nil, err
	}
	select {
	case r := <-reply:
		if !r.Accept {
			return nil, nil, errors.New(r.Reason)
		}
	case <-time.After(20 * time.Second):
		return nil, nil, ErrTimeout
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	_, pk, err := m.pairwise(ctx, uid)
	if err != nil {
		return nil, nil, err
	}
	conn, err := dialWithTimeout(ctx, func(dctx context.Context) (*p2p.Conn, error) {
		return p2p.Dial(dctx, m.p2pConfig(id), p2p.Peer{ID: peer.ID, PubKey: pk}, id)
	})
	if err != nil {
		return nil, nil, err
	}
	ch, err := conn.OpenChannel(ctx, "file", true)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return ch, conn, nil
}

func appNotice(text string) app.Event { return app.Event{Kind: app.EvNotice, Text: text} }

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func safeName(n string) string {
	n = filepath.Base(n)
	if n == "." || n == ".." || n == "/" || n == "" || strings.HasPrefix(n, "-") ||
		strings.HasSuffix(n, ".part") || strings.HasSuffix(n, ".part.json") {
		return "file-" + crypto.RandomID(4)
	}
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '/' {
			return '_'
		}
		return r
	}, n)
}

func uniqueName(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(c); err != nil {
			return c
		}
	}
	return p
}

func humanSize(n int64) string {
	switch {
	case n > 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n > 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
