// Package p2p sets up an authenticated, encrypted WebRTC connection between
// two PairDesk devices over an opaque signaling relay.
//
// Handshake order:
//  1. Noise IK over the relay, static keys derived from the Ed25519
//     identities. Afterwards every signaling payload is sealed.
//  2. pion/webrtc offer/answer with trickle ICE through the sealed channel.
//  3. On the "ctl" data channel each side sends its DTLS certificate
//     fingerprint signed with its identity, bound to the peer's fingerprint,
//     so the DTLS session cannot be substituted by a man in the middle.
//
// One Signaler serves one session: the caller demultiplexes the backend's
// signal stream by SessionID before handing it to Dial or Accept.
package p2p

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Signaler relays opaque blobs between the two parties.
type Signaler interface {
	Send(ctx context.Context, s rendezvous.Signal) error // relay one blob to s.To
	Recv() <-chan rendezvous.Signal                      // blobs addressed to me
}

// Config is everything Dial and Accept need beyond the peer.
type Config struct {
	Me       *crypto.Identity
	MyID     string             // my user id
	STUN     []string           // e.g. stun:stun.l.google.com:19302
	TURN     []webrtc.ICEServer // optional
	Signaler Signaler
	Logger   *slog.Logger
	// ReceiveVideo makes the dialer offer a receive-only video m-line so the
	// acceptor can attach VideoTrack before answering (no renegotiation).
	ReceiveVideo bool
	// VideoTrack, on the acceptor side, is added before the answer.
	VideoTrack *webrtc.TrackLocalStaticRTP
	// VideoSender receives the RTPSender for VideoTrack (for RTCP reading).
	VideoSender func(*webrtc.RTPSender)
}

// Peer is the remote device's user id and pinned key.
type Peer struct {
	ID     string
	PubKey crypto.PublicKey
}

const ctlLabel = "ctl"

// Conn is a live, authenticated peer connection.
type Conn struct {
	pc        *webrtc.PeerConnection
	peer      Peer
	sessionID string
	log       *slog.Logger
	ctl       *channel

	incoming chan *channel
	done     chan struct{}
	doneOnce sync.Once
	err      error
	errMu    sync.Mutex

	trackMu sync.Mutex
	onTrack func(*webrtc.TrackRemote)
	tracks  []*webrtc.TrackRemote
}

// Dial starts a session as the initiator.
func Dial(ctx context.Context, cfg Config, peer Peer, sessionID string) (*Conn, error) {
	return connect(ctx, cfg, peer, sessionID, true)
}

// Accept answers a session the peer initiated.
func Accept(ctx context.Context, cfg Config, peer Peer, sessionID string) (*Conn, error) {
	return connect(ctx, cfg, peer, sessionID, false)
}

