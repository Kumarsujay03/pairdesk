package term

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"

	"github.com/Kumarsujay03/pairdesk/internal/p2p"
)

// VisitorOptions configures RunVisitor.
type VisitorOptions struct {
	Stdin     io.Reader
	Stdout    io.Writer
	Escape    byte // default 0x1d (ctrl-]); Escape then 'q' detaches, Escape then anything else passes both through
	OnCommand func(CommandEvent)
	OnSudo    func(cmd string) // informational: the host is deciding on a sudo command
	OnShare   func(on bool)    // shared-terminal state announced by the host
	ReadOnly  bool             // do not forward stdin
}

// ErrDetached is returned when the user typed the escape sequence.
var ErrDetached = errors.New("term: detached")

// RunVisitor drives the local terminal against a hosted PTY over ch. If
// Stdin is a terminal it is put into raw mode and restored on return; if
// Stdout is a terminal its size is sent on start and on SIGWINCH. It returns
// nil when the host says bye, ErrDetached on the escape sequence, or the
// channel error.
// ResetSequence puts a terminal back into a known state after a raw
// session: mouse reporting and bracketed paste off, main screen, full
// scroll region, default attributes, cursor shown, autowrap on, screen
// cleared, and the window title back to "pairdesk".
const ResetSequence = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l" +
	"\x1b[?1049l\x1b[r\x1b[0m\x1b[?25h\x1b[?7h\x1b[2J\x1b[H\x1b]0;pairdesk\x07"

func RunVisitor(ctx context.Context, ch p2p.Channel, opt VisitorOptions) error {
	if opt.Stdin == nil {
		opt.Stdin = os.Stdin
	}
	if opt.Stdout == nil {
		opt.Stdout = os.Stdout
	}
	if opt.Escape == 0 {
		opt.Escape = 0x1d
	}

	// Raw mode. On the way out, undo whatever the remote programs left in
	// the local terminal (scroll regions from apt's progress bar, mouse
	// modes, a changed window title, alt screen), or the UI paints garbage.
	if f, ok := opt.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		state, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return err
		}
		defer func() {
			_, _ = io.WriteString(opt.Stdout, ResetSequence)
			_ = term.Restore(int(f.Fd()), state)
		}()
	}

	var wmu sync.Mutex
	send := func(typ byte, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return WriteFrame(ch, typ, payload)
	}

	// Window size.
	var sizeFD = -1
	if f, ok := opt.Stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		sizeFD = int(f.Fd())
	}
	sendSize := func() {
		if sizeFD < 0 {
			return
		}
		if cols, rows, err := term.GetSize(sizeFD); err == nil {
			_ = send(FrameResize, encodeResize(uint16(cols), uint16(rows)))
		}
	}
	sendSize()

	result := make(chan error, 3)
	stop := make(chan struct{})
	var stopOnce sync.Once
	finish := func(err error) {
		stopOnce.Do(func() { close(stop) })
		select {
		case result <- err:
		default:
		}
	}

	if sizeFD >= 0 {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGWINCH)
		defer signal.Stop(sig)
		go func() {
			for {
				select {
				case <-sig:
					sendSize()
				case <-stop:
					return
				}
			}
		}()
	}

	// Host → stdout.
	go func() {
		for {
			typ, payload, err := ReadFrame(ch)
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				finish(err)
				return
			}
			switch typ {
			case FrameData:
				if _, err := opt.Stdout.Write(payload); err != nil {
					finish(err)
					return
				}
			case FrameCommand:
				if ev, err := decodeCommand(payload); err == nil && opt.OnCommand != nil {
					opt.OnCommand(ev)
				}
			case FrameSudo:
				if opt.OnSudo != nil {
					var m struct {
						Cmd string `json:"cmd"`
					}
					if err := jsonUnmarshal(payload, &m); err == nil {
						opt.OnSudo(m.Cmd)
					}
				}
			case FrameShare:
				if opt.OnShare != nil {
					opt.OnShare(len(payload) == 1 && payload[0] == 1)
				}
			case FrameBye:
				finish(nil)
				return
			}
		}
	}()

	// Stdin → host. This goroutine may outlive the call while blocked on a
	// terminal read; it exits on the next keystroke.
	if !opt.ReadOnly {
		go func() {
			buf := make([]byte, 4096)
			escaped := false
			for {
				n, err := opt.Stdin.Read(buf)
				if n > 0 {
					select {
					case <-stop:
						return
					default:
					}
					out := make([]byte, 0, n+1)
					for _, b := range buf[:n] {
						if escaped {
							escaped = false
							if b == 'q' {
								if len(out) > 0 {
									_ = send(FrameData, out)
								}
								finish(ErrDetached)
								return
							}
							if b == 's' && !opt.ReadOnly {
								// Ask the host to make this a shared terminal.
								if len(out) > 0 {
									_ = send(FrameData, out)
									out = out[:0]
								}
								_ = send(FrameShare, []byte{1})
								continue
							}
							out = append(out, opt.Escape, b)
							continue
						}
						if b == opt.Escape {
							escaped = true
							continue
						}
						out = append(out, b)
					}
					if len(out) > 0 {
						if err := send(FrameData, out); err != nil {
							finish(err)
							return
						}
					}
				}
				if err != nil {
					if !errors.Is(err, io.EOF) {
						finish(fmt.Errorf("term: stdin: %w", err))
					}
					return
				}
			}
		}()
	}

	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}
	stopOnce.Do(func() { close(stop) })
	if errors.Is(err, ErrDetached) {
		_ = send(FrameBye, nil)
	}
	_ = ch.Close()
	return err
}
