package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/Kumarsujay03/pairdesk/internal/input"
	"github.com/Kumarsujay03/pairdesk/internal/p2p"
	"github.com/Kumarsujay03/pairdesk/internal/screen"
)

// startCapture begins screen capture on the host and wires the track into
// the P2P config so it is part of the answer.
func (m *Manager) startCapture(ctx context.Context, cfg *p2p.Config) (func(), error) {
	cap, err := screen.StartCapture(ctx, screen.CaptureOptions{Logger: m.log})
	if err != nil {
		return nil, err
	}
	cfg.VideoTrack = cap.Track()
	cfg.VideoSender = func(snd *webrtc.RTPSender) { go cap.HandleRTCP(snd) }
	return func() { _ = cap.Stop() }, nil
}

// hostScreen runs the host side of a screen session until it ends.
func (m *Manager) hostScreen(ctx context.Context, s *Session, conn *p2p.Conn) {
	if !s.Control {
		<-conn.Done()
		return
	}
	actx, acancel := context.WithTimeout(ctx, 20*time.Second)
	ch, err := conn.AcceptChannel(actx)
	acancel()
	if err != nil {
		s.setReason("no input channel")
		return
	}
	inj, err := input.NewInjector(1920, 1080)
	if err != nil {
		s.setReason("input: " + err.Error())
		m.app.Emit(appNotice(err.Error()))
		return
	}
	defer inj.Close()
	buf := make([]byte, screen.InputEventSize)
	n := 0
	for {
		if _, err := io.ReadFull(ch, buf); err != nil {
			return
		}
		ev, err := screen.DecodeInput(buf)
		if err != nil {
			continue
		}
		n++
		if n%500 == 0 && s.writer != nil {
			_ = s.writer.Input([]byte(fmt.Sprintf("[%d input events]", n)))
		}
		_ = inj.Apply(ev)
	}
}

// runVisitorScreen shows the host's screen in a window and, in control
// mode, forwards keyboard and pointer input from the terminal.
func (m *Manager) runVisitorScreen(ctx context.Context, s *Session) error {
	if m.TermUI == nil {
		return errors.New("no terminal available")
	}
	trackCh := make(chan *webrtc.TrackRemote, 1)
	s.conn.OnTrack(func(t *webrtc.TrackRemote) {
		select {
		case trackCh <- t:
		default:
		}
	})
	var track *webrtc.TrackRemote
	select {
	case track = <-trackCh:
	case <-time.After(20 * time.Second):
		return errors.New("no video from host")
	case <-s.conn.Done():
		return errors.New("connection closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	var inputCh p2p.Channel
	if s.Control {
		var err error
		inputCh, err = s.conn.OpenChannel(ctx, "input", true)
		if err != nil {
			return err
		}
		go m.expire(ctx, s, controlExpiry)
	}
	w, err := m.newWriter(s, "visitor")
	if err == nil {
		s.writer = w
	}
	return m.TermUI(func(stdin io.Reader, stdout io.Writer) error {
		mode := "viewing"
		if s.Control {
			mode = "controlling"
		}
		fmt.Fprintf(stdout, "\r\n\x1b[36m● %s %s's screen in a separate window · ctrl+] then q to end", mode, s.Peer.Username)
		if s.Control {
			fmt.Fprintf(stdout, " · ctrl+] p toggles pointer mode")
		}
		fmt.Fprintf(stdout, "\x1b[0m\r\n")
		var onInput func(screen.InputEvent)
		if inputCh != nil {
			onInput = func(ev screen.InputEvent) { _, _ = inputCh.Write(screen.EncodeInput(ev)) }
		}
		v, err := screen.StartViewer(ctx, track, screen.ViewerOptions{Title: "PairDesk · " + s.Peer.Username, Logger: m.log, OnInput: onInput})
		if err != nil {
			return err
		}
		defer v.Stop()
		errc := make(chan error, 1)
		go func() {
			if onInput != nil {
				errc <- screen.ReadTerminalInput(ctx, stdin, onInput)
				return
			}
			// View only: wait for ctrl+] q.
			b := make([]byte, 1)
			esc := false
			for {
				if _, err := stdin.Read(b); err != nil {
					errc <- err
					return
				}
				if esc && b[0] == 'q' {
					errc <- nil
					return
				}
				esc = b[0] == 0x1d
			}
		}()
		select {
		case err := <-errc:
			if errors.Is(err, screen.ErrExitRequested) {
				return nil
			}
			return err
		case <-v.Done():
			return v.Err()
		case <-s.conn.Done():
			return nil
		case <-ctx.Done():
			return nil
		}
	})
}
