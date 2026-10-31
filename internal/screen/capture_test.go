package screen

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// With no reachable session bus and no DISPLAY, StartCapture must fail
// quickly with an error that names both problems, without launching gst.
func TestStartCaptureFailsFastWithoutPortalOrDisplay(t *testing.T) {
	if _, err := exec.LookPath(GstBinary); err != nil {
		t.Skip("gst-launch-1.0 not installed")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/pairdesk-test-bus")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	c, err := StartCapture(ctx, CaptureOptions{})
	elapsed := time.Since(start)
	if err == nil {
		c.Stop()
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrPortalUnavailable) {
		t.Errorf("error should wrap ErrPortalUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "DISPLAY") {
		t.Errorf("error should mention the missing DISPLAY: %v", err)
	}
	if elapsed > 15*time.Second {
		t.Errorf("took %s, should fail fast", elapsed)
	}
	t.Logf("error after %s: %v", elapsed, err)
}
