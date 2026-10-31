// Package input injects keyboard and pointer events on the host via
// /dev/uinput while a control-mode screen session is active.
package input

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/bendahl/uinput"
	"golang.org/x/sys/unix"

	"github.com/Kumarsujay03/pairdesk/internal/screen"
)

// DevicePath is the uinput character device.
const DevicePath = "/dev/uinput"

// ErrNoAccess is wrapped by CheckAccess and NewInjector when /dev/uinput
// cannot be written; the error text explains how to grant access.
var ErrNoAccess = errors.New("input: no write access to " + DevicePath)

const accessHelp = `grant your user access to uinput:

    sudo usermod -aG input $USER
    sudo tee /etc/udev/rules.d/80-pairdesk-uinput.rules <<'R'
    KERNEL=="uinput", SUBSYSTEM=="misc", MODE="0660", GROUP="input", OPTIONS+="static_node=uinput"
    R
    sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=uinput

then log out and back in (group changes apply to new sessions only). If
/dev/uinput is missing, run: sudo modprobe uinput`

// CheckAccess reports whether /dev/uinput exists and is writable by the
// current process without creating any device.
func CheckAccess() error {
	if _, err := os.Stat(DevicePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("input: %s does not exist (is the uinput module loaded? try: sudo modprobe uinput): %w", DevicePath, err)
		}
		return fmt.Errorf("input: stat %s: %w", DevicePath, err)
	}
	if err := unix.Access(DevicePath, unix.W_OK); err != nil {
		return fmt.Errorf("%w (%v); %s", ErrNoAccess, err, accessHelp)
	}
	return nil
}

// Injector owns three virtual devices: a keyboard, an absolute-position
// touchpad (for pointer moves and left/right buttons) and a relative mouse
// (for the wheel and the middle button).
type Injector struct {
	mu     sync.Mutex
	kbd    uinput.Keyboard
	pad    uinput.TouchPad
	mouse  uinput.Mouse
	w, h   int32
	closed bool
}

// NewInjector creates the virtual devices for a screen of screenW x screenH
// pixels. Pointer coordinates in later Apply calls are clamped to that area.
func NewInjector(screenW, screenH int32) (*Injector, error) {
	if screenW <= 0 || screenH <= 0 {
		return nil, fmt.Errorf("input: invalid screen size %dx%d", screenW, screenH)
	}
	if err := CheckAccess(); err != nil {
		return nil, err
	}
	inj := &Injector{w: screenW, h: screenH}
	var err error
	if inj.kbd, err = uinput.CreateKeyboard(DevicePath, []byte("pairdesk keyboard")); err != nil {
		return nil, wrapCreate("keyboard", err)
	}
	if inj.pad, err = uinput.CreateTouchPad(DevicePath, []byte("pairdesk pointer"), 0, screenW-1, 0, screenH-1); err != nil {
		inj.kbd.Close()
		return nil, wrapCreate("touchpad", err)
	}
	if inj.mouse, err = uinput.CreateMouse(DevicePath, []byte("pairdesk wheel")); err != nil {
		inj.pad.Close()
		inj.kbd.Close()
		return nil, wrapCreate("mouse", err)
	}
	return inj, nil
}

func wrapCreate(what string, err error) error {
	if errors.Is(err, fs.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		return fmt.Errorf("%w: creating %s: %v; %s", ErrNoAccess, what, err, accessHelp)
	}
	return fmt.Errorf("input: creating virtual %s: %w", what, err)
}

// Apply injects one event.
func (i *Injector) Apply(ev screen.InputEvent) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return errors.New("input: injector closed")
	}
	switch ev.Kind {
	case screen.KindKey:
		if ev.Code <= 0 || ev.Code > 255 {
			return fmt.Errorf("input: key code %d out of range", ev.Code)
		}
		if ev.Press {
			return i.kbd.KeyDown(ev.Code)
		}
		return i.kbd.KeyUp(ev.Code)
	case screen.KindMove:
		x := min(max(ev.X, 0), i.w-1)
		y := min(max(ev.Y, 0), i.h-1)
		return i.pad.MoveTo(x, y)
	case screen.KindClick:
		switch ev.Button {
		case 1:
			if ev.Press {
				return i.pad.LeftPress()
			}
			return i.pad.LeftRelease()
		case 2:
			if ev.Press {
				return i.pad.RightPress()
			}
			return i.pad.RightRelease()
		case 3:
			if ev.Press {
				return i.mouse.MiddlePress()
			}
			return i.mouse.MiddleRelease()
		default:
			return fmt.Errorf("input: unknown button %d", ev.Button)
		}
	case screen.KindScroll:
		if ev.Delta == 0 {
			return nil
		}
		return i.mouse.Wheel(false, int32(ev.Delta))
	default:
		return fmt.Errorf("input: unknown event kind %q", ev.Kind)
	}
}

// Close destroys the virtual devices.
func (i *Injector) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	var errs []error
	for _, c := range []interface{ Close() error }{i.mouse, i.pad, i.kbd} {
		if c != nil {
			if err := c.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
