package term

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/p2p"
)

// HostOptions configures StartHost.
type HostOptions struct {
	Shell    string         // default $SHELL or /bin/bash
	ReadOnly bool           // watch: discard visitor input
	SudoTier model.SudoTier // blocked (default), ask or auto; see PAIRDESK.md 4.7
	ShimDir  string         // where the sudo shim, rc files and FIFO live (default: private temp dir, 0700)

	OnCommand func(CommandEvent)      // each command the visitor ran, once its exit code is known
	OnOutput  func(time.Time, []byte) // redacted output as it is sent to the visitor (for asciicast)

	// OnSudo is consulted in the blocked tier: return true to allow this
	// one command (the host's consent card). Nil means deny.
	OnSudo func(cmd string) bool
	// SudoPassword supplies the host's password for sudo in the auto tier
	// and for consented commands in the blocked tier. It is handed to real
	// sudo through SUDO_ASKPASS and never written to the transcript or log.
	// When nil in the blocked tier, a consented command runs real sudo with
	// its prompt shown to the visitor, who must then know the password.
	SudoPassword func() (string, bool)
	// OnShareRequest is called when the visitor asks for a shared terminal
	// (frame 4). Return true to enable it. Nil denies.
	OnShareRequest func() bool

	Env []string // default os.Environ()
	Dir string   // default $HOME

	realSudo string // tests only: path used as the real sudo binary
}

// Host is one PTY session served to a visitor.
type Host struct {
	opt    HostOptions
	ch     p2p.Channel
	cmd    *exec.Cmd
	ptmx   *os.File
	shim   string
	ownDir bool
	events *os.File
	marker string

	wmu sync.Mutex // channel writes

	stateMu  sync.Mutex
	cur      *CommandEvent // command in flight (hook "pre" seen)
	tokens   map[string]bool
	sudoCmds map[string]string
	typist   string
	shared   bool

	transcript *teeBuffer
	redact     *redactor

	done     chan struct{}
	doneOnce sync.Once
	stopOnce sync.Once
	err      error
}

// StartHost spawns the shell on a PTY and bridges it over ch. The shell's
// output flows to the visitor as data frames and to the transcript tee;
// visitor data frames are written to the PTY unless ReadOnly.
func StartHost(ctx context.Context, ch p2p.Channel, opt HostOptions) (*Host, error) {
	if opt.SudoTier == "" {
		opt.SudoTier = model.SudoBlocked
	}
	shell := opt.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/bash"
	}
	h := &Host{
		opt:        opt,
		ch:         ch,
		tokens:     map[string]bool{},
		sudoCmds:   map[string]string{},
		typist:     "visitor",
		transcript: newTeeBuffer(),
		done:       make(chan struct{}),
	}
	// The marker is an OSC sequence terminals ignore, so the visitor sees a
	// normal prompt while the transcript can spot it reliably.
	h.marker = "\x1b]pairdesk-sudo-" + hex.EncodeToString([]byte(crypto.RandomID(6))) + "\x07[sudo] password for host: "
	h.redact = newRedactor(writerFunc(func(p []byte) (int, error) {
		h.transcript.Write(p)
		if opt.OnOutput != nil {
			opt.OnOutput(time.Now(), append([]byte{}, p...))
		}
		return len(p), nil
	}), h.marker)

	dir := opt.ShimDir
	if dir == "" {
		d, err := os.MkdirTemp("", "pairdesk-shim-")
		if err != nil {
			return nil, err
		}
		dir = d
		h.ownDir = true
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	h.shim = dir
	if err := writeShimDir(dir); err != nil {
		h.cleanupDir()
		return nil, err
	}
	ev, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_RDWR, 0)
	if err != nil {
		h.cleanupDir()
		return nil, err
	}
	h.events = ev

	realSudo := opt.realSudo
	if realSudo == "" {
		if p, err := exec.LookPath("sudo"); err == nil {
			realSudo = p
		} else {
			realSudo = "/usr/bin/sudo"
		}
	}

	env := opt.Env
	if env == nil {
		env = os.Environ()
	}
	envm := map[string]string{}
	var order []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, seen := envm[k]; !seen {
			order = append(order, k)
		}
		envm[k] = v
	}
	set := func(k, v string) {
		if _, seen := envm[k]; !seen {
			order = append(order, k)
		}
		envm[k] = v
	}
	set("PATH", dir+string(os.PathListSeparator)+envm["PATH"])
	set(envHook, filepath.Join(dir, fileEvents))
	set(envShimDir, dir)
	set(envRealSudo, realSudo)
	set(envMarker, h.marker)
	if envm["TERM"] == "" {
		set("TERM", "xterm-256color")
	}

	var args []string
	switch filepath.Base(shell) {
	case "bash":
		args = []string{"--rcfile", filepath.Join(dir, fileBashrc), "-i"}
	case "zsh":
		set("ZDOTDIR", filepath.Join(dir, dirZsh))
		args = []string{"-i"}
	case "fish":
		args = []string{"-i", "--init-command", "source " + shellQuote(filepath.Join(dir, fileFish))}
	default:
		args = []string{"-i"}
	}
	cmd := exec.Command(shell, args...)
	cmd.Dir = opt.Dir
	if cmd.Dir == "" {
		cmd.Dir = envm["HOME"]
	}
	for _, k := range order {
		cmd.Env = append(cmd.Env, k+"="+envm[k])
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		_ = ev.Close()
		h.cleanupDir()
		return nil, err
	}
	h.cmd, h.ptmx = cmd, ptmx

	go h.pumpOutput()
	go h.pumpInput()
	go h.pumpEvents()
	go func() {
		select {
		case <-ctx.Done():
			_ = h.Stop()
		case <-h.done:
		}
	}()
	return h, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (h *Host) cleanupDir() {
	if h.ownDir {
		_ = os.RemoveAll(h.shim)
	} else {
		for _, f := range []string{fileEvents, fileSudo, fileAskpass, fileBashrc, fileFish} {
			_ = os.Remove(filepath.Join(h.shim, f))
		}
		_ = os.RemoveAll(filepath.Join(h.shim, dirZsh))
	}
}

