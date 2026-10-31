package screen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
)

// ViewerOptions tunes the viewer window.
type ViewerOptions struct {
	Title  string       // informational; autovideosink does not expose a title without cgo
	Logger *slog.Logger // nil = slog.Default()
	// OnInput, when non-nil, enables control mode: the caller is expected to
	// feed ReadTerminalInput (or any other source) into it. The viewer itself
	// never generates input events because autovideosink has no input hooks.
	OnInput func(ev InputEvent)
}

// Viewer is a running gst-launch-1.0 decode/display process fed from a
// remote track.
type Viewer struct {
	log    *slog.Logger
	opt    ViewerOptions
	track  *webrtc.TrackRemote
	conn   *net.UDPConn
	port   int
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	exited chan error

	mu      sync.Mutex
	stopped bool
	err     error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// StartViewer picks a free loopback UDP port, launches
// "gst-launch-1.0 udpsrc ... ! rtpjitterbuffer ! rtpvp8depay ! vp8dec !
// videoconvert ! autovideosink sync=false", and copies RTP packets from
// track into it until the track ends, ctx is cancelled or Stop is called.
//
// udpsrc has to own the port itself, so the port is reserved by binding and
// immediately releasing it; the tiny race with another process grabbing it
// is detected because gst then exits during startup.
func StartViewer(ctx context.Context, track *webrtc.TrackRemote, opt ViewerOptions) (*Viewer, error) {
	if track == nil {
		return nil, errors.New("screen: nil track")
	}
	log := logger(opt.Logger)
	if _, err := exec.LookPath(GstBinary); err != nil {
		return nil, fmt.Errorf("screen: %s not found in PATH (install gstreamer1.0-tools and the vpx, rtp and display plugins): %w", GstBinary, err)
	}
	port, err := reserveUDPPort()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return nil, fmt.Errorf("screen: dialing loopback UDP: %w", err)
	}

	args := ViewerArgs(port)
	cmd := exec.Command(args[0], args[1:]...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if opt.Title != "" {
		cmd.Env = append(os.Environ(), "PAIRDESK_VIEWER_TITLE="+opt.Title)
	}
	log.Debug("screen: starting viewer", "argv", cmd.Args)
	if err := cmd.Start(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("screen: starting %s: %w", GstBinary, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Give gst a moment to bind the port and open the window; if it dies in
	// that window the error is almost certainly a missing plugin or display.
	select {
	case err := <-exited:
		conn.Close()
		return nil, fmt.Errorf("screen: %s exited during startup: %w\n%s", GstBinary, err, trimStderr(stderr))
	case <-time.After(gstSettleDelay):
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		conn.Close()
		return nil, ctx.Err()
	}

	vctx, cancel := context.WithCancel(ctx)
	v := &Viewer{
		log: log, opt: opt, track: track, conn: conn, port: port,
		cmd: cmd, stderr: stderr, exited: exited,
		ctx: vctx, cancel: cancel, done: make(chan struct{}),
	}
	go v.pump()
	return v, nil
}

// Done is closed once the viewer process has exited and the pump returned.
func (v *Viewer) Done() <-chan struct{} { return v.done }

// Err returns why the viewer stopped on its own, if it did (nil after Stop
// or a clean end of track).
func (v *Viewer) Err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

// Stop closes the window and stops copying packets.
func (v *Viewer) Stop() error {
	v.mu.Lock()
	if v.stopped {
		v.mu.Unlock()
		<-v.done
		return nil
	}
	v.stopped = true
	v.mu.Unlock()
	v.cancel()
	v.kill()
	<-v.done
	return nil
}

func (v *Viewer) kill() {
	if v.cmd == nil || v.cmd.Process == nil {
		return
	}
	_ = v.cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-v.exited:
		v.exited <- err
	case <-time.After(2 * time.Second):
		_ = v.cmd.Process.Kill()
		err := <-v.exited
		v.exited <- err
	}
}

func (v *Viewer) pump() {
	defer close(v.done)
	defer v.conn.Close()
	defer v.kill()

	// If the user closes the window gst exits; stop the pump then.
	go func() {
		select {
		case <-v.ctx.Done():
		case err := <-v.exited:
			v.exited <- err
			if v.ctx.Err() == nil {
				v.log.Info("screen: viewer window closed", "err", err)
				v.mu.Lock()
				if v.err == nil && err != nil && !v.stopped {
					v.err = fmt.Errorf("screen: %s exited: %w\n%s", GstBinary, err, trimStderr(v.stderr))
				}
				v.mu.Unlock()
			}
			v.cancel()
		}
	}()

	// track.ReadRTP blocks without a context; a deadline loop lets us notice
	// cancellation promptly.
	for v.ctx.Err() == nil {
		_ = v.track.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		pkt, _, err := v.track.ReadRTP()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if !errors.Is(err, io.EOF) && v.ctx.Err() == nil {
				v.mu.Lock()
				if v.err == nil {
					v.err = err
				}
				v.mu.Unlock()
			}
			return
		}
		// The caps handed to udpsrc pin the payload type to 96; the
		// negotiated one may differ.
		pkt.PayloadType = rtpPayloadType
		b, err := pkt.Marshal()
		if err != nil {
			continue
		}
		if _, err := v.conn.Write(b); err != nil {
			v.log.Debug("screen: viewer udp write", "err", err)
		}
	}
}

func reserveUDPPort() (int, error) {
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return 0, fmt.Errorf("screen: reserving loopback UDP port: %w", err)
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	_ = l.Close()
	return port, nil
}
