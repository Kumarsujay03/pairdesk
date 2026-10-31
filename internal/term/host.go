package term

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/p2p"
)

// HostOptions configures StartHost.
type HostOptions struct {
	Shell    string         // default $SHELL or /bin/bash
	ReadOnly bool           // watch: discard visitor input
	SudoTier model.SudoTier // blocked (default), ask or auto; see PAIRDESK.md 4.7
	ShimDir  string         // holds bin/ (shim, rc files) and ipc/ (FIFOs); default: private temp dir, 0700

	// Isolate says whether the visitor shell must run in its own user,
	// PID and mount namespaces (see sandboxPlan). The zero value requires
	// it. ReadOnly sessions follow the same rule.
	Isolate IsolationMode
	// HiddenPaths are directories the visitor shell must not reach: the
	// caller passes the PairDesk config, state and cache dirs. Each is
	// covered by an empty tmpfs inside the sandbox. ShimDir/ipc is always
	// hidden. Note that a consented sudo is still root: hiding protects
	// against the unprivileged visitor, not against the commands the host
	// approves.
	HiddenPaths []string

	OnCommand func(CommandEvent)      // each command the visitor ran, once its exit code is known
	OnOutput  func(time.Time, []byte) // redacted output as it is sent to the visitor (for asciicast)

	// OnSudo is consulted in the blocked tier: return true to allow this
	// one command (the host's consent card). Nil means deny.
	OnSudo func(cmd string) bool
	// SudoPassword supplies the host's password for sudo in the auto tier
	// and for consented commands in the blocked tier. It is handed to real
	// sudo through SUDO_ASKPASS, outside the sandbox, and never written to
	// the transcript, the log or anywhere the visitor can read. When nil
	// (or it returns ok=false) in the blocked tier, a consented command
	// runs real sudo with its prompt shown to the visitor, who must then
	// know the password.
	SudoPassword func() (string, bool)
	// OnShareRequest is called when the visitor asks for a shared terminal
	// (frame 4). Return true to enable it. Nil denies.
	OnShareRequest func() bool

	// Env is the environment to derive the visitor's from (default
	// os.Environ()). Only whitelisted variables reach the shell; see
	// scrubEnv.
	Env []string
	Dir string // default $HOME

	realSudo string // tests only: path used as the real sudo binary
}

// Host is one PTY session served to a visitor.
type Host struct {
	opt      HostOptions
	ch       p2p.Channel
	cmd      *exec.Cmd // the sandbox wrapper, or the shell when not isolated
	ptmx     *os.File
	shim     string // ShimDir
	ownDir   bool
	isolated bool
	events   *os.File // ipc/events, held O_RDWR so writers never block on open
	replies  *os.File // reply FIFO, held O_RDWR; the shell gets a read end as fd 4
	realSudo string
	sudoEnv  []string // environment for host-run sudo
	home     string

	wmu   sync.Mutex // channel writes
	outMu sync.Mutex // redact+send of one output chunk

	stateMu  sync.Mutex
	cur      *CommandEvent // command in flight (hook "pre" seen)
	tokens   map[string]bool
	typist   string
	shared   bool
	cols     uint16
	rows     uint16
	relay    *os.File  // inner PTY of a running host-run sudo, or nil
	relayCmd *exec.Cmd // its process
	// heldPassword is the blocked-tier card's password for the sudo in
	// flight; consumed by answerPassword, cleared when the sudo ends.
	heldPassword string

	sudoMu sync.Mutex // one host-run sudo at a time

	transcript *teeBuffer
	redact     *redactor

	done     chan struct{}
	doneOnce sync.Once
	stopOnce sync.Once
	err      error
}

// envKeep is the whitelist of variables that reach the visitor shell. The
// rest of the host's environment, in particular SSH_AUTH_SOCK,
// DBUS_SESSION_BUS_ADDRESS, GPG_AGENT_INFO, SUDO_ASKPASS and anything
// pointing at credentials or agents, is dropped. LC_* and PAIRDESK_* are
// kept by prefix.
var envKeep = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TERM": true, "LANG": true, "COLORTERM": true, "XDG_RUNTIME_DIR": true,
	"DISPLAY": true, "WAYLAND_DISPLAY": true,
}

