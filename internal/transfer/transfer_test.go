package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pipeCh wraps one end of net.Pipe as a Channel.
type pipeCh struct {
	net.Conn
	label string
}

func (p pipeCh) Label() string { return p.label }

func pipe(a, b string) (Channel, Channel) {
	x, y := net.Pipe()
	return pipeCh{x, a}, pipeCh{y, b}
}

// reqCounter wraps the seed side and counts REQ frames read from the stream.
type reqCounter struct {
	Channel
	mu   sync.Mutex
	buf  []byte
	reqs int64
}

func (c *reqCounter) Read(p []byte) (int, error) {
	n, err := c.Channel.Read(p)
	c.mu.Lock()
	c.buf = append(c.buf, p[:n]...)
	for len(c.buf) >= 4 {
		l := int(binary.BigEndian.Uint32(c.buf))
		if len(c.buf) < 4+l {
			break
		}
		if c.buf[4] == typeReq {
			atomic.AddInt64(&c.reqs, 1)
		}
		c.buf = c.buf[4+l:]
	}
	c.mu.Unlock()
	return n, err
}

func (c *reqCounter) count() int { return int(atomic.LoadInt64(&c.reqs)) }

// corrupter wraps the seed side and flips the last byte of DATA frames.
// target < 0 corrupts every DATA; otherwise only that chunk index.
type corrupter struct {
	Channel
	target int
	hits   int64
}

func (c *corrupter) Write(p []byte) (int, error) {
	if len(p) > 9 && p[4] == typeData {
		idx := int(binary.BigEndian.Uint32(p[5:9]))
		if c.target < 0 || idx == c.target {
			q := append([]byte(nil), p...)
			q[len(q)-1] ^= 0xff
			atomic.AddInt64(&c.hits, 1)
			n, err := c.Channel.Write(q)
			return n, err
		}
	}
	return c.Channel.Write(p)
}

func randomFile(t *testing.T, dir string, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: content mismatch (%d vs %d bytes)", path, len(got), len(want))
	}
	if _, err := os.Stat(path + ".part"); err == nil {
		t.Errorf("%s.part still present", path)
	}
	if _, err := os.Stat(path + ".part.json"); err == nil {
		t.Errorf("%s.part.json still present", path)
	}
}

func serveAsync(ctx context.Context, ch Channel, m Manifest, path string, onProgress func(Progress)) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- Serve(ctx, ch, m, path, onProgress) }()
	return errc
}

func waitErr(t *testing.T, c <-chan error) error {
	t.Helper()
	select {
	case err := <-c:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for Serve")
		return nil
	}
}

const testSize = 3 << 20 // 3 MiB

func TestServeReceive(t *testing.T) {
	dir := t.TempDir()
	src, data := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	if m.NumChunks() != 12 || len(m.Chunks) != 12 {
		t.Fatalf("NumChunks = %d", m.NumChunks())
	}
	sum := sha256.Sum256(data)
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("manifest whole-file hash wrong")
	}
	if m.Name != "src.bin" || m.From != "priya" || m.Size != testSize || m.ID == "" {
		t.Fatalf("manifest fields: %+v", m)
	}

	ctx := context.Background()
	sc, rc := pipe("seed", "recv")
	var sendProg, recvProg atomic.Int64
	var lastRecv Progress
	done := serveAsync(ctx, sc, m, src, func(p Progress) {
		sendProg.Add(1)
		if p.Peer != "seed" || p.Total != 12 {
			t.Errorf("seed progress %+v", p)
		}
	})
	dest := filepath.Join(dir, "out.bin")
	err = Receive(ctx, rc, m, ReceiveOptions{Dest: dest, OnProgress: func(p Progress) {
		recvProg.Add(1)
		lastRecv = p
	}})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if sendProg.Load() != 12 || recvProg.Load() != 12 {
		t.Fatalf("progress callbacks: seed %d recv %d", sendProg.Load(), recvProg.Load())
	}
	if lastRecv.Done != 12 || lastRecv.Bytes != testSize || lastRecv.Peer != "recv" || lastRecv.Rate <= 0 {
		t.Fatalf("final receiver progress %+v", lastRecv)
	}
	assertFile(t, dest, data)
	got, err := hashFile(dest)
	if err != nil || got != m.SHA256 {
		t.Fatalf("final hash %s err %v", got, err)
	}
	// A completed receiver is a full seed.
	have, err := Have(dest, m)
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range have {
		if !h {
			t.Fatalf("Have: chunk %d missing after completion", i)
		}
	}
}

