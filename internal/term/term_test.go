package term

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
)

// pipeChannel adapts net.Pipe to p2p.Channel.
type pipeChannel struct {
	net.Conn
	label string
}

func (p pipeChannel) Label() string { return p.label }

func newPipePair() (pipeChannel, pipeChannel) {
	a, b := net.Pipe()
	return pipeChannel{a, "term"}, pipeChannel{b, "term"}
}

// visitorSim reads frames from the visitor end, collecting data and events.
type visitorSim struct {
	ch     pipeChannel
	mu     sync.Mutex
	out    bytes.Buffer
	events chan CommandEvent
	bye    chan struct{}
	wmu    sync.Mutex
}

func newVisitorSim(ch pipeChannel) *visitorSim {
	v := &visitorSim{ch: ch, events: make(chan CommandEvent, 64), bye: make(chan struct{})}
	go func() {
		for {
			typ, payload, err := ReadFrame(ch)
			if err != nil {
				select {
				case <-v.bye:
				default:
					close(v.bye)
				}
				return
			}
			switch typ {
			case FrameData:
				v.mu.Lock()
				v.out.Write(payload)
				v.mu.Unlock()
			case FrameCommand:
				if ev, err := decodeCommand(payload); err == nil {
					v.events <- ev
				}
			case FrameBye:
				select {
				case <-v.bye:
				default:
					close(v.bye)
				}
			}
		}
	}()
	return v
}

func (v *visitorSim) typeLine(t *testing.T, s string) {
	t.Helper()
	v.wmu.Lock()
	defer v.wmu.Unlock()
	if err := WriteFrame(v.ch, FrameData, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func (v *visitorSim) output() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.out.String()
}

func (v *visitorSim) waitOutput(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := v.output(); strings.Contains(s, want) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output never contained %q; got:\n%s", want, v.output())
	return ""
}

func (v *visitorSim) waitEvent(t *testing.T, match func(CommandEvent) bool) CommandEvent {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-v.events:
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no matching command event; output:\n%s", v.output())
		}
	}
}

func pickShell(t *testing.T) (shell string, hooks bool) {
	if p, err := exec.LookPath("bash"); err == nil {
		return p, true
	}
	return "/bin/sh", false
}

func testEnv(t *testing.T) []string {
	home := t.TempDir()
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TERM=dumb", "PS1=$ "}
}

func startHost(t *testing.T, opt HostOptions) (*Host, *visitorSim) {
	t.Helper()
	hostEnd, visitorEnd := newPipePair()
	if opt.Env == nil {
		opt.Env = testEnv(t)
	}
	if opt.Dir == "" {
		opt.Dir = t.TempDir()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, err := StartHost(ctx, hostEnd, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Stop() })
	return h, newVisitorSim(visitorEnd)
}

// lockedBuffer is a bytes.Buffer safe for one writer and one reader.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func drainTranscript(h *Host) *lockedBuffer {
	buf := &lockedBuffer{}
	go func() { _, _ = io.Copy(buf, h.Transcript()) }()
	return buf
}

func TestHostEchoAndCommandEvent(t *testing.T) {
	shell, hooks := pickShell(t)
	var evMu sync.Mutex
	var hostEvents []CommandEvent
	h, v := startHost(t, HostOptions{
		Shell: shell,
		OnCommand: func(ev CommandEvent) {
			evMu.Lock()
			hostEvents = append(hostEvents, ev)
			evMu.Unlock()
		},
	})
	v.typeLine(t, "echo hello\n")
	out := v.waitOutput(t, "\nhello")
	if !strings.Contains(out, "\r\nhello\r\n") && !strings.Contains(out, "\nhello\n") {
		t.Fatalf("expected a line reading hello, got %q", out)
	}
	if hooks {
		ev := v.waitEvent(t, func(ev CommandEvent) bool { return ev.Cmd == "echo hello" })
		if ev.Exit != 0 || ev.Sudo || ev.Typist != "visitor" {
			t.Fatalf("bad event: %+v", ev)
		}
		v.typeLine(t, "false\n")
		ev = v.waitEvent(t, func(ev CommandEvent) bool { return ev.Cmd == "false" })
		if ev.Exit != 1 {
			t.Fatalf("exit = %d, want 1", ev.Exit)
		}
		evMu.Lock()
		n := len(hostEvents)
		evMu.Unlock()
		if n < 2 {
			t.Fatalf("host OnCommand saw %d events", n)
		}
	}
	v.typeLine(t, "exit\n")
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("host did not finish after exit")
	}
	select {
	case <-v.bye:
	case <-time.After(5 * time.Second):
		t.Fatal("visitor never got bye")
	}
}