// scrubEnv applies envKeep to env, keeping the first value of each name
// and the original order.
func scrubEnv(env []string) (map[string]string, []string) {
	envm := map[string]string{}
	var order []string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if !(envKeep[k] || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "PAIRDESK_")) {
			continue
		}
		if _, seen := envm[k]; seen {
			continue
		}
		order = append(order, k)
		envm[k] = v
	}
	return envm, order
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
		typist:     "visitor",
		transcript: newTeeBuffer(),
		done:       make(chan struct{}),
		cols:       80,
		rows:       24,
	}
	h.redact = newRedactor(writerFunc(func(p []byte) (int, error) {
		h.transcript.Write(p)
		if opt.OnOutput != nil {
			opt.OnOutput(time.Now(), append([]byte{}, p...))
		}
		return len(p), nil
	}))

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
	bin, ipc := filepath.Join(dir, dirBin), filepath.Join(dir, dirIPC)
	fail := func(err error) (*Host, error) {
		if h.events != nil {
			_ = h.events.Close()
		}
		if h.replies != nil {
			_ = h.replies.Close()
		}
		h.cleanupDir()
		return nil, err
	}
	if err := writeShimDir(dir); err != nil {
		return fail(err)
	}
	ev, err := os.OpenFile(filepath.Join(ipc, fileEvents), os.O_RDWR, 0)
	if err != nil {
		return fail(err)
	}
	h.events = ev
	// The reply FIFO lives in ipc too, but the shell only ever sees it as
	// an inherited fd, so it may be hidden with the rest of ipc.
	replyPath := filepath.Join(ipc, "reply")
	if err := syscall.Mkfifo(replyPath, 0o600); err != nil {
		return fail(fmt.Errorf("term: mkfifo reply: %w", err))
	}
	rp, err := os.OpenFile(replyPath, os.O_RDWR, 0)
	if err != nil {
		return fail(err)
	}
	h.replies = rp

	// Resolve real sudo on the host side, on a PATH that cannot contain
	// ShimDir, so the visitor can never pick the binary that gets the
	// host's password.
	h.realSudo = opt.realSudo
	if h.realSudo == "" {
		h.realSudo = lookupRealSudo(dir)
	}

	base := opt.Env
	if base == nil {
		base = os.Environ()
	}
	envm, order := scrubEnv(base)
	set := func(k, v string) {
		if _, seen := envm[k]; !seen {
			order = append(order, k)
		}
		envm[k] = v
	}
	hostPath := envm["PATH"]
	if hostPath == "" {
		hostPath = "/usr/local/bin:/usr/bin:/bin"
	}
	set("PATH", bin+string(os.PathListSeparator)+hostPath)
	set("SHELL", shell)
	if envm["TERM"] == "" {
		set("TERM", "xterm-256color")
	}
	if envm["HOME"] == "" {
		if hd, err := os.UserHomeDir(); err == nil {
			set("HOME", hd)
		}
	}
	h.home = envm["HOME"]

	var args []string
	switch filepath.Base(shell) {
	case "bash":
		args = []string{"--rcfile", filepath.Join(bin, fileBashrc), "-i"}
	case "zsh":
		set("ZDOTDIR", filepath.Join(bin, dirZsh))
		args = []string{"-i"}
	case "fish":
		args = []string{"-i", "--init-command", "source " + shellQuote(filepath.Join(bin, fileFish))}
	default:
		args = []string{"-i"}
	}
	var env []string
	for _, k := range order {
		env = append(env, k+"="+envm[k])
	}
	// Host-run sudo gets the same scrubbed environment minus the shim
	// PATH entry, plus the askpass helper. sudo resets the command's
	// environment itself; this is what sudo and askpass see.
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			h.sudoEnv = append(h.sudoEnv, "PATH="+hostPath)
		case strings.HasPrefix(kv, "ZDOTDIR="):
		default:
			h.sudoEnv = append(h.sudoEnv, kv)
		}
	}
	h.sudoEnv = append(h.sudoEnv, "SUDO_ASKPASS="+filepath.Join(bin, fileAskpass))

	argv := append([]string{shell}, args...)
	if opt.Isolate != IsolateOff {
		hidden := append(append([]string{}, opt.HiddenPaths...), ipc)
		plan, perr := buildSandbox(shell, args, hidden, bin)
		if perr == nil {
			perr = plan.probe(ctx, env)
		}
		switch {
		case perr == nil:
			argv = plan.argv
			h.isolated = true
		case opt.Isolate == IsolateRequired:
			return fail(fmt.Errorf("%w: %v", ErrNoIsolation, perr))
		}
	}

	// Drop any credentials sudo cached for this user before the visitor
	// gets a shell, and run every sudo with -k (see runSudo), so one
	// consented command never turns into 15 minutes of root.
	h.sudoReset(ctx)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = opt.Dir
	if cmd.Dir == "" {
		cmd.Dir = h.home
	}
	cmd.Env = env
	evW, err := os.OpenFile(filepath.Join(ipc, fileEvents), os.O_WRONLY, 0)
	if err != nil {
		return fail(err)
	}
	rpR, err := os.OpenFile(replyPath, os.O_RDONLY, 0)
	if err != nil {
		_ = evW.Close()
		return fail(err)
	}
	cmd.ExtraFiles = []*os.File{evW, rpR} // fd 3, fd 4 in the shell
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: h.rows, Cols: h.cols})
	_ = evW.Close()
	_ = rpR.Close()
	if err != nil {
		return fail(err)
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

// lookupRealSudo finds the sudo binary on a PATH that excludes shimDir,
// preferring /usr/bin/sudo.
func lookupRealSudo(shimDir string) string {
	if st, err := os.Stat("/usr/bin/sudo"); err == nil && !st.IsDir() {
		return "/usr/bin/sudo"
	}
	var parts []string
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "" || strings.HasPrefix(p, shimDir) {
			continue
		}
		parts = append(parts, p)
	}
	for _, p := range parts {
		cand := filepath.Join(p, "sudo")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return cand
		}
	}
	return "/usr/bin/sudo"
}

