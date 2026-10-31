package term

import (
	"bytes"
	"context"
	"errors"
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
	if opt.realSudo == "" {
		opt.realSudo = writeFakeSudo(t)
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

// writeFakeSudo stands in for real sudo. It must never be reached when
// denied. It understands the flags the host passes (-K, -k, -A, -p) and
// records every password it obtained through askpass in <dir>/received,
// which is outside the sandbox (fake sudo is run by the host process).
func writeFakeSudo(t *testing.T) string {
	dir := t.TempDir()
	p := filepath.Join(dir, "fakesudo")
	script := `#!/bin/sh
rec="` + dir + `/received"
askpass=0; prompt=
while [ $# -gt 0 ]; do
  case "$1" in
    -K) exit 0 ;;
    -k) shift ;;
    -A) askpass=1; shift ;;
    -p) prompt=$2; shift 2 ;;
    *) break ;;
  esac
done
if [ "$askpass" = 1 ]; then
  pw=$("$SUDO_ASKPASS" "[sudo: $prompt] Password: ")
  printf '%s\n' "$pw" >> "$rec"
  echo "FAKE-SUDO askpass len=${#pw}"
  exec "$@"
fi
printf '%s' "$prompt"
IFS= read -r typed
echo "FAKE-SUDO prompt mode typed=${#typed}"
exec "$@"
`
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeSudoReceived returns the passwords the fake sudo at path obtained.
func fakeSudoReceived(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "received"))
	return string(b)
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
	fake := writeFakeSudo(t)
	pwCalls := 0
	h, v := startHost(t, HostOptions{
		Shell:    shell,
		SudoTier: model.SudoBlocked,
		OnSudo:   func(string) bool { return true },
		SudoPassword: func() (string, bool) {
			pwCalls++
			return "hunter2", true
		},
		realSudo: fake,
	})
	tr := drainTranscript(h)
	v.typeLine(t, "sudo echo done; echo rc=$?\n")
	out := v.waitOutput(t, "rc=0")
	if !strings.Contains(out, "FAKE-SUDO askpass len=7") || !strings.Contains(out, "\ndone") && !strings.Contains(out, "\r\ndone") {
		t.Fatalf("askpass flow not used: %q", out)
	}
	if got := fakeSudoReceived(t, fake); got != "hunter2\n" {
		t.Fatalf("fake sudo received %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(v.output(), "hunter2") || strings.Contains(tr.String(), "hunter2") {
		t.Fatal("password leaked to visitor or transcript")
	}
	// Nothing under ShimDir ever holds the password.
	_ = filepath.Walk(h.shim, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "hunter2") {
			t.Errorf("%s contains the password", p)
		}
		return nil
	})
	if pwCalls != 1 {
		t.Fatalf("SudoPassword called %d times, want 1", pwCalls)
	}
	// A second sudo needs a fresh verdict and password: no caching.
	v.typeLine(t, "sudo echo again; echo rc=$?\n")
	v.waitOutput(t, "again")
	if got := fakeSudoReceived(t, fake); got != "hunter2\nhunter2\n" {
		t.Fatalf("second sudo: fake sudo received %q", got)
	}
}