func TestReadOnlyDropsInput(t *testing.T) {
	shell, _ := pickShell(t)
	_, v := startHost(t, HostOptions{Shell: shell, ReadOnly: true})
	v.typeLine(t, "echo should-not-run\n")
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(v.output(), "should-not-run") {
		t.Fatal("read-only host executed visitor input")
	}
}

func TestStopKillsShell(t *testing.T) {
	shell, _ := pickShell(t)
	h, v := startHost(t, HostOptions{Shell: shell})
	v.typeLine(t, "sleep 30\n")
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Stop took too long")
	}
	select {
	case <-h.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed")
	}
}

// fakeSudo stands in for real sudo: it must never be reached when denied,
// and when allowed via askpass it reports the password length only.
func writeFakeSudo(t *testing.T) string {
	dir := t.TempDir()
	p := filepath.Join(dir, "fakesudo")
	script := `#!/bin/sh
if [ "$1" = "-A" ]; then
  shift
  pw=$("$SUDO_ASKPASS" "prompt")
  echo "FAKE-SUDO askpass len=${#pw}"
  exec "$@"
fi
if [ "$1" = "-p" ]; then
  shift; shift
  echo "FAKE-SUDO prompt mode"
  exec "$@"
fi
echo "FAKE-SUDO ran: $*"
`
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSudoBlockedDenied(t *testing.T) {
	shell, hooks := pickShell(t)
	asked := make(chan string, 1)
	h, v := startHost(t, HostOptions{
		Shell:    shell,
		SudoTier: model.SudoBlocked,
		OnSudo: func(cmd string) bool {
			asked <- cmd
			return false
		},
		realSudo: writeFakeSudo(t),
	})
	v.typeLine(t, "sudo apt install nginx\n")
	select {
	case cmd := <-asked:
		if cmd != "apt install nginx" {
			t.Fatalf("OnSudo got %q", cmd)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnSudo never called")
	}
	ev := v.waitEvent(t, func(ev CommandEvent) bool { return ev.Sudo })
	if ev.Cmd != "sudo apt install nginx" || ev.Exit != 1 {
		t.Fatalf("sudo event = %+v (hooks=%v)", ev, hooks)
	}
	v.typeLine(t, "echo rc=$?\n")
	out := v.waitOutput(t, "rc=1")
	if !strings.Contains(out, "pairdesk: sudo needs the host's consent") {
		t.Fatalf("missing denial message in %q", out)
	}
	if strings.Contains(out, "FAKE-SUDO") {
		t.Fatal("real sudo was invoked despite denial")
	}
	_ = h
}

func TestSudoBlockedAllowedWithAskpass(t *testing.T) {
	shell, _ := pickShell(t)
	h, v := startHost(t, HostOptions{
		Shell:        shell,
		SudoTier:     model.SudoBlocked,
		OnSudo:       func(string) bool { return true },
		SudoPassword: func() (string, bool) { return "hunter2", true },
		realSudo:     writeFakeSudo(t),
	})
	tr := drainTranscript(h)
	v.typeLine(t, "sudo echo done; echo rc=$?\n")
	out := v.waitOutput(t, "rc=0")
	if !strings.Contains(out, "FAKE-SUDO askpass len=7") {
		t.Fatalf("askpass flow not used: %q", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(tr.String(), "hunter2") {
		t.Fatal("password leaked to visitor or transcript")
	}
}

func TestSudoAutoWithoutPasswordDenies(t *testing.T) {
	shell, _ := pickShell(t)
	_, v := startHost(t, HostOptions{Shell: shell, SudoTier: model.SudoAuto, realSudo: writeFakeSudo(t)})
	v.typeLine(t, "sudo id; echo rc=$?\n")
	out := v.waitOutput(t, "rc=1")
	if strings.Contains(out, "FAKE-SUDO") {
		t.Fatal("sudo ran without a password source")
	}
}

func TestShimRefusesWithoutSocket(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(shimScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "sudo"), "true")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), envHook + "=" + filepath.Join(dir, "missing"), envShimDir + "=" + dir}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("shim ran without the host link")
	}
	if !strings.Contains(string(out), "host link missing") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestRedactor(t *testing.T) {
	marker := "\x1b]pairdesk-sudo-abc\x07[sudo] password: "
	var out bytes.Buffer
	r := newRedactor(&out, marker)
	write := func(s string) {
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("before\r\n")
	// Marker split across writes.
	write(marker[:5])
	write(marker[5:] + "secret")
	write("word\r\nafter\r\n")
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "secret") || strings.Contains(got, "secretword") {
		t.Fatalf("password not redacted: %q", got)
	}
	if !strings.Contains(got, "before\r\n") || !strings.Contains(got, "after\r\n") || !strings.Contains(got, marker) {
		t.Fatalf("surrounding output damaged: %q", got)
	}
	// A partial-prefix tail that turns out not to be the marker is kept.
	out.Reset()
	r = newRedactor(&out, marker)
	write("x\x1b]")
	write("0;title\x07y")
	_ = r.Flush()
	if out.String() != "x\x1b]0;title\x07y" {
		t.Fatalf("false positive mangled output: %q", out.String())
	}
}

func TestTranscriptRedactsAfterMarker(t *testing.T) {
	shell, _ := pickShell(t)
	h, v := startHost(t, HostOptions{Shell: shell})
	tr := drainTranscript(h)
	// Print the marker then a "password" on the same line, as sudo's prompt
	// would be followed by echoed input on a misconfigured tty.
	v.typeLine(t, `printf '%s' "$PAIRDESK_SUDO_MARKER"; echo hun""ter2; echo visible`+"\n")
	v.waitOutput(t, "visible")
	time.Sleep(200 * time.Millisecond)
	if !strings.Contains(v.output(), "hunter2") {
		t.Fatal("visitor should still see the live stream")
	}
	got := tr.String()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("transcript holds the redacted line: %q", got)
	}
	if !strings.Contains(got, "visible") {
		t.Fatalf("transcript lost later output: %q", got)
	}
}

