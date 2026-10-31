package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// Channel is the peer stream a transfer runs over. It is the same shape as
// internal/p2p's Channel; anything satisfying it works. Bytes arrive in
// order with no message boundaries.
type Channel interface {
	io.ReadWriteCloser
	Label() string
}

var (
	// ErrExists means dest already exists and Overwrite was not set.
	ErrExists = errors.New("transfer: destination exists")
	// ErrCorrupt means the same chunk failed verification MaxChunkFailures
	// times.
	ErrCorrupt = errors.New("transfer: chunk failed verification repeatedly")
	// ErrNoSeeds means every seed dropped or none holds a chunk still needed.
	ErrNoSeeds = errors.New("transfer: no seed can supply the remaining chunks")
	// ErrHashMismatch means the assembled file's SHA-256 is not the
	// manifest's. The sidecar is reset so a retry starts over.
	ErrHashMismatch = errors.New("transfer: whole-file hash mismatch")
)

const (
	// MaxChunkFailures aborts the transfer once one chunk has been
	// delivered corrupt this many times.
	MaxChunkFailures = 3
	// maxSeedCorrupt drops a seed after this many corrupt chunks from it.
	maxSeedCorrupt = 3
	// maxQueuedReqs bounds how many REQs a seed will hold for a receiver.
	maxQueuedReqs = 256
	// DefaultParallel is the per-seed number of outstanding requests.
	DefaultParallel = 8
)

// Progress is reported after every chunk. On the seed side Done counts
// chunks sent; on the receiver side it counts chunks held (including those
// resumed from the sidecar). Bytes and Rate cover this call only. Peer is
// the Label of the channel the chunk crossed.
type Progress struct {
	Done  int
	Total int
	Bytes int64
	Rate  float64 // bytes per second
	Peer  string
}

type rate struct {
	start time.Time
	bytes int64
}

func (r *rate) add(n int) (int64, float64) {
	r.bytes += int64(n)
	el := time.Since(r.start).Seconds()
	if el <= 0 {
		return r.bytes, 0
	}
	return r.bytes, float64(r.bytes) / el
}

// closeOnCancel closes chs when ctx is cancelled, which unblocks any
// pending Read or Write. Call the returned function when done.
func closeOnCancel(ctx context.Context, chs ...Channel) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			for _, ch := range chs {
				ch.Close()
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

// Serve seeds one file over ch until the receiver sends DONE, the channel
// breaks or ctx is cancelled. path is either the complete file or the
// destination of a (possibly partial) Receive, whose ".part" and sidecar
// are then served; the receiver is told which chunks are available. The
// channel is closed when Serve returns.
func Serve(ctx context.Context, ch Channel, m Manifest, path string, onProgress func(Progress)) error {
	defer ch.Close()
	if err := m.Validate(); err != nil {
		return err
	}
	file, have, err := locate(path, m)
	if err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	stop := closeOnCancel(ctx, ch)
	defer stop()
	ctxErr := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

	fr := newFrameReader(ch)
	hello, err := fr.read()
	if err != nil {
		return ctxErr(err)
	}
	if hello.typ != typeHello {
		writeErr(ch, "expected HELLO")
		return fmt.Errorf("%w: expected HELLO, got type %d", ErrProtocol, hello.typ)
	}
	if string(hello.payload) != m.ID {
		writeErr(ch, "unknown manifest")
		return fmt.Errorf("%w: receiver asked for manifest %q, serving %q", ErrBadManifest, hello.payload, m.ID)
	}
	if err := writeFrame(ch, typeHave, packBits(have)); err != nil {
		return ctxErr(err)
	}

	reqs := make(chan int, maxQueuedReqs)
	errc := make(chan error, 1)
	go func() {
		for {
			f, err := fr.read()
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				errc <- err
				return
			}
			switch f.typ {
			case typeReq:
				idx := int(uint32(f.payload[0])<<24 | uint32(f.payload[1])<<16 | uint32(f.payload[2])<<8 | uint32(f.payload[3]))
				select {
				case reqs <- idx:
				default:
					errc <- fmt.Errorf("%w: too many outstanding requests", ErrProtocol)
					return
				}
			case typeDone:
				errc <- nil
				return
			case typeErr:
				errc <- fmt.Errorf("transfer: peer error: %s", f.payload)
				return
			default:
				errc <- fmt.Errorf("%w: unexpected frame type %d", ErrProtocol, f.typ)
				return
			}
		}
	}()

	n := m.NumChunks()
	buf := make([]byte, m.ChunkSize)
	rt := rate{start: time.Now()}
	sent := 0
	for {
		select {
		case err := <-errc:
			return ctxErr(err)
		case idx := <-reqs:
			if idx < 0 || idx >= n || !have[idx] {
				writeErr(ch, fmt.Sprintf("chunk %d not available", idx))
				return fmt.Errorf("%w: chunk %d not available", ErrProtocol, idx)
			}
			l := m.chunkLen(idx)
			if _, err := f.ReadAt(buf[:l], int64(idx)*int64(m.ChunkSize)); err != nil {
				writeErr(ch, "read failed")
				return err
			}
			if err := writeFrame(ch, typeData, u32(uint32(idx)), buf[:l]); err != nil {
				return ctxErr(err)
			}
			sent++
			if onProgress != nil {
				b, r := rt.add(l)
				onProgress(Progress{Done: sent, Total: n, Bytes: b, Rate: r, Peer: ch.Label()})
			}
		}
	}
}