// TestSudoBlockedApprovedWithoutPassword: the host said yes but has no
// password to give, so real sudo prompts the visitor (marker prompt)
// instead of askpass with an empty password.
func TestSudoBlockedApprovedWithoutPassword(t *testing.T) {
	shell, _ := pickShell(t)
	h, v := startHost(t, HostOptions{
		Shell:        shell,
		SudoTier:     model.SudoBlocked,
		OnSudo:       func(string) bool { return true },
		SudoPassword: func() (string, bool) { return "", false },
	})
	tr := drainTranscript(h)
	v.typeLine(t, "sudo echo done; echo rc=$?\n")
	v.waitOutput(t, h.Marker())
	v.typeLine(t, "hunter2\n")
	out := v.waitOutput(t, "rc=0")
	if !strings.Contains(out, "FAKE-SUDO prompt mode typed=7") {
		t.Fatalf("prompt flow not used: %q", out)
	}
	time.Sleep(100 * time.Millisecond)
	if got := tr.String(); strings.Contains(got, "hunter2") {
		t.Fatalf("transcript holds the typed password: %q", got)
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

func TestShimRefusesWithoutLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(shimScript(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "sudo"), "true")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("shim ran without the host link")
	}
	if !strings.Contains(string(out), "host link missing") {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestRedactor(t *testing.T) {
	marker := markerPrefix + "0123456789ab" + markerSuffix
	var out bytes.Buffer
	r := newRedactor(&out)
	r.issue(marker)
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
	r = newRedactor(&out)
	write("x\x1b]")
	write("0;title\x07y")
	_ = r.Flush()
	if out.String() != "x\x1b]0;title\x07y" {
		t.Fatalf("false positive mangled output: %q", out.String())
	}
	// A marker the host did not issue (or retired) hides nothing.
	out.Reset()
	r = newRedactor(&out)
	forged := markerPrefix + "deadbeefcafe" + markerSuffix
	write(forged + "notsecret\r\n")
	r.issue(marker)
	r.retire(marker)
	write(marker + "alsovisible\r\n")
	_ = r.Flush()
	if got := out.String(); !strings.Contains(got, "notsecret") || !strings.Contains(got, "alsovisible") {
		t.Fatalf("unissued marker redacted output: %q", got)
	}
}

// TestForgedMarkerDoesNotRedact: the visitor prints a marker-shaped
// sequence of their own; since the host never issued it, nothing after
// it is hidden from the transcript.
func TestForgedMarkerDoesNotRedact(t *testing.T) {
	shell, _ := pickShell(t)
	h, v := startHost(t, HostOptions{Shell: shell})
	tr := drainTranscript(h)
	forged := markerPrefix + "deadbeefcafe" + markerSuffix
	v.typeLine(t, `printf '%s' `+shellQuote(forged)+`; echo hun""ter2; echo visible`+"\n")
	v.waitOutput(t, "visible")
	time.Sleep(200 * time.Millisecond)
	got := tr.String()
	if !strings.Contains(got, "hunter2") || !strings.Contains(got, "visible") {
		t.Fatalf("transcript dropped output after a forged marker: %q", got)
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

func requireIsolation(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err == nil {
		return
	}
	if _, err := exec.LookPath("unshare"); err == nil {
		return
	}
	t.Skip("neither bwrap nor unshare available")
}

func TestIsolationHidesPaths(t *testing.T) {
	requireIsolation(t)
	shell, _ := pickShell(t)
	hidden := t.TempDir()
	secret := filepath.Join(hidden, "identity.key")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, v := startHost(t, HostOptions{Shell: shell, Isolate: IsolateBestEffort, HiddenPaths: []string{hidden}})
	if !h.Isolated() {
		t.Skip("sandbox could not be set up here")
	}
	v.typeLine(t, "cat "+secret+"; echo rc=$?\n")
	out := v.waitOutput(t, "rc=1")
	if strings.Contains(out, "top secret") || !strings.Contains(out, "No such file") {
		t.Fatalf("hidden path readable: %q", out)
	}
	ipc := filepath.Join(h.shim, dirIPC)
	v.typeLine(t, "test -p "+filepath.Join(ipc, fileEvents)+" && echo VISIBLE-EV || echo HIDDEN-EV; test -e "+filepath.Join(ipc, filePw)+" && echo VISIBLE-PW || echo HIDDEN-PW\n")
	out = v.waitOutput(t, "-PW")
	if !strings.Contains(out, "HIDDEN-EV") || !strings.Contains(out, "HIDDEN-PW") {
		t.Fatalf("ipc FIFOs visible inside the sandbox: %q", out)
	}
	// The shim dir is read-only inside, the host's processes invisible.
	v.typeLine(t, "touch "+filepath.Join(h.shim, dirBin, "x")+" 2>/dev/null && echo WRITABLE || echo READONLY; ls /proc | grep -c '^[0-9]'\n")
	out = v.waitOutput(t, "READONLY")
	if strings.Contains(out, "WRITABLE") {
		t.Fatalf("shim bin writable: %q", out)
	}
	// Sudo still works from inside (the host runs it outside).
	v.typeLine(t, "sudo echo viasudo; echo sudorc=$?\n")
	out = v.waitOutput(t, "sudorc=")
	if strings.Contains(out, "sudorc=0") {
		t.Fatalf("blocked tier allowed sudo: %q", out)
	}
	if !strings.Contains(out, "needs the host's consent") {
		t.Fatalf("no denial: %q", out)
	}
}

func TestIsolationRequiredFailsCleanly(t *testing.T) {
	shell, _ := pickShell(t)
	empty := t.TempDir()
	t.Setenv("PATH", empty) // no bwrap, no unshare
	hostEnd, _ := newPipePair()
	_, err := StartHost(context.Background(), hostEnd, HostOptions{Shell: shell, Env: testEnv(t), Dir: t.TempDir(), realSudo: writeFakeSudo(t)})
	if !errors.Is(err, ErrNoIsolation) {
		t.Fatalf("StartHost = %v, want ErrNoIsolation", err)
	}
	hostEnd, visitorEnd := newPipePair()
	h, err := StartHost(context.Background(), hostEnd, HostOptions{Shell: shell, Isolate: IsolateBestEffort, Env: testEnv(t), Dir: t.TempDir(), realSudo: writeFakeSudo(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	if h.Isolated() {
		t.Fatal("Isolated() true without a sandbox tool")
	}
	v := newVisitorSim(visitorEnd)
	v.typeLine(t, "echo unisolated\n")
	v.waitOutput(t, "\nunisolated")
}

func TestEnvScrubbed(t *testing.T) {
	shell, _ := pickShell(t)
	env := append(testEnv(t), "SSH_AUTH_SOCK=/run/user/1/ssh", "DBUS_SESSION_BUS_ADDRESS=unix:x", "GPG_AGENT_INFO=y", "SUDO_ASKPASS=/evil", "PAIRDESK_SESSION=s1", "LC_ALL=C")
	_, v := startHost(t, HostOptions{Shell: shell, Env: env})
	v.typeLine(t, "echo A=$SSH_AUTH_SOCK B=$DBUS_SESSION_BUS_ADDRESS C=$GPG_AGENT_INFO D=$SUDO_ASKPASS E=$PAIRDESK_SESSION F=$LC_ALL END\n")
	out := v.waitOutput(t, " END")
	if !strings.Contains(out, "A= B= C= D= E=s1 F=C END") {
		t.Fatalf("environment not scrubbed as expected: %q", out)
	}
}

// TestRealSudoNotVisitorSelectable: the binary that gets the host's
// password is fixed at write time; PAIRDESK_REAL_SUDO in the visitor's
// environment changes nothing.
func TestRealSudoNotVisitorSelectable(t *testing.T) {
	shell, _ := pickShell(t)
	evilDir := t.TempDir()
	evil := filepath.Join(evilDir, "evil")
	mark := filepath.Join(evilDir, "ran")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\ntouch "+mark+"\necho EVIL-RAN\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeSudo(t)
	_, v := startHost(t, HostOptions{Shell: shell, SudoTier: model.SudoAsk, realSudo: fake})
	v.typeLine(t, "PAIRDESK_REAL_SUDO="+evil+" PAIRDESK_HOOK=/dev/null PAIRDESK_SHIM_DIR=/tmp sudo true; echo rc=$?\n")
	// Ask tier: the fake prompts and waits for a line.
	v.waitOutput(t, "[sudo] password for host: ")
	v.typeLine(t, "pw\n")
	out := v.waitOutput(t, "rc=0")
	if strings.Contains(out, "EVIL-RAN") {
		t.Fatalf("visitor-selected binary ran: %q", out)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("visitor-selected binary was executed")
	}
	if !strings.Contains(out, "FAKE-SUDO prompt mode") {
		t.Fatalf("real sudo recorded at write time did not run: %q", out)
	}
}

// TestStopKillsEscapedProcess: a setsid'd background process started by
// the visitor dies with the sandbox when the host stops the session.
func TestStopKillsEscapedProcess(t *testing.T) {
	requireIsolation(t)
	shell, _ := pickShell(t)
	h, v := startHost(t, HostOptions{Shell: shell, Isolate: IsolateBestEffort})
	if !h.Isolated() {
		t.Skip("sandbox could not be set up here")
	}
	const tag = "6123"
	v.typeLine(t, "setsid nohup sleep "+tag+" >/dev/null 2>&1 & echo started\n")
	v.waitOutput(t, "\nstarted")
	alive := func() bool {
		out, _ := exec.Command("pgrep", "-f", "^sleep "+tag).Output()
		return len(bytes.TrimSpace(out)) > 0
	}
	deadline := time.Now().Add(3 * time.Second)
	for !alive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !alive() {
		t.Fatal("escaped sleep never started")
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive() {
		_ = exec.Command("pkill", "-f", "^sleep "+tag).Run()
		t.Fatal("setsid'd process survived Stop")
	}
}

// TestSudoReadsExactArgs: arguments with spaces reach real sudo intact.
func TestSudoReadsExactArgs(t *testing.T) {
	shell, _ := pickShell(t)
	var got string
	_, v := startHost(t, HostOptions{
		Shell:        shell,
		SudoTier:     model.SudoBlocked,
		OnSudo:       func(cmd string) bool { got = cmd; return true },
		SudoPassword: func() (string, bool) { return "pw", true },
	})
	v.typeLine(t, "sudo printf '[%s]' 'a b' c; echo; echo rc=$?\n")
	out := v.waitOutput(t, "rc=0")
	if !strings.Contains(out, "[a b][c]") {
		t.Fatalf("args mangled: %q", out)
	}
	if got != "printf [%s] a b c" {
		t.Fatalf("card text = %q", got)
	}
}

// TestUnshareFallback forces the unshare(1) sandbox by hiding bwrap from
// PATH and checks the same guarantees hold there.
func TestUnshareFallback(t *testing.T) {
	un, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("unshare not available")
	}
	shell, _ := pickShell(t)
	bindir := t.TempDir()
	if err := os.Symlink(un, filepath.Join(bindir, "unshare")); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t) // full PATH for the shell and the mount script
	t.Setenv("PATH", bindir)
	hidden := t.TempDir()
	if err := os.WriteFile(filepath.Join(hidden, "k"), []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, v := startHost(t, HostOptions{Shell: shell, Isolate: IsolateBestEffort, HiddenPaths: []string{hidden}, Env: env})
	if !h.Isolated() {
		t.Skip("unshare sandbox could not be set up here")
	}
	if filepath.Base(h.cmd.Path) != "unshare" {
		t.Fatalf("sandbox tool = %s, want unshare", h.cmd.Path)
	}
	v.typeLine(t, "cat "+filepath.Join(hidden, "k")+"; echo rc=$?; test -p "+filepath.Join(h.shim, dirIPC, fileEvents)+" && echo VISIBLE || echo HIDDEN; touch "+filepath.Join(h.shim, dirBin, "x")+" 2>/dev/null && echo WRITABLE || echo READONLY; setsid sleep 6124 & echo END\n")
	out := v.waitOutput(t, "\nEND")
	if strings.Contains(out, "top secret") || !strings.Contains(out, "HIDDEN") || !strings.Contains(out, "READONLY") {
		t.Fatalf("unshare sandbox leaks: %q", out)
	}
	alive := func() bool {
		out, _ := exec.Command("/usr/bin/pgrep", "-f", "^sleep 6124").Output()
		return len(bytes.TrimSpace(out)) > 0
	}
	deadline := time.Now().Add(3 * time.Second)
	for !alive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive() {
		_ = exec.Command("/usr/bin/pkill", "-f", "^sleep 6124").Run()
		t.Fatal("setsid'd process survived Stop under unshare")
	}
}
