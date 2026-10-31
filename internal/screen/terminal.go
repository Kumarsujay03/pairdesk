package screen

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/bendahl/uinput"
	"golang.org/x/sys/unix"
)

// PointerStep is how far one arrow key moves the pointer in pointer mode.
const PointerStep = 20

// ErrExitRequested is returned by ReadTerminalInput when the operator presses
// ctrl+] then q.
var ErrExitRequested = errors.New("screen: exit requested")

// ReadTerminalInput turns keystrokes from r (normally os.Stdin) into
// InputEvents until ctx is cancelled, r hits EOF, or the operator exits.
//
// If r is a terminal it is put into raw mode for the duration of the call and
// restored on return. Keys are mapped through LookupKey and emitted as
// press/release pairs. The control prefix is ctrl+]:
//
//	ctrl+] q   exit (returns ErrExitRequested)
//	ctrl+] p   toggle pointer mode
//	ctrl+] ]   send a literal ctrl+]
//
// In pointer mode the arrow keys move the pointer by PointerStep pixels and
// emit "move" events (absolute, clamped at 0; the host clamps to its screen),
// Enter emits a left click, space a right click, PageUp/PageDown scroll by
// one tick. Every other key is still forwarded as a key event.
//
// This terminal-driven control is a stopgap: autovideosink exposes no input
// hooks without cgo. A native viewer window with pointer capture is a later
// improvement.
//
// Note: a blocking Read on a terminal cannot be interrupted, so after ctx is
// cancelled the reader goroutine lingers until one more byte arrives.
func ReadTerminalInput(ctx context.Context, r io.Reader, emit func(InputEvent)) error {
	if f, ok := r.(*os.File); ok {
		restore, err := makeRaw(int(f.Fd()))
		if err == nil {
			defer restore()
		}
	}

	type chunk struct {
		b   []byte
		err error
	}
	ch := make(chan chunk, 1)
	go func() {
		for {
			buf := make([]byte, 256)
			n, err := r.Read(buf)
			select {
			case ch <- chunk{buf[:n], err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	st := &termState{emit: emit}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c := <-ch:
			for _, seq := range SplitSequences(c.b) {
				if err := st.handle(seq); err != nil {
					return err
				}
			}
			if c.err != nil {
				if c.err == io.EOF {
					return nil
				}
				return c.err
			}
		}
	}
}

// termState is the small state machine behind ReadTerminalInput, separated
// so tests can drive it without a terminal.
type termState struct {
	emit    func(InputEvent)
	prefix  bool // ctrl+] seen, waiting for the command key
	pointer bool
	x, y    int32
}

const ctrlRightBracket = 0x1d

func (s *termState) handle(seq []byte) error {
	if s.prefix {
		s.prefix = false
		if len(seq) == 1 {
			switch seq[0] {
			case 'q', 'Q':
				return ErrExitRequested
			case 'p', 'P':
				s.pointer = !s.pointer
				return nil
			case ctrlRightBracket, ']':
				s.emitKey(Key{Code: uinput.KeyRightbrace, Ctrl: true})
				return nil
			}
		}
		return nil // unknown command: swallow
	}
	if len(seq) == 1 && seq[0] == ctrlRightBracket {
		s.prefix = true
		return nil
	}
	k, ok := LookupKey(seq)
	if !ok {
		return nil
	}
	if s.pointer && !k.Ctrl && !k.Alt && !k.Shift {
		switch k.Code {
		case uinput.KeyUp:
			s.move(0, -PointerStep)
			return nil
		case uinput.KeyDown:
			s.move(0, PointerStep)
			return nil
		case uinput.KeyLeft:
			s.move(-PointerStep, 0)
			return nil
		case uinput.KeyRight:
			s.move(PointerStep, 0)
			return nil
		case uinput.KeyEnter:
			s.click(1)
			return nil
		case uinput.KeySpace:
			s.click(2)
			return nil
		case uinput.KeyPageup:
			s.emit(InputEvent{Kind: KindScroll, Delta: 1})
			return nil
		case uinput.KeyPagedown:
			s.emit(InputEvent{Kind: KindScroll, Delta: -1})
			return nil
		}
	}
	s.emitKey(k)
	return nil
}

func (s *termState) emitKey(k Key) {
	for _, ev := range k.Events() {
		s.emit(ev)
	}
}

func (s *termState) move(dx, dy int32) {
	s.x = max(0, s.x+dx)
	s.y = max(0, s.y+dy)
	s.emit(InputEvent{Kind: KindMove, X: s.x, Y: s.y})
}

func (s *termState) click(button int) {
	s.emit(InputEvent{Kind: KindClick, Button: button, Press: true})
	s.emit(InputEvent{Kind: KindClick, Button: button, Press: false})
}

// makeRaw puts fd into raw mode and returns a function that restores it.
func makeRaw(fd int) (func(), error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }, nil
}