// ReceiveOptions configures Receive and ReceiveMulti.
type ReceiveOptions struct {
	Dest       string
	Overwrite  bool
	OnProgress func(Progress)
	Parallel   int // outstanding chunk requests per seed; default 8
}

// Receive pulls the file described by m from one seed. See ReceiveMulti.
func Receive(ctx context.Context, ch Channel, m Manifest, opt ReceiveOptions) error {
	return ReceiveMulti(ctx, []Channel{ch}, m, opt)
}

type seed struct {
	ch       Channel
	label    string
	have     []bool // nil until HAVE arrives
	alive    bool
	bad      map[int]bool // chunks this seed delivered corrupt
	corrupt  int
	inflight int
}

type receiver struct {
	m   Manifest
	opt ReceiveOptions
	n   int

	mu        sync.Mutex
	cond      *sync.Cond
	have      []bool
	inflight  map[int]*seed
	failures  map[int]int
	seeds     []*seed
	done      bool
	cancelled bool
	err       error
	lastErr   error
	part      *os.File
	rt        rate
}

// ReceiveMulti pulls the file described by m from several seeds at once.
// Each seed is asked which chunks it holds; missing chunks are requested
// rarest-first across seeds with at most opt.Parallel outstanding per seed.
// A seed that drops mid-transfer has its outstanding chunks reassigned; a
// chunk delivered corrupt is re-requested from another seed, and the
// transfer aborts after MaxChunkFailures failures on the same chunk.
//
// Data is written into Dest+".part" with a Dest+".part.json" bitmap so a
// later call with the same Dest resumes. On completion the whole-file hash
// is verified and the file renamed to Dest. ErrExists is returned when Dest
// exists unless Overwrite is set. Every channel is closed when the call
// returns.
func ReceiveMulti(ctx context.Context, chs []Channel, m Manifest, opt ReceiveOptions) error {
	defer func() {
		for _, ch := range chs {
			ch.Close()
		}
	}()
	if err := m.Validate(); err != nil {
		return err
	}
	if opt.Dest == "" {
		return errors.New("transfer: Dest is required")
	}
	if opt.Parallel <= 0 {
		opt.Parallel = DefaultParallel
	}
	if _, err := os.Stat(opt.Dest); err == nil && !opt.Overwrite {
		return ErrExists
	}
	if len(chs) == 0 && m.NumChunks() > 0 {
		return ErrNoSeeds
	}

	r := &receiver{m: m, opt: opt, n: m.NumChunks(), inflight: map[int]*seed{}, failures: map[int]int{}}
	r.cond = sync.NewCond(&r.mu)
	r.have = loadSidecar(opt.Dest, m)

	part, err := os.OpenFile(partPath(opt.Dest), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	r.part = part
	if st, err := part.Stat(); err != nil || st.Size() != m.Size {
		if err := part.Truncate(m.Size); err != nil {
			part.Close()
			return err
		}
	}
	if err := writeSidecar(opt.Dest, m, r.have); err != nil {
		part.Close()
		return err
	}
	r.done = r.allHave()
	r.rt = rate{start: time.Now()}

	if !r.done {
		for _, ch := range chs {
			r.seeds = append(r.seeds, &seed{ch: ch, label: ch.Label(), alive: true, bad: map[int]bool{}})
		}
		stop := closeOnCancel(ctx, chs...)
		cancelWatch := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				r.mu.Lock()
				r.cancelled = true
				r.cond.Broadcast()
				r.mu.Unlock()
			case <-cancelWatch:
			}
		}()
		var wg sync.WaitGroup
		for _, s := range r.seeds {
			wg.Add(1)
			go func(s *seed) {
				defer wg.Done()
				err := r.runSeed(s)
				r.seedExit(s, err)
			}(s)
		}
		wg.Wait()
		close(cancelWatch)
		stop()

		if ctx.Err() != nil {
			part.Close()
			return ctx.Err()
		}
		r.mu.Lock()
		err, done := r.err, r.done
		r.mu.Unlock()
		if err != nil {
			part.Close()
			return err
		}
		if !done {
			part.Close()
			return ErrNoSeeds
		}
	}
	return r.finish()
}

