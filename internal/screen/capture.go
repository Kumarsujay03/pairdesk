package screen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Timeouts for StartCapture. PortalTimeout bounds every D-Bus call,
// including the compositor's source picker; GstStartupTimeout bounds how long
// we wait for the first RTP packet (or for gst to settle) before giving up.
const (
	PortalTimeout     = 10 * time.Second
	GstStartupTimeout = 10 * time.Second
	gstSettleDelay    = 1500 * time.Millisecond
	stallThreshold    = 3 * time.Second
)

// Capture is a running host-side screen capture feeding a pion track.
type Capture struct {
	log    *slog.Logger
	opt    CaptureOptions
	track  *webrtc.TrackLocalStaticRTP
	udp    *net.UDPConn
	port   int
	portal *portalSession // nil when using the X11 fallback

	mu      sync.Mutex
	cmd     *exec.Cmd
	stderr  *bytes.Buffer
	stopped bool
	err     error
	exited  chan error

	lastPacket   atomic.Int64 // unix nanos of the last RTP packet from gst
	keyframeReqs atomic.Int64
	restarting   atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// StartCapture asks the desktop portal for a PipeWire monitor stream and
// starts a gst-launch-1.0 encoder that delivers VP8 RTP to a loopback UDP
// port we own; those packets are written to Track().
//
// If the portal cannot be reached (no portal service, or no ScreenCast
// interface, e.g. a bare X11 session) and DISPLAY is set, it falls back to
// ximagesrc. The function returns a descriptive error within roughly
// PortalTimeout + GstStartupTimeout at worst.
func StartCapture(ctx context.Context, opt CaptureOptions) (*Capture, error) {
	opt = opt.withDefaults()
	log := logger(opt.Logger)
	if _, err := exec.LookPath(GstBinary); err != nil {
		return nil, fmt.Errorf("screen: %s not found in PATH (install gstreamer1.0-tools and the pipewire, vpx and rtp plugins): %w", GstBinary, err)
	}

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "pairdesk-screen",
	)
	if err != nil {
		return nil, fmt.Errorf("screen: creating track: %w", err)
	}

	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("screen: binding loopback UDP: %w", err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port

	cctx, cancel := context.WithCancel(ctx)
	c := &Capture{
		log:    log,
		opt:    opt,
		track:  track,
		udp:    udp,
		port:   port,
		ctx:    cctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	// Portal first.
	pctx, pcancel := context.WithTimeout(ctx, PortalTimeout)
	ps, perr := openPortalSession(pctx)
	pcancel()
	if perr == nil {
		c.portal = ps
		log.Info("screen: portal screencast ready", "node", ps.Node, "size", fmt.Sprintf("%dx%d", ps.Width, ps.Height))
	} else if errors.Is(perr, ErrPortalUnavailable) && os.Getenv("DISPLAY") != "" {
		log.Warn("screen: portal unavailable, falling back to ximagesrc", "err", perr)
	} else {
		c.teardown()
		if os.Getenv("DISPLAY") == "" {
			return nil, fmt.Errorf("screen: cannot capture: %w; and no DISPLAY for the X11 fallback", perr)
		}
		return nil, fmt.Errorf("screen: cannot capture: %w", perr)
	}

	if err := c.startGst(); err != nil {
		c.teardown()
		return nil, err
	}

	go c.pump()
	return c, nil
}

// Track is the VP8 track to add to a PeerConnection.
func (c *Capture) Track() *webrtc.TrackLocalStaticRTP { return c.track }

// Done is closed once the capture has fully stopped (gst exited and the pump
// has returned), whether by Stop, context cancellation or a gst crash.
func (c *Capture) Done() <-chan struct{} { return c.done }

// Err returns why the capture stopped on its own, if it did.
func (c *Capture) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// RequestKeyframe is best effort. gst-launch-1.0 cannot be told to force a
// keyframe (there is no signal hook from outside the process), so the
// encoder's keyframe-max-dist=30 is what actually guarantees recovery within
// about 2 seconds at the default 15 fps. The call therefore only restarts the
// gst child when the pipeline looks stalled (no RTP for stallThreshold), which
// both produces a fresh keyframe and recovers from a wedged source.
func (c *Capture) RequestKeyframe() {
	c.keyframeReqs.Add(1)
	last := time.Unix(0, c.lastPacket.Load())
	if time.Since(last) < stallThreshold {
		return
	}
	if !c.restarting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.restarting.Store(false)
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		c.log.Warn("screen: pipeline stalled, restarting encoder")
		c.killGst()
		if err := c.startGst(); err != nil {
			c.log.Error("screen: restart failed", "err", err)
			c.setErr(err)
			c.cancel()
		}
	}()
}

// HandleRTCP reads RTCP from the sender bound to Track() and calls
// RequestKeyframe on PLI/FIR. It blocks until the sender is closed, so run it
// in its own goroutine.
func (c *Capture) HandleRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				c.RequestKeyframe()
			}
		}
	}
}

