// Command pairdesk is the PairDesk terminal client.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
		tag, err := update.Apply(context.Background(), version, func(s string) { fmt.Println("•", s) })
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

	if err := run(*demo, *reduced); err != nil {
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
	return tui.RunWith(ctx, a, tui.Options{Version: version, Demo: demo, ReducedMotion: reduced, Hooks: hooks, ProgramReady: ready, Boot: a.Resume, AppLock: applock, Unlocked: lock.Trust})
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