func (h *Host) send(typ byte, payload []byte) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	return WriteFrame(h.ch, typ, payload)
}

// pumpOutput copies PTY output to the transcript and the visitor. When the
// shell exits the PTY read fails and the session is torn down.
func (h *Host) pumpOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			_, _ = h.redact.Write(buf[:n])
			if err := h.send(FrameData, buf[:n]); err != nil {
				h.finish(err)
				return
			}
		}
		if err != nil {
			h.finish(nil)
			return
		}
	}
}

func (h *Host) pumpInput() {
	for {
		typ, payload, err := ReadFrame(h.ch)
		if err != nil {
			h.finish(err)
			return
		}
		switch typ {
		case FrameData:
			if h.opt.ReadOnly {
				continue
			}
			h.stateMu.Lock()
			h.typist = "visitor"
			h.stateMu.Unlock()
			if _, err := h.ptmx.Write(payload); err != nil {
				h.finish(err)
				return
			}
		case FrameResize:
			if cols, rows, err := decodeResize(payload); err == nil {
				_ = h.Resize(cols, rows)
			}
		case FrameShare:
			on := len(payload) == 1 && payload[0] == 1
			granted := false
			if on && h.opt.OnShareRequest != nil {
				granted = h.opt.OnShareRequest()
			}
			h.SetShared(granted)
		case FrameBye:
			h.finish(nil)
			return
		}
	}
}

// pumpEvents reads lines from the hook FIFO.
func (h *Host) pumpEvents() {
	sc := bufio.NewScanner(h.events)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		h.handleEvent(sc.Text())
	}
}

func (h *Host) handleEvent(line string) {
	f := strings.SplitN(line, "\t", 4)
	switch f[0] {
	case "pre":
		if len(f) < 3 {
			return
		}
		h.stateMu.Lock()
		h.cur = &CommandEvent{Time: time.Now(), Cwd: f[1], Cmd: strings.Join(f[2:], "\t"), Typist: h.typist}
		h.stateMu.Unlock()
	case "post":
		if len(f) < 3 {
			return
		}
		code, _ := strconv.Atoi(f[1])
		h.stateMu.Lock()
		ev := h.cur
		h.cur = nil
		h.stateMu.Unlock()
		if ev == nil {
			return // precmd before any command (first prompt)
		}
		ev.Exit = code
		h.emit(*ev)
	case "sudo":
		if len(f) < 3 {
			return
		}
		id := f[1]
		cmd := strings.Join(f[2:], "\t")
		h.stateMu.Lock()
		if h.cur != nil {
			h.cur.Sudo = true
		}
		h.stateMu.Unlock()
		go h.decideSudo(id, cmd)
	case "sudoexit":
		if len(f) < 3 {
			return
		}
		code, _ := strconv.Atoi(f[2])
		h.stateMu.Lock()
		standalone := h.cur == nil
		h.stateMu.Unlock()
		if standalone {
			// No shell hooks (plain sh): the shim is the only source.
			h.emit(CommandEvent{Time: time.Now(), Cmd: "sudo " + h.sudoCmd(f[1]), Exit: code, Sudo: true, Typist: "visitor"})
		}
		h.forgetSudo(f[1])
	case "pw":
		if len(f) < 3 {
			return
		}
		h.answerPassword(f[1], f[2])
	}
}

func (h *Host) emit(ev CommandEvent) {
	if h.opt.OnCommand != nil {
		h.opt.OnCommand(ev)
	}
	_ = h.send(FrameCommand, encodeCommand(ev))
}