// Stop kills gst, closes the portal session and releases the UDP socket.
func (c *Capture) Stop() error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		<-c.done
		return nil
	}
	c.stopped = true
	c.mu.Unlock()
	c.cancel()
	c.killGst()
	<-c.done
	return nil
}

func (c *Capture) setErr(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

// teardown releases everything; used on StartCapture failure and by pump.
func (c *Capture) teardown() {
	c.cancel()
	c.killGst()
	if c.portal != nil {
		if err := c.portal.Close(); err != nil {
			c.log.Debug("screen: portal close", "err", err)
		}
		c.portal = nil
	}
	_ = c.udp.Close()
}

// startGst launches the encoder for the current source. It waits until gst
// either produced its first packet, survived gstSettleDelay, or exited.
func (c *Capture) startGst() error {
	var cmd *exec.Cmd
	if c.portal != nil {
		// ExtraFiles[0] becomes fd 3 in the child.
		args := PipeWireCaptureArgs(c.opt, 3, c.portal.Node, c.port)
		cmd = exec.Command(args[0], args[1:]...)
		cmd.ExtraFiles = []*os.File{c.portal.Remote}
	} else {
		args := X11CaptureArgs(c.opt, c.port)
		cmd = exec.Command(args[0], args[1:]...)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	c.log.Debug("screen: starting encoder", "argv", cmd.Args)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("screen: starting %s: %w", GstBinary, err)
	}
	c.mu.Lock()
	c.cmd, c.stderr = cmd, stderr
	c.mu.Unlock()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	c.mu.Lock()
	c.exited = exited
	c.mu.Unlock()

	startedAt := time.Now()
	settle := time.NewTimer(gstSettleDelay)
	defer settle.Stop()
	deadline := time.NewTimer(GstStartupTimeout)
	defer deadline.Stop()
	for {
		select {
		case err := <-exited:
			// Put it back so pump sees the exit too.
			exited <- err
			return fmt.Errorf("screen: %s exited during startup: %w\n%s", GstBinary, err, trimStderr(stderr))
		case <-settle.C:
			return nil // alive and quiet: a static desktop produces few frames
		case <-deadline.C:
			return fmt.Errorf("screen: %s produced nothing within %s\n%s", GstBinary, GstStartupTimeout, trimStderr(stderr))
		case <-time.After(50 * time.Millisecond):
			if t := c.lastPacket.Load(); t != 0 && time.Unix(0, t).After(startedAt) {
				return nil
			}
		}
	}
}

func (c *Capture) killGst() {
	c.mu.Lock()
	cmd, exited := c.cmd, c.exited
	c.cmd, c.exited = nil, nil
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-exited:
		exited <- err
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		err := <-exited
		exited <- err
	}
}

// pump copies RTP from the loopback socket into the track until the socket
// is closed. It also watches the gst child and stops everything if it dies.
func (c *Capture) pump() {
	defer close(c.done)
	defer c.teardown()

	go c.watchGst()

	buf := make([]byte, 2048)
	for {
		n, _, err := c.udp.ReadFromUDP(buf)
		if err != nil {
			if c.ctx.Err() == nil {
				c.setErr(err)
			}
			return
		}
		c.lastPacket.Store(time.Now().UnixNano())
		if _, err := c.track.Write(buf[:n]); err != nil && !errors.Is(err, webrtc.ErrUnbindFailed) {
			// ErrClosedPipe-ish errors mean the peer connection went away.
			c.log.Debug("screen: track write", "err", err)
		}
	}
}

// watchGst stops the capture when the gst child exits on its own (unless a
// restart is in progress).
func (c *Capture) watchGst() {
	for {
		c.mu.Lock()
		exited := c.exited
		c.mu.Unlock()
		if exited == nil {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		select {
		case <-c.ctx.Done():
			return
		case err := <-exited:
			exited <- err
			c.mu.Lock()
			current := c.exited == exited
			stderr := c.stderr
			c.mu.Unlock()
			if !current || c.restarting.Load() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if c.ctx.Err() == nil {
				c.log.Error("screen: encoder exited", "err", err, "stderr", trimStderr(stderr))
				c.setErr(fmt.Errorf("screen: %s exited: %w", GstBinary, err))
				_ = c.udp.Close() // unblocks pump
			}
			return
		}
	}
}

func trimStderr(b *bytes.Buffer) string {
	if b == nil {
		return ""
	}
	s := bytes.TrimSpace(b.Bytes())
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return string(s)
}