func connect(ctx context.Context, cfg Config, peer Peer, sessionID string, initiator bool) (*Conn, error) {
	if cfg.Me == nil || cfg.Signaler == nil || len(peer.PubKey) == 0 {
		return nil, errors.New("p2p: incomplete config")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("session", sessionID, "peer", peer.ID)

	// Filter the relay stream to this session and sender.
	filtered := make(chan rendezvous.Signal, 64)
	stop := make(chan struct{})
	go func() {
		defer close(filtered)
		for {
			select {
			case <-stop:
				return
			case s, ok := <-cfg.Signaler.Recv():
				if !ok {
					return
				}
				if s.SessionID != sessionID || s.From != peer.ID {
					log.Debug("ignoring signal", "from", s.From, "session", s.SessionID)
					continue
				}
				select {
				case filtered <- s:
				case <-stop:
					return
				}
			}
		}
	}()
	stopOnce := sync.Once{}
	stopFilter := func() { stopOnce.Do(func() { close(stop) }) }

	ss, err := handshake(ctx, cfg, peer, sessionID, initiator, filtered)
	if err != nil {
		stopFilter()
		return nil, err
	}

	// Our own DTLS certificate, so we know its fingerprint up front.
	sk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		stopFilter()
		return nil, err
	}
	cert, err := webrtc.GenerateCertificate(sk)
	if err != nil {
		stopFilter()
		return nil, err
	}
	fps, err := cert.GetFingerprints()
	if err != nil || len(fps) == 0 {
		stopFilter()
		return nil, fmt.Errorf("p2p: certificate fingerprint: %v", err)
	}
	localFP, err := parseFingerprint(fps[0].Value)
	if err != nil {
		stopFilter()
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.DetachDataChannels()
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	var servers []webrtc.ICEServer
	if len(cfg.STUN) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: cfg.STUN})
	}
	servers = append(servers, cfg.TURN...)
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   servers,
		Certificates: []webrtc.Certificate{*cert},
	})
	if err != nil {
		stopFilter()
		return nil, err
	}

	c := &Conn{
		pc: pc, peer: peer, sessionID: sessionID, log: log,
		incoming: make(chan *channel, 16),
		done:     make(chan struct{}),
	}
	fail := func(err error) (*Conn, error) {
		c.fail(err)
		stopFilter()
		return nil, err
	}

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Debug("connection state", "state", s.String())
		switch s {
		case webrtc.PeerConnectionStateFailed:
			c.fail(errors.New("p2p: connection failed"))
		case webrtc.PeerConnectionStateClosed:
			c.fail(ErrClosed)
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.trackMu.Lock()
		cb := c.onTrack
		if cb == nil {
			c.tracks = append(c.tracks, t)
		}
		c.trackMu.Unlock()
		if cb != nil {
			cb(t)
		}
	})

	ctlCh := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == ctlLabel && !initiator {
			select {
			case ctlCh <- dc:
			default:
			}
			return
		}
		go func() {
			ch, err := openDetached(ctx, dc, c.done)
			if err != nil {
				return
			}
			select {
			case c.incoming <- ch:
			case <-c.done:
				_ = ch.Close()
			}
		}()
	})

	// Trickle ICE: local candidates go out sealed; remote ones are queued
	// until the remote description is set.
	var candMu sync.Mutex
	var pendingCands []webrtc.ICECandidateInit
	remoteSet := false
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		b, _ := json.Marshal(cand.ToJSON())
		if err := ss.sendSealed(ctx, sigICE, b); err != nil {
			log.Debug("send ice", "err", err)
		}
	})
	addCand := func(ci webrtc.ICECandidateInit) {
		candMu.Lock()
		if !remoteSet {
			pendingCands = append(pendingCands, ci)
			candMu.Unlock()
			return
		}
		candMu.Unlock()
		if err := pc.AddICECandidate(ci); err != nil {
			log.Debug("add ice", "err", err)
		}
	}
	markRemoteSet := func() {
		candMu.Lock()
		remoteSet = true
		cands := pendingCands
		pendingCands = nil
		candMu.Unlock()
		for _, ci := range cands {
			if err := pc.AddICECandidate(ci); err != nil {
				log.Debug("add ice", "err", err)
			}
		}
	}

	// Signal pump: runs until the connection is done.
	descCh := make(chan webrtc.SessionDescription, 1)
	wantDesc := sigAnswer
	if !initiator {
		wantDesc = sigOffer
	}
	go func() {
		defer stopFilter()
		for {
			select {
			case <-c.done:
				return
			case s, ok := <-filtered:
				if !ok {
					return
				}
				pt, err := ss.open(s.Type, s.Payload)
				if err != nil {
					log.Warn("rejected signal", "type", s.Type, "err", err)
					continue
				}
				switch s.Type {
				case sigICE:
					var ci webrtc.ICECandidateInit
					if err := json.Unmarshal(pt, &ci); err == nil {
						addCand(ci)
					}
				case wantDesc:
					var sd webrtc.SessionDescription
					if err := json.Unmarshal(pt, &sd); err == nil {
						select {
						case descCh <- sd:
						default:
						}
					}
				default:
					log.Debug("unexpected signal", "type", s.Type)
				}
			}
		}
	}()

	var ctlDC *webrtc.DataChannel
	if initiator {
		ordered := true
		ctlDC, err = pc.CreateDataChannel(ctlLabel, &webrtc.DataChannelInit{Ordered: &ordered})
		if err != nil {
			return fail(err)
		}
		if cfg.ReceiveVideo {
			if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
				return fail(err)
			}
		}
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			return fail(err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(offer)
		if err := ss.sendSealed(ctx, sigOffer, b); err != nil {
			return fail(err)
		}
		var answer webrtc.SessionDescription
		select {
		case answer = <-descCh:
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-c.done:
			return fail(c.Err())
		}
		if err := pc.SetRemoteDescription(answer); err != nil {
			return fail(err)
		}
		markRemoteSet()
	} else {
		var offer webrtc.SessionDescription
		select {
		case offer = <-descCh:
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-c.done:
			return fail(c.Err())
		}
		if err := pc.SetRemoteDescription(offer); err != nil {
			return fail(err)
		}
		markRemoteSet()
		if cfg.VideoTrack != nil {
			snd, err := pc.AddTrack(cfg.VideoTrack)
			if err != nil {
				return fail(err)
			}
			if cfg.VideoSender != nil {
				cfg.VideoSender(snd)
			}
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			return fail(err)
		}
		if err := pc.SetLocalDescription(answer); err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(answer)
		if err := ss.sendSealed(ctx, sigAnswer, b); err != nil {
			return fail(err)
		}
		select {
		case ctlDC = <-ctlCh:
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-c.done:
			return fail(c.Err())
		}
	}

	ctl, err := openDetached(ctx, ctlDC, c.done)
	if err != nil {
		return fail(err)
	}
	c.ctl = ctl

	// Bind DTLS to the identities.
	remoteDER := pc.SCTP().Transport().GetRemoteCertificate()
	if len(remoteDER) == 0 {
		return fail(errors.New("p2p: no remote DTLS certificate"))
	}
	remoteFP := sha256.Sum256(remoteDER)
	domain := "pairdesk/dtls/" + sessionID
	msg := append(append([]byte{}, localFP[:]...), remoteFP[:]...)
	sig := cfg.Me.Sign(domain, msg)
	if _, err := ctl.Write(append(append([]byte{}, localFP[:]...), sig...)); err != nil {
		return fail(err)
	}
	buf := make([]byte, 32+64)
	if err := readWithDeadline(ctx, ctl, buf, c.done); err != nil {
		return fail(fmt.Errorf("p2p: fingerprint exchange: %w", err))
	}
	theirFP, theirSig := buf[:32], buf[32:]
	if !cryptoEqual(theirFP, remoteFP[:]) {
		return fail(ErrFingerprint)
	}
	theirMsg := append(append([]byte{}, theirFP...), localFP[:]...)
	if err := peer.PubKey.Verify(domain, theirMsg, theirSig); err != nil {
		return fail(ErrFingerprint)
	}
	log.Info("p2p connected", "initiator", initiator)

	// Keep reading ctl: EOF or "bye" means the peer went away.
	go func() {
		b := make([]byte, 16)
		for {
			n, err := ctl.Read(b)
			if err != nil {
				c.fail(ErrClosed)
				return
			}
			if n > 0 && b[0] == ctlBye {
				c.fail(ErrClosed)
				return
			}
		}
	}()
	return c, nil
}