func TestResume(t *testing.T) {
	dir := t.TempDir()
	src, data := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.bin")

	// Run 1: cancel once 8 of 12 chunks are stored.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	sc1, rc1 := pipe("seed", "recv")
	cnt1 := &reqCounter{Channel: sc1}
	done1 := serveAsync(context.Background(), cnt1, m, src, nil)
	err = Receive(ctx1, rc1, m, ReceiveOptions{Dest: dest, Parallel: 2, OnProgress: func(p Progress) {
		if p.Done >= 8 {
			cancel1()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run 1: want context.Canceled, got %v", err)
	}
	if err := waitErr(t, done1); err == nil {
		t.Fatal("run 1: Serve returned nil after receiver vanished")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("run 1: dest exists after cancel")
	}
	have, err := Have(dest, m)
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, h := range have {
		if h {
			kept++
		}
	}
	if kept < 8 {
		t.Fatalf("sidecar records %d chunks, want >= 8", kept)
	}

	// Run 2: fresh channel, same dest.
	sc2, rc2 := pipe("seed", "recv")
	cnt2 := &reqCounter{Channel: sc2}
	done2 := serveAsync(context.Background(), cnt2, m, src, nil)
	var first Progress
	var once sync.Once
	err = Receive(context.Background(), rc2, m, ReceiveOptions{Dest: dest, Parallel: 2, OnProgress: func(p Progress) {
		once.Do(func() { first = p })
	}})
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if err := waitErr(t, done2); err != nil {
		t.Fatalf("run 2 Serve: %v", err)
	}
	if cnt2.count() != 12-kept {
		t.Fatalf("run 2 requested %d chunks, want %d", cnt2.count(), 12-kept)
	}
	if cnt2.count() >= cnt1.count() {
		t.Fatalf("run 2 requested %d chunks, run 1 %d; expected fewer", cnt2.count(), cnt1.count())
	}
	if first.Done != kept+1 {
		t.Fatalf("resumed progress Done = %d, want %d", first.Done, kept+1)
	}
	assertFile(t, dest, data)
}

func TestCorruptSingleSeedFails(t *testing.T) {
	dir := t.TempDir()
	src, _ := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.bin")
	sc, rc := pipe("seed", "recv")
	bad := &corrupter{Channel: sc, target: 0}
	done := serveAsync(context.Background(), bad, m, src, nil)
	err = Receive(context.Background(), rc, m, ReceiveOptions{Dest: dest, Parallel: 1})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if n := atomic.LoadInt64(&bad.hits); n != MaxChunkFailures {
		t.Fatalf("chunk 0 was requested %d times, want %d", n, MaxChunkFailures)
	}
	waitErr(t, done)
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dest created despite corruption")
	}
}

func TestCorruptSeedRecoveredByHonestSeed(t *testing.T) {
	dir := t.TempDir()
	src, data := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.bin")
	badS, badR := pipe("bad", "recv-bad")
	goodS, goodR := pipe("good", "recv-good")
	bad := &corrupter{Channel: badS, target: -1}
	d1 := serveAsync(context.Background(), bad, m, src, nil)
	d2 := serveAsync(context.Background(), goodS, m, src, nil)
	var mu sync.Mutex
	peers := map[string]int{}
	err = ReceiveMulti(context.Background(), []Channel{badR, goodR}, m, ReceiveOptions{Dest: dest, Parallel: 2, OnProgress: func(p Progress) {
		mu.Lock()
		peers[p.Peer]++
		mu.Unlock()
	}})
	if err != nil {
		t.Fatalf("ReceiveMulti: %v", err)
	}
	waitErr(t, d1)
	if err := waitErr(t, d2); err != nil {
		t.Fatalf("honest Serve: %v", err)
	}
	if atomic.LoadInt64(&bad.hits) == 0 {
		t.Fatal("corrupting seed never sent anything")
	}
	if peers["recv-bad"] != 0 || peers["recv-good"] != 12 {
		t.Fatalf("chunks accepted per peer: %v", peers)
	}
	assertFile(t, dest, data)
}

// makePartialSeed writes dest.part holding only the chunks where keep(i)
// is true, plus a matching sidecar.
func makePartialSeed(t *testing.T, dest string, m Manifest, data []byte, keep func(int) bool) {
	t.Helper()
	part := make([]byte, len(data))
	have := make([]bool, m.NumChunks())
	for i := range have {
		if keep(i) {
			off := i * m.ChunkSize
			copy(part[off:off+m.chunkLen(i)], data[off:off+m.chunkLen(i)])
			have[i] = true
		}
	}
	if err := os.WriteFile(partPath(dest), part, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSidecar(dest, m, have); err != nil {
		t.Fatal(err)
	}
}

func TestReceiveMultiHalfSeeds(t *testing.T) {
	dir := t.TempDir()
	src, data := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	makePartialSeed(t, a, m, data, func(i int) bool { return i%2 == 0 })
	makePartialSeed(t, b, m, data, func(i int) bool { return i%2 == 1 })
	ha, _ := Have(a, m)
	if ha[0] != true || ha[1] != false {
		t.Fatalf("Have on partial seed: %v", ha)
	}

	aS, aR := pipe("a", "recv-a")
	bS, bR := pipe("b", "recv-b")
	da := serveAsync(context.Background(), aS, m, a, nil)
	db := serveAsync(context.Background(), bS, m, b, nil)
	dest := filepath.Join(dir, "out.bin")
	var mu sync.Mutex
	peers := map[string]int{}
	err = ReceiveMulti(context.Background(), []Channel{aR, bR}, m, ReceiveOptions{Dest: dest, OnProgress: func(p Progress) {
		mu.Lock()
		peers[p.Peer]++
		mu.Unlock()
	}})
	if err != nil {
		t.Fatalf("ReceiveMulti: %v", err)
	}
	if err := waitErr(t, da); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := waitErr(t, db); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	if peers["recv-a"] != 6 || peers["recv-b"] != 6 {
		t.Fatalf("chunks per seed: %v", peers)
	}
	assertFile(t, dest, data)
}

func TestSeedDropsMidTransfer(t *testing.T) {
	dir := t.TempDir()
	src, data := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	flakyS, flakyR := pipe("flaky", "recv-flaky")
	goodS, goodR := pipe("good", "recv-good")
	flakyCtx, dropFlaky := context.WithCancel(context.Background())
	defer dropFlaky()
	d1 := serveAsync(flakyCtx, flakyS, m, src, func(p Progress) {
		if p.Done == 2 {
			dropFlaky()
		}
	})
	d2 := serveAsync(context.Background(), goodS, m, src, nil)
	dest := filepath.Join(dir, "out.bin")
	err = ReceiveMulti(context.Background(), []Channel{flakyR, goodR}, m, ReceiveOptions{Dest: dest, Parallel: 2})
	if err != nil {
		t.Fatalf("ReceiveMulti: %v", err)
	}
	waitErr(t, d1)
	if err := waitErr(t, d2); err != nil {
		t.Fatalf("good seed: %v", err)
	}
	assertFile(t, dest, data)
}

func TestWrongManifestIDRefused(t *testing.T) {
	dir := t.TempDir()
	src, _ := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	other := m
	other.ID = "not-the-same-file"
	sc, rc := pipe("seed", "recv")
	done := serveAsync(context.Background(), sc, m, src, nil)
	dest := filepath.Join(dir, "out.bin")
	err = Receive(context.Background(), rc, other, ReceiveOptions{Dest: dest})
	if err == nil || !errors.Is(err, ErrNoSeeds) {
		t.Fatalf("receiver: want ErrNoSeeds (seed refused), got %v", err)
	}
	if err := waitErr(t, done); !errors.Is(err, ErrBadManifest) {
		t.Fatalf("seed: want ErrBadManifest, got %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dest created")
	}
}

func TestErrExists(t *testing.T) {
	dir := t.TempDir()
	src, data := randomFile(t, dir, testSize)
	m, err := BuildManifest(src, "priya")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, rc := pipe("seed", "recv")
	err = Receive(context.Background(), rc, m, ReceiveOptions{Dest: dest})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	sc.Close()

	sc, rc = pipe("seed", "recv")
	done := serveAsync(context.Background(), sc, m, src, nil)
	err = Receive(context.Background(), rc, m, ReceiveOptions{Dest: dest, Overwrite: true})
	if err != nil {
		t.Fatalf("Overwrite: %v", err)
	}
	if err := waitErr(t, done); err != nil {
		t.Fatal(err)
	}
	assertFile(t, dest, data)
}

func TestFrameBounds(t *testing.T) {
	// A frame claiming more than MaxFrame is rejected before allocation.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	fr := newFrameReader(bytes.NewReader(hdr[:]))
	if _, err := fr.read(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol, got %v", err)
	}
	// Manifest with an oversize chunk size is refused.
	m := Manifest{ID: "x", Size: 1, ChunkSize: ChunkSize + 1, SHA256: hex.EncodeToString(make([]byte, 32)), Chunks: []string{hex.EncodeToString(make([]byte, 32))}}
	if err := m.Validate(); !errors.Is(err, ErrBadManifest) {
		t.Fatalf("want ErrBadManifest, got %v", err)
	}
}