// sudoReset runs `sudo -K` as the host user so no cached credentials are
// available when the session starts. It runs from the host process (the
// cache is per user, and every sudo of this session is run by the host
// process anyway), never prompts, and failures are ignored.
func (h *Host) sudoReset(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.realSudo, "-K")
	cmd.Env = h.sudoEnv
	cmd.Stdin = nil
	_ = cmd.Run()
}

// randomHex returns 2n hex characters from crypto/rand.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (h *Host) cleanupDir() {
	if h.ownDir {
		_ = os.RemoveAll(h.shim)
	} else {
		_ = os.RemoveAll(filepath.Join(h.shim, dirBin))
		_ = os.RemoveAll(filepath.Join(h.shim, dirIPC))
	}
}

func (h *Host) send(typ byte, payload []byte) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	return WriteFrame(h.ch, typ, payload)
}

// output passes one chunk through the redactor (transcript) and on to the
// visitor, atomically with respect to other chunks.
func (h *Host) output(p []byte) error {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_, _ = h.redact.Write(p)
	return h.send(FrameData, p)
}

// pumpOutput copies PTY output to the transcript and the visitor. When the
// shell exits the PTY read fails and the session is torn down.
func (h *Host) pumpOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			if err := h.output(buf[:n]); err != nil {
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

// writeInput delivers typed bytes to whatever currently owns the visitor's
// keyboard: the host-run sudo's PTY while one is running, else the shell.
func (h *Host) writeInput(p []byte) error {
	h.stateMu.Lock()
	relay := h.relay
	h.stateMu.Unlock()
	if relay != nil {
		if _, err := relay.Write(p); err == nil {
			return nil
		}
	}
	_, err := h.ptmx.Write(p)
	return err
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
			if err := h.writeInput(payload); err != nil {
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

// pumpEvents reads lines from the events FIFO. Everything on it comes
// from the visitor's side (hooks, shim) except "pw", which the askpass
// helper writes from outside the sandbox; none of it is trusted beyond
// what the tier and the token check allow.
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
		if len(f) < 4 {
			return
		}
		args, ok := decodeHexArgs(f[3])
		if !ok {
			h.reply(f[1], "deny", "pairdesk: sudo shim: malformed request")
			return
		}
		h.stateMu.Lock()
		if h.cur != nil {
			h.cur.Sudo = true
		}
		h.stateMu.Unlock()
		go h.runSudo(f[1], f[2], args)
	case "pw":
		if len(f) < 2 {
			return
		}
		go h.answerPassword(f[1])
	}
}

func (h *Host) emit(ev CommandEvent) {
	if h.opt.OnCommand != nil {
		h.opt.OnCommand(ev)
	}
	_ = h.send(FrameCommand, encodeCommand(ev))
}

// reply writes one verdict line for the shim with the given id.
func (h *Host) reply(id, kind, rest string) {
	_, _ = h.replies.WriteString(id + "\t" + kind + "\t" + rest + "\n")
}

// runSudo applies the host's tier to one shim request and, when allowed,
// runs real sudo itself: outside the sandbox, on a fresh PTY whose output
// goes to the visitor and which receives the visitor's keystrokes until
// sudo exits. Real sudo is always run with -k, so it neither uses nor
// refreshes the credential cache: every invocation needs the password
// again (from the host via askpass, or typed by the visitor in the ask
// tier).
//
// What this cannot prevent: a visitor who knows the host's password (ask
// tier) or has NOPASSWD in sudoers can run /usr/bin/sudo directly inside
// the sandbox; the transcript and command log still record it, and the
// tier card is only skipped, never the record.
func (h *Host) runSudo(id, cwd string, args []string) {
	h.sudoMu.Lock()
	defer h.sudoMu.Unlock()

	cmdStr := strings.Join(args, " ")
	_ = h.send(FrameSudo, []byte(`{"cmd":`+strconv.Quote(cmdStr)+`}`))

	const (
		modeDeny = iota
		modePrompt
		modeAskpass
	)
	mode, denyMsg := modeDeny, "pairdesk: sudo needs the host's consent"
	switch h.opt.SudoTier {
	case model.SudoAsk:
		mode = modePrompt
	case model.SudoAuto:
		if h.opt.SudoPassword != nil {
			mode = modeAskpass
		} else {
			denyMsg = "pairdesk: sudo is unavailable: the host has not stored a password"
		}
	default: // blocked
		if h.opt.OnSudo != nil && h.opt.OnSudo(cmdStr) {
			mode = modePrompt
			if h.opt.SudoPassword != nil {
				if pw, ok := h.opt.SudoPassword(); ok {
					mode = modeAskpass
					// Hold this one password for the askpass helper of
					// this invocation only.
					h.stateMu.Lock()
					h.heldPassword = pw
					h.stateMu.Unlock()
				}
			}
		}
	}

	rc := 1
	if mode == modeDeny {
		h.reply(id, "deny", denyMsg)
	} else {
		rc = h.execSudo(mode == modeAskpass, cwd, args)
		h.reply(id, "done", strconv.Itoa(rc))
	}
	h.stateMu.Lock()
	h.heldPassword = ""
	standalone := h.cur == nil
	h.stateMu.Unlock()
	if standalone {
		// No shell hooks (plain sh): the shim is the only source.
		h.emit(CommandEvent{Time: time.Now(), Cmd: "sudo " + cmdStr, Cwd: cwd, Exit: rc, Sudo: true, Typist: "visitor"})
	}
}

// execSudo runs real sudo on a relayed PTY and returns its exit code.
func (h *Host) execSudo(askpass bool, cwd string, args []string) int {
	marker := markerPrefix + randomHex(markerHexLen/2) + markerSuffix
	h.redact.issue(marker)
	defer h.redact.retire(marker)

	argv := []string{h.realSudo, "-k"}
	tok := ""
	if askpass {
		tok = randomHex(16)
		h.stateMu.Lock()
		h.tokens[tok] = true
		h.stateMu.Unlock()
		defer func() {
			h.stateMu.Lock()
			delete(h.tokens, tok)
			h.stateMu.Unlock()
		}()
		argv = append(argv, "-A", "-p", askpassPromptPrefix+tok)
	} else {
		argv = append(argv, "-p", marker)
	}
	argv = append(argv, args...)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = h.sudoEnv
	cmd.Dir = h.home
	if filepath.IsAbs(cwd) && !h.hidden(cwd) {
		if st, err := os.Stat(cwd); err == nil && st.IsDir() {
			cmd.Dir = cwd
		}
	}
	h.stateMu.Lock()
	size := &pty.Winsize{Rows: h.rows, Cols: h.cols}
	h.stateMu.Unlock()
	inner, err := pty.StartWithSize(cmd, size)
	if err != nil {
		_ = h.output([]byte("pairdesk: cannot run sudo: " + err.Error() + "\r\n"))
		return 1
	}
	h.stateMu.Lock()
	h.relay, h.relayCmd = inner, cmd
	h.stateMu.Unlock()

	buf := make([]byte, 32*1024)
	for {
		n, err := inner.Read(buf)
		if n > 0 {
			_ = h.output(buf[:n])
		}
		if err != nil {
			break
		}
	}
	werr := cmd.Wait()
	h.stateMu.Lock()
	h.relay, h.relayCmd = nil, nil
	h.stateMu.Unlock()
	_ = inner.Close()

	rc := 0
	if werr != nil {
		rc = 1
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				rc = 128 + int(ws.Signal())
			} else if ee.ExitCode() >= 0 {
				rc = ee.ExitCode()
			}
		}
	}
	return rc
}

