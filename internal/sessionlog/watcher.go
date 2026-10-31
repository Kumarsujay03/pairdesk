package sessionlog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	maxSnapshot = 1 << 20 // 1 MiB: larger files are logged but not diffed
	maxCache    = 64
	maxDepth    = 6
)

// Watcher records file activity under one or more roots with inotify.
type Watcher struct {
	w        *Writer
	fd       int
	mu       sync.Mutex
	wds      map[int]string
	paths    map[string]int
	exclude  []string
	snaps    map[string][]byte // path -> content at open time
	snapOrder []string
	done     chan struct{}
}

// NewWatcher starts watching root (recursively, bounded depth) and any
// paths later passed to Add. exclude lists path prefixes never watched
// (the PairDesk config and log directories).
func NewWatcher(ctx context.Context, w *Writer, root string, exclude []string) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	wt := &Watcher{w: w, fd: fd, wds: map[int]string{}, paths: map[string]int{}, exclude: exclude, snaps: map[string][]byte{}, done: make(chan struct{})}
	wt.addTree(root, 0)
	go wt.loop(ctx)
	return wt, nil
}

// Add watches another directory tree (the visitor cd'd somewhere new).
func (wt *Watcher) Add(dir string) { wt.addTree(dir, maxDepth-2) }

func (wt *Watcher) excluded(p string) bool {
	for _, e := range wt.exclude {
		if p == e || strings.HasPrefix(p, e+string(os.PathSeparator)) {
			return true
		}
	}
	base := filepath.Base(p)
	switch base {
	case ".git", ".cache", "node_modules", ".npm", ".cargo", "target", ".venv", "__pycache__", ".local/share/Trash":
		return true
	}
	return strings.HasPrefix(p, "/proc") || strings.HasPrefix(p, "/sys") || strings.HasPrefix(p, "/dev")
}

func (wt *Watcher) addTree(dir string, depth int) {
	if depth > maxDepth || wt.excluded(dir) {
		return
	}
	wt.mu.Lock()
	if _, ok := wt.paths[dir]; ok {
		wt.mu.Unlock()
		return
	}
	wd, err := unix.InotifyAddWatch(wt.fd, dir, unix.IN_OPEN|unix.IN_CLOSE_WRITE|unix.IN_CREATE|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO|unix.IN_ONLYDIR|unix.IN_EXCL_UNLINK)
	if err != nil {
		wt.mu.Unlock()
		return
	}
	wt.wds[wd] = dir
	wt.paths[dir] = wd
	wt.mu.Unlock()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			wt.addTree(filepath.Join(dir, e.Name()), depth+1)
		}
	}
}

func (wt *Watcher) loop(ctx context.Context) {
	defer close(wt.done)
	buf := make([]byte, 64<<10)
	pfd := []unix.PollFd{{Fd: int32(wt.fd), Events: unix.POLLIN}}
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := unix.Poll(pfd, 250)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		if n == 0 {
			continue
		}
		n, err = unix.Read(wt.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			return
		}
		off := 0
		for off+unix.SizeofInotifyEvent <= n {
			ev := (*unix.InotifyEvent)(unsafePointer(buf[off:]))
			nameLen := int(ev.Len)
			name := ""
			if nameLen > 0 {
				name = strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+nameLen]), "\x00")
			}
			off += unix.SizeofInotifyEvent + nameLen
			wt.mu.Lock()
			dir := wt.wds[int(ev.Wd)]
			wt.mu.Unlock()
			if dir == "" {
				continue
			}
			wt.handle(dir, name, ev.Mask)
		}
	}
}

func (wt *Watcher) handle(dir, name string, mask uint32) {
	p := filepath.Join(dir, name)
	isDir := mask&unix.IN_ISDIR != 0
	switch {
	case mask&unix.IN_CREATE != 0 || mask&unix.IN_MOVED_TO != 0:
		if isDir {
			wt.addTree(p, maxDepth-2)
			return
		}
		_ = wt.w.File(FileEvent{Path: p, Event: "create"})
	case mask&unix.IN_DELETE != 0 || mask&unix.IN_MOVED_FROM != 0:
		if isDir {
			return
		}
		_ = wt.w.File(FileEvent{Path: p, Event: "delete"})
		wt.dropSnap(p)
	case mask&unix.IN_OPEN != 0:
		if isDir || name == "" {
			return
		}
		wt.snapshot(p)
		_ = wt.w.File(FileEvent{Path: p, Event: "open"})
	case mask&unix.IN_CLOSE_WRITE != 0:
		if isDir || name == "" {
			return
		}
		diff := wt.diff(p)
		_ = wt.w.File(FileEvent{Path: p, Event: "modify", Diff: diff})
	}
}

func (wt *Watcher) snapshot(p string) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxSnapshot {
		return
	}
	wt.mu.Lock()
	if _, ok := wt.snaps[p]; ok {
		wt.mu.Unlock()
		return
	}
	wt.mu.Unlock()
	b, err := os.ReadFile(p)
	if err != nil || !looksText(b) {
		return
	}
	wt.mu.Lock()
	defer wt.mu.Unlock()
	if len(wt.snapOrder) >= maxCache {
		old := wt.snapOrder[0]
		wt.snapOrder = wt.snapOrder[1:]
		delete(wt.snaps, old)
	}
	wt.snaps[p] = b
	wt.snapOrder = append(wt.snapOrder, p)
}

func (wt *Watcher) dropSnap(p string) {
	wt.mu.Lock()
	delete(wt.snaps, p)
	wt.mu.Unlock()
}

// diff produces a unified diff between the snapshot and the file now.
func (wt *Watcher) diff(p string) string {
	wt.mu.Lock()
	before, had := wt.snaps[p]
	delete(wt.snaps, p)
	wt.mu.Unlock()
	after, err := os.ReadFile(p)
	if err != nil || len(after) > maxSnapshot || !looksText(after) {
		return ""
	}
	if !had {
		before = nil
	}
	if bytes.Equal(before, after) {
		return ""
	}
	out := unifiedDiff(p, before, after)
	rel, err := wt.w.WriteDiff(filepath.Base(p)+".diff", out)
	if err != nil {
		return ""
	}
	return rel
}

// unifiedDiff shells out to diff(1) when present, else stores both versions.
func unifiedDiff(p string, before, after []byte) []byte {
	if diffBin, err := exec.LookPath("diff"); err == nil {
		dir, err := os.MkdirTemp("", "pdiff")
		if err == nil {
			defer os.RemoveAll(dir)
			a := filepath.Join(dir, "a")
			b := filepath.Join(dir, "b")
			_ = os.WriteFile(a, before, 0o600)
			_ = os.WriteFile(b, after, 0o600)
			cmd := exec.Command(diffBin, "-u", "--label", "before "+p, "--label", "after "+p, a, b)
			out, _ := cmd.Output()
			if len(out) > 0 {
				return out
			}
		}
	}
	var sb bytes.Buffer
	sb.WriteString("--- before " + p + "\n+++ after " + p + "\n")
	for _, l := range bytes.Split(before, []byte("\n")) {
		sb.WriteString("-" + string(l) + "\n")
	}
	for _, l := range bytes.Split(after, []byte("\n")) {
		sb.WriteString("+" + string(l) + "\n")
	}
	return sb.Bytes()
}

func looksText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	sample := b
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	return utf8.Valid(sample) || utf8.Valid(sample[:len(sample)-3])
}

// Close stops watching.
func (wt *Watcher) Close() {
	_ = unix.Close(wt.fd)
	<-wt.done
}