func (r *receiver) allHave() bool {
	for _, h := range r.have {
		if !h {
			return false
		}
	}
	return true
}

func (r *receiver) countHave() int {
	c := 0
	for _, h := range r.have {
		if h {
			c++
		}
	}
	return c
}

// fail records the first fatal error and wakes every seed. Callers hold mu.
func (r *receiver) fail(err error) {
	if r.err == nil {
		r.err = err
	}
	r.cond.Broadcast()
}

// seedExit drops a seed, reassigning its outstanding chunks.
func (r *receiver) seedExit(s *seed, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.alive = false
	s.inflight = 0
	for idx, sd := range r.inflight {
		if sd == s {
			delete(r.inflight, idx)
		}
	}
	if err != nil {
		r.lastErr = fmt.Errorf("transfer: seed %s: %w", s.label, err)
	}
	if !r.done && r.err == nil && r.liveSeeds() == 0 {
		if r.lastErr != nil {
			r.fail(fmt.Errorf("%w: %v", ErrNoSeeds, r.lastErr))
		} else {
			r.fail(ErrNoSeeds)
		}
	}
	r.cond.Broadcast()
}

func (r *receiver) liveSeeds() int {
	c := 0
	for _, s := range r.seeds {
		if s.alive {
			c++
		}
	}
	return c
}

// availability counts live seeds known to hold chunk i.
func (r *receiver) availability(i int) int {
	c := 0
	for _, s := range r.seeds {
		if s.alive && s.have != nil && s.have[i] {
			c++
		}
	}
	return c
}

// otherHas reports whether a live seed other than s holds chunk i.
func (r *receiver) otherHas(i int, s *seed) bool {
	for _, t := range r.seeds {
		if t != s && t.alive && t.have != nil && t.have[i] {
			return true
		}
	}
	return false
}

// pick assigns up to k missing chunks to s, rarest first. Callers hold mu.
func (r *receiver) pick(s *seed, k int) []int {
	if k <= 0 || s.have == nil {
		return nil
	}
	type cand struct{ idx, avail int }
	var cands []cand
	for i := 0; i < r.n; i++ {
		if r.have[i] || r.inflight[i] != nil || !s.have[i] {
			continue
		}
		if s.bad[i] && r.otherHas(i, s) {
			continue // let a seed that has not corrupted it try
		}
		cands = append(cands, cand{i, r.availability(i)})
	}
	sort.Slice(cands, func(a, b int) bool {
		if cands[a].avail != cands[b].avail {
			return cands[a].avail < cands[b].avail
		}
		return cands[a].idx < cands[b].idx
	})
	if len(cands) > k {
		cands = cands[:k]
	}
	out := make([]int, 0, len(cands))
	for _, c := range cands {
		r.inflight[c.idx] = s
		s.inflight++
		out = append(out, c.idx)
	}
	return out
}

// stalled reports whether some missing chunk can never arrive: every live
// seed has announced its bitmap and none holds it. Callers hold mu.
func (r *receiver) stalled() (int, bool) {
	for _, s := range r.seeds {
		if s.alive && s.have == nil {
			return 0, false // still waiting for a HAVE
		}
	}
	for i := 0; i < r.n; i++ {
		if !r.have[i] && r.inflight[i] == nil && r.availability(i) == 0 {
			return i, true
		}
	}
	return 0, false
}