func TestVisitorRoundTripAndEscape(t *testing.T) {
	shell, _ := pickShell(t)
	hostEnd, visitorEnd := newPipePair()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := StartHost(ctx, hostEnd, HostOptions{Shell: shell, Env: testEnv(t), Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()

	stdinR, stdinW := io.Pipe()
	out := &lockedBuffer{}
	errc := make(chan error, 1)
	go func() { errc <- RunVisitor(ctx, visitorEnd, VisitorOptions{Stdin: stdinR, Stdout: out}) }()

	_, _ = stdinW.Write([]byte("echo via\"\"visitor\n"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := out.String()
		if strings.Contains(s, "\nviavisitor") || strings.Contains(s, "\r\nviavisitor") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no output: %q", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Escape then a non-q byte passes through; escape then q detaches.
	_, _ = stdinW.Write([]byte{0x1d, 'x'})
	_, _ = stdinW.Write([]byte{0x1d, 'q'})
	select {
	case err := <-errc:
		if err != ErrDetached {
			t.Fatalf("RunVisitor = %v, want ErrDetached", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunVisitor did not return on escape")
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("host did not end after visitor detached")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameResize, encodeResize(120, 40)); err != nil {
		t.Fatal(err)
	}
	typ, p, err := ReadFrame(&buf)
	if err != nil || typ != FrameResize {
		t.Fatal(err, typ)
	}
	c, r, err := decodeResize(p)
	if err != nil || c != 120 || r != 40 {
		t.Fatal(c, r, err)
	}
}

// TestOtherShellHooks checks the zsh and fish hook files when those shells
// are installed.
func TestOtherShellHooks(t *testing.T) {
	for _, name := range []string{"zsh", "fish"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Logf("%s not installed, skipping", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, v := startHost(t, HostOptions{Shell: p})
			v.typeLine(t, "echo hello\n")
			v.waitOutput(t, "hello") // fish redraws with escape codes, so no line check here
			ev := v.waitEvent(t, func(ev CommandEvent) bool { return ev.Cmd == "echo hello" })
			if ev.Exit != 0 {
				t.Fatalf("exit = %d", ev.Exit)
			}
			v.typeLine(t, "false\n")
			ev = v.waitEvent(t, func(ev CommandEvent) bool { return ev.Cmd == "false" })
			if ev.Exit != 1 {
				t.Fatalf("exit = %d, want 1", ev.Exit)
			}
		})
	}
}
