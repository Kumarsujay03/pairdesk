package input

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Kumarsujay03/pairdesk/internal/screen"
)

func TestCheckAccessMatchesRealPermission(t *testing.T) {
	err := CheckAccess()
	writable := unix.Access(DevicePath, unix.W_OK) == nil
	switch {
	case writable && err != nil:
		t.Errorf("device writable but CheckAccess failed: %v", err)
	case !writable && err == nil:
		t.Error("device not writable but CheckAccess passed")
	case !writable && errors.Is(err, ErrNoAccess):
		for _, want := range []string{"usermod -aG input", "udev"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q: %v", want, err)
			}
		}
	}
}

func TestNewInjectorRejectsBadSize(t *testing.T) {
	if _, err := NewInjector(0, 100); err == nil {
		t.Error("expected error for zero width")
	}
}

func TestNewInjectorWithoutAccess(t *testing.T) {
	if unix.Access(DevicePath, unix.W_OK) == nil {
		t.Skip("uinput is writable here; skipping permission-denied path")
	}
	_, err := NewInjector(1920, 1080)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "usermod -aG input") {
		t.Errorf("error should explain how to grant access: %v", err)
	}
}

func TestApplyOnClosedInjector(t *testing.T) {
	inj := &Injector{closed: true, w: 10, h: 10}
	if err := inj.Apply(screen.InputEvent{Kind: screen.KindMove}); err == nil {
		t.Error("expected error on closed injector")
	}
}
