package term

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// IsolationMode says what StartHost does when the visitor shell cannot be
// put in its own namespaces.
type IsolationMode int

const (
	// IsolateRequired (the zero value) refuses to start the shell without
	// isolation: StartHost returns ErrNoIsolation.
	IsolateRequired IsolationMode = iota
	// IsolateBestEffort starts the shell unisolated when the sandbox cannot
	// be set up; Host.Isolated reports false so the caller can warn.
	IsolateBestEffort
	// IsolateOff never tries to isolate.
	IsolateOff
)

// ErrNoIsolation is returned (wrapped) when the visitor shell cannot be
// isolated and HostOptions.Isolate is IsolateRequired.
var ErrNoIsolation = errors.New("term: visitor shell cannot be isolated")

// sandboxPlan is the wrapper command line the visitor shell runs under.
//
// The sandbox gives the visitor its own user, PID and mount namespaces:
//
//   - PID namespace: the whole process tree dies with the sandbox init, so a
//     `setsid nohup … &` cannot outlive Stop, and /proc shows only the
//     visitor's processes (the host client's /proc/<pid>/fd, environ and
//     cwd are out of reach).
//   - Mount namespace: every HiddenPath is covered by an empty tmpfs, so
//     the PairDesk config/state/cache dirs and ShimDir/ipc do not exist
//     from the visitor's point of view; ShimDir/bin (shim, askpass, rc
//     files) is bind-mounted read-only.
//   - User namespace: needed for the mounts; the visitor is "root" of the
//     namespace only, which maps to the host's uid outside. $USER and
//     $HOME stay the host's.
//
// The shell keeps the PTY as its controlling tty (no --new-session), so
// job control works. bwrap is preferred; unshare(1) is the fallback.
type sandboxPlan struct {
	tool string // "bwrap" or "unshare"
	argv []string
}

// IsolationAvailable reports whether a visitor shell can be sandboxed on
// this machine (bwrap or unprivileged user namespaces). Callers use it to
// explain a refused session; tests use it to skip.
func IsolationAvailable() bool {
	dir, err := os.MkdirTemp("", "pairdesk-probe")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	plan, err := buildSandbox("/bin/true", nil, nil, dir)
	if err != nil {
		return false
	}
	return plan.probe(context.Background(), os.Environ()) == nil
}

// buildSandbox returns the sandbox wrapper for shell+args, or an error
// when neither bwrap nor unshare is available. hidden are directories to
// cover with tmpfs (missing ones are skipped: nothing to hide); roBin is
// bind-mounted read-only on itself.
func buildSandbox(shell string, args []string, hidden []string, roBin string) (*sandboxPlan, error) {
	var hide []string
	seen := map[string]bool{}
	for _, p := range hidden {
		if p == "" || seen[p] {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			continue
		}
		seen[p] = true
		hide = append(hide, p)
	}
	// Parents before children so a tmpfs over a parent does not hide a
	// later mount point (bwrap and the unshare script both mount in order).
	sort.Slice(hide, func(i, j int) bool { return len(hide[i]) < len(hide[j]) })

	if bw, err := exec.LookPath("bwrap"); err == nil {
		argv := []string{bw, "--unshare-user", "--unshare-pid", "--die-with-parent",
			"--bind", "/", "/", "--dev-bind", "/dev", "/dev", "--proc", "/proc"}
		for _, p := range hide {
			argv = append(argv, "--tmpfs", p)
		}
		argv = append(argv, "--ro-bind", roBin, roBin, "--", shell)
		argv = append(argv, args...)
		return &sandboxPlan{tool: "bwrap", argv: argv}, nil
	}
	if un, err := exec.LookPath("unshare"); err == nil {
		var sb strings.Builder
		sb.WriteString("set -e\n")
		for _, p := range hide {
			fmt.Fprintf(&sb, "mount -t tmpfs -o mode=0700 none %s\n", shellQuote(p))
		}
		q := shellQuote(roBin)
		fmt.Fprintf(&sb, "mkdir -p %s\nmount --bind %s %s\n", q, q, q)
		// A remount inside a user namespace must repeat the flags the
		// kernel locked on the parent mount (nosuid/nodev on most /tmp
		// and /home), and which ones that is varies; try the
		// combinations, never noexec since the shim scripts must run.
		sb.WriteString("ro=0\nfor o in ro ro,nosuid,nodev ro,nosuid ro,nodev; do\n")
		fmt.Fprintf(&sb, "  if mount -o remount,bind,$o %s %s 2>/dev/null; then ro=1; break; fi\ndone\n", q, q)
		fmt.Fprintf(&sb, "[ \"$ro\" = 1 ] || { echo 'pairdesk: cannot make the shim dir read-only' >&2; exit 125; }\n")
		sb.WriteString("exec \"$0\" \"$@\"\n")
		argv := []string{un, "-Urmpf", "--mount-proc=/proc", "--kill-child", "--", "/bin/sh", "-c", sb.String(), shell}
		argv = append(argv, args...)
		return &sandboxPlan{tool: "unshare", argv: argv}, nil
	}
	return nil, errors.New("neither bwrap nor unshare found on PATH")
}

// probe runs the plan with /bin/true in place of the shell so a sandbox
// that cannot be set up (apparmor, locked-down kernel, missing mount
// points) is reported before the session starts.
func (p *sandboxPlan) probe(ctx context.Context, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv := append([]string{}, p.argv...)
	// The shell is the first argument after "--" (bwrap) or the $0 after
	// the script (unshare); both plans place it at a known index.
	switch p.tool {
	case "bwrap":
		for i, a := range argv {
			if a == "--" {
				argv = append(argv[:i+1], "/bin/true")
				break
			}
		}
	case "unshare":
		argv = append(argv[:8], "/bin/true")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", p.tool, msg)
	}
	return nil
}