// decideSudo applies the host's tier to one shim request and writes the
// verdict to the shim's reply FIFO.
func (h *Host) decideSudo(id, cmd string) {
	_ = h.send(FrameSudo, []byte(`{"cmd":`+strconv.Quote(cmd)+`}`))
	h.stateMu.Lock()
	h.sudoCmds[id] = cmd
	h.stateMu.Unlock()

	verdict := "deny:pairdesk: sudo needs the host's consent"
	askpass := func() string {
		tok := crypto.RandomID(16)
		h.stateMu.Lock()
		h.tokens[tok] = true
		h.stateMu.Unlock()
		return "askpass:" + tok
	}
	switch h.opt.SudoTier {
	case model.SudoAsk:
		verdict = "allow"
	case model.SudoAuto:
		if h.opt.SudoPassword != nil {
			verdict = askpass()
		} else {
			verdict = "deny:pairdesk: sudo is unavailable: the host has not stored a password"
		}
	default: // blocked
		if h.opt.OnSudo != nil && h.opt.OnSudo(cmd) {
			if h.opt.SudoPassword != nil {
				verdict = askpass()
			} else {
				verdict = "allow"
			}
		}
	}
	h.replyFIFO(filepath.Join(h.shim, "resp."+id), verdict)
}

func (h *Host) answerPassword(id, token string) {
	h.stateMu.Lock()
	ok := h.tokens[token]
	delete(h.tokens, token)
	h.stateMu.Unlock()
	pw := ""
	if ok && h.opt.SudoPassword != nil {
		if p, got := h.opt.SudoPassword(); got {
			pw = p
		}
	}
	h.replyFIFO(filepath.Join(h.shim, "pw."+id), pw)
}

func (h *Host) sudoCmd(id string) string {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.sudoCmds[id]
}

func (h *Host) forgetSudo(id string) {
	h.stateMu.Lock()
	delete(h.sudoCmds, id)
	h.stateMu.Unlock()
}

// replyFIFO writes one line to a FIFO the shim is reading. The open is
// non-blocking and retried briefly so a shim that died cannot wedge the host.
func (h *Host) replyFIFO(path, line string) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, _ = f.WriteString(line + "\n")
			_ = f.Close()
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-h.done:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Input writes bytes typed locally by the host (shared terminal).
func (h *Host) Input(p []byte) error {
	h.stateMu.Lock()
	h.typist = "host"
	h.stateMu.Unlock()
	_, err := h.ptmx.Write(p)
	return err
}

// SetShared announces the shared-terminal state to the visitor.
func (h *Host) SetShared(on bool) {
	h.stateMu.Lock()
	h.shared = on
	h.stateMu.Unlock()
	b := byte(0)
	if on {
		b = 1
	}
	_ = h.send(FrameShare, []byte{b})
}

// Shared reports whether the terminal is currently shared.
func (h *Host) Shared() bool {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.shared
}

// Marker is the sudo password prompt marker for this session.
func (h *Host) Marker() string { return h.marker }

// Resize sets the PTY window size.
func (h *Host) Resize(cols, rows uint16) error {
	return pty.Setsize(h.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Transcript is a tee of everything written to the visitor, with sudo
// password input redacted. Drain it (or use OnOutput) or it grows.
func (h *Host) Transcript() io.Reader { return h.transcript }

// Done closes when the session has ended for any reason.
func (h *Host) Done() <-chan struct{} { return h.done }

// Err reports why the session ended, after Done.
func (h *Host) Err() error {
	<-h.done
	return h.err
}

// Stop is the kill switch: kills the shell's process group, closes the PTY
// and the channel.
func (h *Host) Stop() error {
	h.stopOnce.Do(func() {
		if h.cmd.Process != nil {
			pgid := h.cmd.Process.Pid
			_ = syscall.Kill(-pgid, syscall.SIGHUP)
			go func() {
				select {
				case <-h.done:
				case <-time.After(500 * time.Millisecond):
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
				}
			}()
		}
		_ = h.ptmx.Close()
	})
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		return errors.New("term: shell did not exit")
	}
	return nil
}

// finish tears everything down exactly once.
func (h *Host) finish(err error) {
	h.doneOnce.Do(func() {
		h.err = err
		_ = h.send(FrameBye, nil)
		_ = h.ch.Close()
		if h.cmd.Process != nil {
			_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGHUP)
		}
		_ = h.ptmx.Close()
		go func() {
			// Reap; if SIGHUP was ignored, escalate.
			waited := make(chan struct{})
			go func() { _ = h.cmd.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-time.After(500 * time.Millisecond):
				_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
				<-waited
			}
			_ = h.events.Close()
			h.cleanupDir()
		}()
		_ = h.redact.Flush()
		h.transcript.Close()
		close(h.done)
	})
}

// teeBuffer is an in-memory byte queue: Write appends, Read blocks until
// data is available or the buffer is closed.
type teeBuffer struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newTeeBuffer() *teeBuffer {
	t := &teeBuffer{}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *teeBuffer) Write(p []byte) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	t.mu.Unlock()
	t.cond.Broadcast()
}

func (t *teeBuffer) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.buf) == 0 && !t.closed {
		t.cond.Wait()
	}
	if len(t.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

func (t *teeBuffer) Close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.cond.Broadcast()
}