const ctlBye = 0xff

func readWithDeadline(ctx context.Context, ch *channel, buf []byte, done <-chan struct{}) error {
	errc := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(ch, buf)
		errc <- err
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ErrClosed
	}
}

func parseFingerprint(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) != 32 {
		return out, errors.New("p2p: bad local fingerprint")
	}
	copy(out[:], b)
	return out, nil
}

func (c *Conn) fail(err error) {
	c.doneOnce.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.done)
		go func() { _ = c.pc.Close() }()
	})
}

// Err reports why the connection ended, once Done is closed.
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// OpenChannel creates a new data channel the peer receives via AcceptChannel.
func (c *Conn) OpenChannel(ctx context.Context, label string, ordered bool) (Channel, error) {
	if label == ctlLabel {
		return nil, errors.New("p2p: reserved label")
	}
	select {
	case <-c.done:
		return nil, ErrClosed
	default:
	}
	init := &webrtc.DataChannelInit{Ordered: &ordered}
	if !ordered {
		zero := uint16(0)
		init.MaxRetransmits = &zero
	}
	dc, err := c.pc.CreateDataChannel(label, init)
	if err != nil {
		return nil, err
	}
	return openDetached(ctx, dc, c.done)
}

// AcceptChannel returns the next channel the peer opened.
func (c *Conn) AcceptChannel(ctx context.Context) (Channel, error) {
	select {
	case ch := <-c.incoming:
		return ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, ErrClosed
	}
}

// AddVideoTrack attaches an outgoing RTP track. Renegotiation is not yet
// handled; add tracks before the offer or use a fresh session.
func (c *Conn) AddVideoTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error) {
	return c.pc.AddTrack(track)
}

// OnTrack registers the callback for incoming media; tracks that arrived
// earlier are replayed.
func (c *Conn) OnTrack(f func(*webrtc.TrackRemote)) {
	c.trackMu.Lock()
	c.onTrack = f
	queued := c.tracks
	c.tracks = nil
	c.trackMu.Unlock()
	for _, t := range queued {
		f(t)
	}
}

func (c *Conn) Peer() Peer            { return c.peer }
func (c *Conn) SessionID() string     { return c.sessionID }
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close tells the peer goodbye and tears the connection down.
func (c *Conn) Close() error {
	select {
	case <-c.done:
		return nil
	default:
	}
	if c.ctl != nil {
		_, _ = c.ctl.Write([]byte{ctlBye})
		// Give the bye a moment to leave before DTLS shuts.
		time.Sleep(20 * time.Millisecond)
	}
	c.fail(ErrClosed)
	return nil
}
