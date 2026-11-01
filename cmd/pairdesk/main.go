// Command pairdesk is the PairDesk terminal client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/admin"
	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous/memory"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous/supabase"
	"github.com/Kumarsujay03/pairdesk/internal/store"
	"github.com/Kumarsujay03/pairdesk/internal/tui"
	"github.com/Kumarsujay03/pairdesk/internal/update"
)

// Set by the linker: -X main.version=1.2.3 -X main.supabaseURL=... -X main.supabaseAnon=...
var (
	version      = "0.0.0-dev"
	supabaseURL  string
	supabaseAnon string
)

func main() {
	demo := flag.Bool("demo", false, "run against an in-memory backend (no network; the login code is 000000)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	doUpdate := flag.Bool("update", false, "download and install the latest release, then exit")
	reduced := flag.Bool("reduced-motion", false, "disable animations (good for slow SSH links)")
	flag.Parse()

	if *showVersion {
		fmt.Println("pairdesk", version)
		return
	}
	if *doUpdate {
		tag, err := update.Apply(context.Background(), version, func(s string) {
			if strings.HasPrefix(s, "downloading") {
				fmt.Printf("\r• %-60s", s) // one moving line for the percentage
				return
			}
			fmt.Println("\n•", s)
		})
		if err != nil {
			fmt.Println("update:", err)
			if tag != "" {
				fmt.Println("latest is", tag)
			}
			os.Exit(1)
		}
		fmt.Println("now on", tag)
		return
	}
	app.Version = version

	// Resolve our path now: once an update has renamed a new file over us,
	// /proc/self/exe reads "… (deleted)" and could not be exec'd.
	exe, exeErr := os.Executable()
	if exeErr == nil {
		exe, _ = filepath.EvalSymlinks(exe)
	}
	err := run(*demo, *reduced)
	if errors.Is(err, tui.ErrRestart) {
		// A release was installed: become the new binary with the same arguments.
		e := exeErr
		if e == nil {
			e = syscall.Exec(exe, os.Args, os.Environ())
		}
		fmt.Fprintln(os.Stderr, "pairdesk: updated, but could not restart:", e, "- start pairdesk again")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pairdesk:", err)
		os.Exit(1)
	}
}

func run(demo, reduced bool) error {
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	if demo {
		// Keep demo state away from the real config so a demo never clobbers
		// a real identity or login.
		paths.Config = filepath.Join(paths.Cache, "demo", "config")
		paths.State = filepath.Join(paths.Cache, "demo", "state")
	}
	st, err := store.Open(paths)
	if err != nil {
		return err
	}
	logger, closeLog := openLog(paths.State)
	defer closeLog()
	slog.SetDefault(logger)

	var be rendezvous.Backend
	if demo {
		be = memory.New().Client()
	} else {
		url := supabaseURL
		if v := os.Getenv("PAIRDESK_SUPABASE_URL"); v != "" {
			url = v
		}
		anon := supabaseAnon
		if v := os.Getenv("PAIRDESK_SUPABASE_ANON"); v != "" {
			anon = v
		}
		if url == "" || anon == "" {
			return fmt.Errorf("no backend configured: this build has no Supabase URL baked in; set PAIRDESK_SUPABASE_URL and PAIRDESK_SUPABASE_ANON, or run with --demo")
		}
		be = supabase.New(supabase.Config{URL: url, AnonKey: anon, Logger: logger})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a := app.New(app.Options{Backend: be, Store: st, Logger: logger})
	if err := a.Prepare(); err != nil {
		logger.Error("start", "err", err)
		return err
	}
	logger.Info("pairdesk starting", "version", version, "demo", demo)
	lock := admin.NewLock(paths.Config)
	applock := admin.NewLockAt(filepath.Join(paths.Config, "app.pass"))
	applock.MinLen = 4 // guards this terminal only; guesses are rate limited
	hooks, ready := wire(a, lock)
	var auto func(ctx context.Context, progress func(string)) (bool, error)
	if !demo && os.Getenv("PAIRDESK_NO_AUTOUPDATE") == "" {
		auto = func(ctx context.Context, progress func(string)) (bool, error) {
			return autoUpdate(ctx, a, logger, progress)
		}
	}
	return tui.RunWith(ctx, a, tui.Options{Version: version, Demo: demo, ReducedMotion: reduced, Hooks: hooks, ProgramReady: ready, Boot: a.Resume, AppLock: applock, Unlocked: lock.Trust, AutoUpdate: auto})
}

// autoUpdate installs a newer release at start-up when the user has not
// turned that off. It never blocks start-up on a bad network: a slow or
// failed check just means starting the current version.
func autoUpdate(ctx context.Context, a *app.App, logger *slog.Logger, progress func(string)) (bool, error) {
	if !a.AutoUpdateOn() || !update.IsRelease(version) {
		return false, nil
	}
	progress("checking for updates")
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	tag, err := update.Latest(cctx)
	cancel()
	if err != nil || !update.Newer(tag, version) {
		return false, nil
	}
	cctx, cancel = context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := update.Apply(cctx, version, progress); err != nil {
		logger.Warn("auto-update failed", "tag", tag, "err", err)
		return false, nil
	}
	logger.Info("auto-updated", "from", version, "to", tag)
	return true, nil
}

// openLog writes slog output to <state>/pairdesk.log so the terminal stays
// clean for the UI.
func openLog(stateDir string) (*slog.Logger, func()) {
	f, err := os.OpenFile(filepath.Join(stateDir, "pairdesk.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})), func() { _ = f.Close() }
}
