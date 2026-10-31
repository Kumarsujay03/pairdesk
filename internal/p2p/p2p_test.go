package p2p

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// memSignaler is one end of an in-memory relay.
type memSignaler struct {
	in     chan rendezvous.Signal
	out    chan<- rendezvous.Signal
	tamper func(*rendezvous.Signal)
}

func newRelay() (*memSignaler, *memSignaler) {
	a := make(chan rendezvous.Signal, 64)
	b := make(chan rendezvous.Signal, 64)
	return &memSignaler{in: a, out: b}, &memSignaler{in: b, out: a}
}

func (m *memSignaler) Send(ctx context.Context, s rendezvous.Signal) error {
	if m.tamper != nil {
		m.tamper(&s)
	}
	select {
	case m.out <- s:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *memSignaler) Recv() <-chan rendezvous.Signal { return m.in }

func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("P2P_DEBUG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

type pair struct {
	a, b       *Conn
	errA, errB error
}

func dialPair(t *testing.T, ctx context.Context, sa, sb *memSignaler, idA, idB *crypto.Identity, peerForA, peerForB crypto.PublicKey) pair {
	t.Helper()
	cfgA := Config{Me: idA, MyID: "alice", Signaler: sa, Logger: testLogger(t)}
	cfgB := Config{Me: idB, MyID: "bob", Signaler: sb, Logger: testLogger(t)}
	var p pair
	done := make(chan struct{})
	go func() {
		p.b, p.errB = Accept(ctx, cfgB, Peer{ID: "alice", PubKey: peerForB}, "sess1")
		close(done)
	}()
	p.a, p.errA = Dial(ctx, cfgA, Peer{ID: "bob", PubKey: peerForA}, "sess1")
	<-done
	return p
}

func mustIDs(t *testing.T) (*crypto.Identity, *crypto.Identity) {
	t.Helper()
	a, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestDialAcceptRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	idA, idB := mustIDs(t)
	sa, sb := newRelay()
	p := dialPair(t, ctx, sa, sb, idA, idB, idB.Public(), idA.Public())
	if p.errA != nil || p.errB != nil {
		t.Fatalf("handshake failed: dial=%v accept=%v", p.errA, p.errB)
	}
	defer p.a.Close()
	defer p.b.Close()
	if p.a.Peer().ID != "bob" || p.b.Peer().ID != "alice" || p.a.SessionID() != "sess1" {
		t.Fatal("peer metadata wrong")
	}

	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	chA, err := p.a.OpenChannel(ctx, "data", true)
	if err != nil {
		t.Fatal(err)
	}
	chB, err := p.b.AcceptChannel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if chB.Label() != "data" {
		t.Fatalf("label %q", chB.Label())
	}
	go func() {
		if _, err := chA.Write(payload); err != nil {
			t.Error(err)
		}
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(chB, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload corrupted")
	}
	// And back the other way, small write.
	if _, err := chB.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(chA, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("reverse: %q %v", buf, err)
	}

	// Close propagates Done.
	if err := p.a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.a.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a.Done not closed")
	}
	select {
	case <-p.b.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("b.Done not closed after peer Close")
	}
	if _, err := chB.Read(buf); err == nil {
		t.Fatal("read after close should fail")
	}
}

func TestWrongPeerKeyFails(t *testing.T) {
	idA, idB := mustIDs(t)
	rogue, _ := mustIDs(t)

	t.Run("dialer has wrong responder key", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sa, sb := newRelay()
		p := dialPair(t, ctx, sa, sb, idA, idB, rogue.Public(), idA.Public())
		if p.errA == nil || p.errB == nil {
			t.Fatalf("expected both to fail: dial=%v accept=%v", p.errA, p.errB)
		}
		if !errors.Is(p.errB, ErrHandshake) {
			t.Fatalf("accept err = %v, want ErrHandshake", p.errB)
		}
	})
	t.Run("responder expects a different initiator", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sa, sb := newRelay()
		p := dialPair(t, ctx, sa, sb, idA, idB, idB.Public(), rogue.Public())
		if !errors.Is(p.errB, ErrPeerKey) {
			t.Fatalf("accept err = %v, want ErrPeerKey", p.errB)
		}
		if p.errA == nil {
			t.Fatal("dial should fail")
		}
	})
}

func TestTamperedSignalRejected(t *testing.T) {
	var ctx context.Context
	var cancel context.CancelFunc
	idA, idB := mustIDs(t)
	sa, sb := newRelay()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sa.tamper = func(s *rendezvous.Signal) {
		if s.Type == sigOffer {
			s.Payload = append([]byte{}, s.Payload...)
			s.Payload[len(s.Payload)-1] ^= 0x01
		}
	}
	p := dialPair(t, ctx, sa, sb, idA, idB, idB.Public(), idA.Public())
	if p.errA == nil || p.errB == nil {
		t.Fatalf("tampered offer must not connect: dial=%v accept=%v", p.errA, p.errB)
	}
}

func TestSealedSignalReplayAndTamper(t *testing.T) {
	idA, idB := mustIDs(t)
	sa, sb := newRelay()
	ctx := context.Background()
	var ssB *secureSignaler
	var errB error
	done := make(chan struct{})
	go func() {
		ssB, errB = handshake(ctx, Config{Me: idB, MyID: "bob", Signaler: sb}, Peer{ID: "alice", PubKey: idA.Public()}, "s", false, sb.in)
		close(done)
	}()
	ssA, err := handshake(ctx, Config{Me: idA, MyID: "alice", Signaler: sa}, Peer{ID: "bob", PubKey: idB.Public()}, "s", true, sa.in)
	<-done
	if err != nil || errB != nil {
		t.Fatal(err, errB)
	}
	if err := ssA.sendSealed(ctx, sigOffer, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	s := <-sb.in
	pt, err := ssB.open(s.Type, s.Payload)
	if err != nil || string(pt) != "hello" {
		t.Fatal(err, string(pt))
	}
	if _, err := ssB.open(s.Type, s.Payload); !errors.Is(err, ErrBadSignal) {
		t.Fatalf("replay accepted: %v", err)
	}
	if err := ssA.sendSealed(ctx, sigICE, []byte("cand")); err != nil {
		t.Fatal(err)
	}
	s = <-sb.in
	s.Payload[len(s.Payload)-1] ^= 1
	if _, err := ssB.open(s.Type, s.Payload); !errors.Is(err, ErrBadSignal) {
		t.Fatalf("tamper accepted: %v", err)
	}
	// Wrong type as AAD.
	if err := ssA.sendSealed(ctx, sigICE, []byte("cand")); err != nil {
		t.Fatal(err)
	}
	s = <-sb.in
	if _, err := ssB.open(sigOffer, s.Payload); !errors.Is(err, ErrBadSignal) {
		t.Fatalf("type swap accepted: %v", err)
	}
}