func (r *receiver) runSeed(s *seed) error {
	ch := s.ch
	if err := writeFrame(ch, typeHello, []byte(r.m.ID)); err != nil {
		return err
	}
	fr := newFrameReader(ch)
	f, err := fr.read()
	if err != nil {
		return err
	}
	switch f.typ {
	case typeHave:
		bits, ok := unpackBits(f.payload, r.n)
		if !ok {
			return fmt.Errorf("%w: HAVE bitmap length %d", ErrProtocol, len(f.payload))
		}
		r.mu.Lock()
		s.have = bits
		r.cond.Broadcast()
		r.mu.Unlock()
	case typeErr:
		return fmt.Errorf("transfer: seed refused: %s", f.payload)
	default:
		return fmt.Errorf("%w: expected HAVE, got type %d", ErrProtocol, f.typ)
	}

	for {
		r.mu.Lock()
		var picked []int
		for {
			if r.done || r.cancelled || r.err != nil {
				r.mu.Unlock()
				if r.done {
					writeFrame(ch, typeDone)
				}
				return nil
			}
			picked = r.pick(s, r.opt.Parallel-s.inflight)
			if len(picked) > 0 || s.inflight > 0 {
				break
			}
			if i, ok := r.stalled(); ok {
				r.fail(fmt.Errorf("%w: chunk %d", ErrNoSeeds, i))
				continue
			}
			r.cond.Wait()
		}
		r.mu.Unlock()

		for _, idx := range picked {
			if err := writeFrame(ch, typeReq, u32(uint32(idx))); err != nil {
				return err
			}
		}
		f, err := fr.read()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		switch f.typ {
		case typeData:
			idx := int(uint32(f.payload[0])<<24 | uint32(f.payload[1])<<16 | uint32(f.payload[2])<<8 | uint32(f.payload[3]))
			if err := r.gotData(s, idx, f.payload[4:]); err != nil {
				return err
			}
		case typeErr:
			return fmt.Errorf("transfer: seed error: %s", f.payload)
		default:
			return fmt.Errorf("%w: unexpected frame type %d", ErrProtocol, f.typ)
		}
	}
}

// gotData verifies and stores one DATA frame from s.
func (r *receiver) gotData(s *seed, idx int, data []byte) error {
	r.mu.Lock()
	if idx < 0 || idx >= r.n || r.inflight[idx] != s {
		r.mu.Unlock()
		return fmt.Errorf("%w: unrequested chunk %d", ErrProtocol, idx)
	}
	delete(r.inflight, idx)
	s.inflight--
	if !r.m.chunkMatches(idx, data) {
		r.failures[idx]++
		s.bad[idx] = true
		s.corrupt++
		if r.failures[idx] >= MaxChunkFailures {
			r.fail(fmt.Errorf("%w: chunk %d", ErrCorrupt, idx))
			r.mu.Unlock()
			return fmt.Errorf("corrupt chunk %d", idx)
		}
		r.cond.Broadcast()
		dropped := s.corrupt >= maxSeedCorrupt
		r.mu.Unlock()
		if dropped {
			return fmt.Errorf("sent %d corrupt chunks", s.corrupt)
		}
		return nil
	}
	if _, err := r.part.WriteAt(data, int64(idx)*int64(r.m.ChunkSize)); err != nil {
		r.fail(err)
		r.mu.Unlock()
		return err
	}
	r.have[idx] = true
	if err := writeSidecar(r.opt.Dest, r.m, r.have); err != nil {
		r.fail(err)
		r.mu.Unlock()
		return err
	}
	if r.allHave() {
		r.done = true
	}
	b, rt := r.rt.add(len(data))
	p := Progress{Done: r.countHave(), Total: r.n, Bytes: b, Rate: rt, Peer: s.label}
	r.cond.Broadcast()
	r.mu.Unlock()
	if r.opt.OnProgress != nil {
		r.opt.OnProgress(p)
	}
	return nil
}

// finish verifies the assembled file and moves it into place.
func (r *receiver) finish() error {
	if err := r.part.Sync(); err != nil {
		r.part.Close()
		return err
	}
	if err := r.part.Close(); err != nil {
		return err
	}
	sum, err := hashFile(partPath(r.opt.Dest))
	if err != nil {
		return err
	}
	if sum != r.m.SHA256 {
		writeSidecar(r.opt.Dest, r.m, make([]bool, r.n))
		return ErrHashMismatch
	}
	if _, err := os.Stat(r.opt.Dest); err == nil && !r.opt.Overwrite {
		return ErrExists
	}
	if err := os.Rename(partPath(r.opt.Dest), r.opt.Dest); err != nil {
		return err
	}
	os.Remove(sidecarPath(r.opt.Dest))
	return nil
}
