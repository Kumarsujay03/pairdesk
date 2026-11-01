// Package notify sends desktop notifications through notify-send when the
// user has them enabled. It never includes message bodies for DMs in groups
// the user did not opt into; callers pass what to show.
package notify

import (
	"context"
	"strings"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Notifier rate-limits and dispatches notifications.
type Notifier struct {
	Enabled bool
	mu      sync.Mutex
	last    map[string]time.Time
	bin     string
}

// New looks up notify-send; Enabled is false when it is missing.
func New(enabled bool) *Notifier {
	n := &Notifier{Enabled: enabled, last: map[string]time.Time{}}
	// Never pop desktop notifications from a test binary or when the user
	// opted out through the environment.
	if strings.HasSuffix(os.Args[0], ".test") || os.Getenv("PAIRDESK_NO_NOTIFY") != "" {
		n.Enabled = false
		return n
	}
	if p, err := exec.LookPath("notify-send"); err == nil {
		n.bin = p
	} else {
		n.Enabled = false
	}
	return n
}

// Send shows a notification. key collapses bursts: the same key fires at
// most once every 3 seconds. urgency is low|normal|critical.
func (n *Notifier) Send(key, title, body, urgency string) {
	if n == nil || !n.Enabled || n.bin == "" {
		return
	}
	n.mu.Lock()
	if t, ok := n.last[key]; ok && time.Since(t) < 3*time.Second {
		n.mu.Unlock()
		return
	}
	n.last[key] = time.Now()
	n.mu.Unlock()
	if urgency == "" {
		urgency = "normal"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	go func() {
		defer cancel()
		cmd := exec.CommandContext(ctx, n.bin, "-a", "PairDesk", "-u", urgency, "-t", "5000", title, body)
		cmd.Env = os.Environ()
		_ = cmd.Run()
	}()
}
