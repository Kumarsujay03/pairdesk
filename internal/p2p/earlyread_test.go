package p2p

import (
	"context"
	"testing"
	"time"
)

// A reader on a freshly opened channel must block until the peer accepts
// and writes, not see EOF.
func TestEarlyReadBlocksUntilPeerWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	idA, idB := mustIDs(t)
	sa, sb := newRelay()
	p := dialPair(t, ctx, sa, sb, idA, idB, idB.Public(), idA.Public())
	if p.errA != nil || p.errB != nil {
		t.Fatal(p.errA, p.errB)
	}
	defer p.a.Close()
	defer p.b.Close()
	ch, err := p.a.OpenChannel(ctx, "term", true)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := ch.Read(buf)
		if err != nil {
			got <- err
			return
		}
		if string(buf[:n]) != "late" {
			t.Errorf("read %q", buf[:n])
		}
		got <- nil
	}()
	time.Sleep(500 * time.Millisecond)
	bch, err := p.b.AcceptChannel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := bch.Write([]byte("late")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("early read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read never returned")
	}
}