// hidden reports whether p is under one of HiddenPaths. A cwd there is
// the empty tmpfs from the visitor's side but the real directory from the
// host's, so sudo is run from $HOME instead.
func (h *Host) hidden(p string) bool {
	for _, d := range h.opt.HiddenPaths {
		if d == "" {
			continue
		}
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// answerPassword serves one askpass request: the token must be the one
// issued for the sudo currently running, and it is consumed at once. The
// password goes into ipc/pw, which only the askpass helper (outside the
// sandbox) can open.
func (h *Host) answerPassword(token string) {
	h.stateMu.Lock()
	ok := h.tokens[token]
	delete(h.tokens, token)
	pw := h.heldPassword
	h.heldPassword = ""
	h.stateMu.Unlock()
	if ok && pw == "" && h.opt.SudoPassword != nil {
		if p, got := h.opt.SudoPassword(); got {
			pw = p
		}
	}
	if !ok {
		pw = ""
	}
	h.replyFIFO(filepath.Join(h.shim, dirIPC, filePw), pw)
}

// replyFIFO writes one line to a FIFO the askpass helper is reading. The
// open is non-blocking and retried briefly so a helper that died cannot
// wedge the host. The path is in ipc, which only the host writes, and is
// checked to be a FIFO before anything is written.
func (h *Host) replyFIFO(path, line string) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		if err == nil {
			if st, serr := f.Stat(); serr == nil && st.Mode()&os.ModeNamedPipe != 0 {
				_, _ = f.WriteString(line + "\n")
			}
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
	return h.writeInput(p)
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

// Isolated reports whether the visitor shell runs inside the namespace
// sandbox. False only with IsolateOff or a best-effort start that could
// not set it up.
func (h *Host) Isolated() bool { return h.isolated }

// Marker is the fixed prefix of the sudo password prompt markers. Each
// allowed sudo gets its own full marker; see redact.go.
func (h *Host) Marker() string { return markerPrefix }

// Resize sets the PTY window size (and the relayed sudo PTY, if any).
func (h *Host) Resize(cols, rows uint16) error {
	h.stateMu.Lock()
	h.cols, h.rows = cols, rows
	relay := h.relay
	h.stateMu.Unlock()
	if relay != nil {
		_ = pty.Setsize(relay, &pty.Winsize{Cols: cols, Rows: rows})
	}
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

// killTree signals the shell's process group and, when isolated, the
// sandbox process itself. SIGKILL on the sandbox init (bwrap or unshare
// with --die-with-parent / --kill-child) tears down the PID namespace,
// so nothing started with setsid/nohup inside survives.
func (h *Host) killTree(sig syscall.Signal) {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	pid := h.cmd.Process.Pid
	_ = syscall.Kill(-pid, sig)
	_ = syscall.Kill(pid, sig)
	h.stateMu.Lock()
	rc := h.relayCmd
	h.stateMu.Unlock()
	if rc != nil && rc.Process != nil {
		_ = syscall.Kill(-rc.Process.Pid, sig)
	}
}

// Stop is the kill switch: SIGHUP to the shell tree, SIGKILL to the
// sandbox after a short grace, then the PTY and the channel are closed.
func (h *Host) Stop() error {
	h.stopOnce.Do(func() {
		h.killTree(syscall.SIGHUP)
		go func() {
			select {
			case <-h.done:
			case <-time.After(500 * time.Millisecond):
				h.killTree(syscall.SIGKILL)
			}
		}()
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
		h.killTree(syscall.SIGHUP)
		_ = h.ptmx.Close()
		go func() {
			// Reap; if SIGHUP was ignored, escalate.
			waited := make(chan struct{})
			go func() { _ = h.cmd.Wait(); close(waited) }()
			select {
			case <-waited:
			case <-time.After(500 * time.Millisecond):
				h.killTree(syscall.SIGKILL)
				<-waited
			}
			_ = h.events.Close()
			_ = h.replies.Close()
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
