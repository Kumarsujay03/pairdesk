package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kumarsujay03/pairdesk/internal/admin"
	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/session"
	"github.com/Kumarsujay03/pairdesk/internal/sessionlog"
	"github.com/Kumarsujay03/pairdesk/internal/transfer"
	"github.com/Kumarsujay03/pairdesk/internal/tui"
)

// wire builds the session manager and the TUI hooks around it.
func wire(a *app.App, lock *admin.Lock) (tui.Hooks, func(*tea.Program)) {
	m := session.New(a, lock, a.Logger())
	var pmu sync.Mutex
	var prog *tea.Program
	m.TermUI = func(fn func(stdin io.Reader, stdout io.Writer) error) error {
		pmu.Lock()
		p := prog
		pmu.Unlock()
		if p == nil {
			return errors.New("ui not running")
		}
		if err := p.ReleaseTerminal(); err != nil {
			return err
		}
		err := fn(os.Stdin, os.Stdout)
		_ = p.RestoreTerminal()
		return err
	}
	ready := func(p *tea.Program) {
		pmu.Lock()
		prog = p
		pmu.Unlock()
		go m.PruneLogs()
	}

	request := func(kind model.SessionKind) func(ctx context.Context, user string, args []string) ([]string, error) {
		return func(ctx context.Context, user string, _ []string) ([]string, error) {
			if err := m.Request(ctx, user, kind, false); err != nil {
				return nil, friendly(err)
			}
			return []string{fmt.Sprintf("%s session with %s ended", kind, user)}, nil
		}
	}
	h := tui.Hooks{
		Board: request(model.SessionBoard),
		Watch: request(model.SessionWatch),
		Screen: func(ctx context.Context, user string, control bool) ([]string, error) {
			if err := m.Request(ctx, user, model.SessionScreen, control); err != nil {
				return nil, friendly(err)
			}
			return []string{"screen session with " + user + " ended"}, nil
		},
		Send: func(ctx context.Context, user, path string) ([]string, error) {
			if strings.HasPrefix(user, "#") {
				g, ok := a.GroupByName(user)
				if !ok {
					return nil, errors.New("no such group")
				}
				c := a.Conv(g.ID)
				if c == nil {
					return nil, errors.New("open the group first")
				}
				if err := m.ShareToGroup(ctx, c, path); err != nil {
					return nil, err
				}
				return []string{"shared " + filepath.Base(path) + " with #" + g.Name + "; members download it with /get <id>"}, nil
			}
			var last string
			err := m.Send(ctx, user, path, func(p transfer.Progress) {
				last = fmt.Sprintf("%d/%d", p.Done, p.Total)
			})
			if err != nil {
				return nil, friendly(err)
			}
			return []string{fmt.Sprintf("sent %s to %s (%s chunks)", filepath.Base(path), user, last)}, nil
		},
		Fetch: func(ctx context.Context, convID, id string) ([]string, error) {
			c := a.Conv(convID)
			if c == nil || c.Type != model.ConvGroup {
				return nil, errors.New("open the group the file was shared in first")
			}
			if err := m.Fetch(ctx, c, id, nil); err != nil {
				return nil, err
			}
			return []string{"download complete, saved under " + session.DownloadDir()}, nil
		},
		Share: func(ctx context.Context) ([]string, error) {
			return []string{"inside a board session press ctrl+] then s to ask the host to share the terminal"}, nil
		},
		Stop: func(ctx context.Context) ([]string, error) {
			n := m.StopAll("stopped by host")
			if n == 0 {
				return []string{"no live sessions"}, nil
			}
			return []string{fmt.Sprintf("ended %d session(s)", n)}, nil
		},
		End: func(ctx context.Context) ([]string, error) {
			n := m.StopAll("ended")
			return []string{fmt.Sprintf("ended %d session(s)", n)}, nil
		},
		Search: func(ctx context.Context, q string) ([]string, error) {
			hits, err := a.Search(q, "", 20)
			if err != nil {
				return nil, err
			}
			if len(hits) == 0 {
				return []string{"no matches"}, nil
			}
			var out []string
			for _, h := range hits {
				c := a.Conv(h.ConvID)
				where := h.ConvID
				if c != nil {
					where = a.ConvTitle(c)
				}
				out = append(out, fmt.Sprintf("%s  %s  %s: %s", time.Unix(0, h.At).Format("Jan 02 15:04"), where, a.Name(h.Sender), h.Text))
			}
			return out, nil
		},
		Logs: func(ctx context.Context, args []string) ([]string, error) { return logsCommand(ctx, m, args) },
		Unlock: func(ctx context.Context, pass string) error {
			if !a.IsHelperOrAdmin() {
				return app.ErrNotAdmin
			}
			return lock.Unlock(pass)
		},
		Passphrase: func(ctx context.Context, old, pass string) error {
			if !a.IsHelperOrAdmin() {
				return app.ErrNotAdmin
			}
			return lock.Set(old, pass)
		},
		Lock: func(ctx context.Context) ([]string, error) {
			lock.Relock()
			return []string{"locked"}, nil
		},
		Sessions: func(ctx context.Context) ([]string, error) {
			live := m.Live()
			if len(live) == 0 {
				return []string{"no live sessions"}, nil
			}
			var out []string
			for _, s := range live {
				role := "visitor"
				if s.Host {
					role = "host"
				}
				out = append(out, fmt.Sprintf("%s  %s  %s  %s  %s  %s", s.ID[:6], s.Kind, s.Peer.Username, role, s.Via, time.Since(s.Started).Truncate(time.Second)))
			}
			return out, nil
		},
		RevokeAll: func(ctx context.Context) ([]string, error) {
			n := m.StopAll("revoke-all")
			held, _ := a.GrantsHeld(ctx)
			dropped := 0
			for _, g := range held {
				if err := a.Backend().RevokeGrant(ctx, g.ID); err == nil {
					dropped++
				}
			}
			lock.Relock()
			return []string{fmt.Sprintf("ended %d session(s), dropped %d grant(s), relocked", n, dropped)}, nil
		},
	}
	return h, ready
}

func friendly(err error) error {
	switch {
	case errors.Is(err, admin.ErrLocked):
		return errors.New("locked: /admin unlock first")
	case errors.Is(err, session.ErrDenied):
		return errors.New("they denied the request")
	case errors.Is(err, session.ErrTimeout):
		return errors.New("no answer in time")
	}
	return err
}

// logsCommand implements /logs [list|show <id>|files <id>|replay <id>|verify <id>|delete <id>|keep <days>].
func logsCommand(ctx context.Context, m *session.Manager, args []string) ([]string, error) {
	entries, err := m.Logs()
	if err != nil {
		return nil, err
	}
	find := func(prefix string) (sessionlog.Entry, error) {
		for _, e := range entries {
			if strings.HasPrefix(e.Meta.ID, prefix) {
				return e, nil
			}
		}
		return sessionlog.Entry{}, errors.New("no session with that id")
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		if len(entries) == 0 {
			return []string{"no sessions recorded on this machine yet"}, nil
		}
		out := []string{"sessions on this machine (newest first) · /logs show|files|replay|verify|delete <id>"}
		for i, e := range entries {
			if i >= 30 {
				break
			}
			mt := e.Meta
			dur := "-"
			if mt.EndedAt != nil {
				dur = mt.EndedAt.Sub(mt.StartedAt).Truncate(time.Second).String()
			}
			who := mt.VisitorName
			if mt.Role == "visitor" {
				who = "→ " + mt.HostName
			}
			chain := "✔"
			if r, err := sessionlog.Verify(e.Dir, nil); err != nil || !r.ChainOK || !r.SignatureOK {
				chain = "✘"
			}
			out = append(out, fmt.Sprintf("%s  %s  %-10s %-6s %-6s %-5s %d cmds · %d files · chain %s", mt.ID[:6], mt.StartedAt.Local().Format("Jan 02 15:04"), who, mt.Kind, dur, mt.Via, mt.Commands, mt.FilesTouched, chain))
		}
		return out, nil
	case "show":
		if len(args) < 2 {
			return nil, errors.New("/logs show <id>")
		}
		e, err := find(args[1])
		if err != nil {
			return nil, err
		}
		cmds, err := sessionlog.ReadCommands(e.Dir)
		if err != nil {
			return nil, err
		}
		out := []string{fmt.Sprintf("%s · %s · %s · %d commands", e.Meta.ID[:6], e.Meta.Kind, e.Meta.VisitorName, len(cmds))}
		for _, c := range cmds {
			flag := ""
			if c.Sudo {
				flag = " [sudo]"
			}
			out = append(out, fmt.Sprintf("%s  %s$ %s%s  (exit %d)", c.Time.Local().Format("15:04:05"), shortDir(c.Cwd), c.Cmd, flag, c.Exit))
			if o, err := sessionlog.CommandOutput(e.Dir, c); err == nil && strings.TrimSpace(o) != "" {
				lines := strings.Split(strings.ReplaceAll(strings.TrimRight(o, "\r\n"), "\r\n", "\n"), "\n")
				if len(lines) > 8 {
					lines = append(lines[:8], fmt.Sprintf("… %d more lines, /logs replay %s", len(lines)-8, e.Meta.ID[:6]))
				}
				for _, l := range lines {
					out = append(out, "    "+l)
				}
			}
		}
		return out, nil
	case "files":
		if len(args) < 2 {
			return nil, errors.New("/logs files <id>")
		}
		e, err := find(args[1])
		if err != nil {
			return nil, err
		}
		fs, err := sessionlog.ReadFiles(e.Dir)
		if err != nil {
			return nil, err
		}
		if len(fs) == 0 {
			return []string{"no file activity recorded"}, nil
		}
		var out []string
		for _, f := range fs {
			line := fmt.Sprintf("%s  %-7s %s", f.Time.Local().Format("15:04:05"), f.Event, f.Path)
			if f.ByCmd > 0 {
				line += fmt.Sprintf("  (cmd #%d)", f.ByCmd)
			}
			if f.Diff != "" {
				line += "  diff: " + filepath.Join(e.Dir, f.Diff)
			}
			out = append(out, line)
		}
		return out, nil
	case "replay":
		if len(args) < 2 {
			return nil, errors.New("/logs replay <id>")
		}
		e, err := find(args[1])
		if err != nil {
			return nil, err
		}
		if err := m.Replay(ctx, e.Dir, 1); err != nil {
			return nil, err
		}
		return []string{"replay finished"}, nil
	case "verify":
		if len(args) < 2 {
			return nil, errors.New("/logs verify <id>")
		}
		e, err := find(args[1])
		if err != nil {
			return nil, err
		}
		r, err := m.Verify(e.Dir)
		if err != nil {
			return nil, err
		}
		out := []string{fmt.Sprintf("chain %v · transcript %v · signature %v (signer %s)", r.ChainOK, r.TranscriptOK, r.SignatureOK, short(r.Signer))}
		out = append(out, r.Problems...)
		return out, nil
	case "delete":
		if len(args) < 2 {
			return nil, errors.New("/logs delete <id>")
		}
		e, err := find(args[1])
		if err != nil {
			return nil, err
		}
		if err := m.DeleteLog(e.Dir); err != nil {
			return nil, err
		}
		return []string{"deleted " + e.Meta.ID[:6] + " (noted in deletions.log)"}, nil
	case "keep":
		if len(args) < 2 {
			return nil, errors.New("/logs keep <days|forever>")
		}
		days := 0
		if args[1] != "forever" {
			n, err := strconv.Atoi(strings.TrimSuffix(args[1], "d"))
			if err != nil || n < 0 {
				return nil, errors.New("days must be a number or 'forever'")
			}
			days = n
		}
		p := m.Store().Prefs()
		p.LogKeepDays = days
		if err := m.Store().SavePrefs(p); err != nil {
			return nil, err
		}
		m.PruneLogs()
		if days == 0 {
			return []string{"keeping session logs forever"}, nil
		}
		return []string{fmt.Sprintf("keeping session logs for %d days", days)}, nil
	}
	return nil, errors.New("usage: /logs [list|show|files|replay|verify|delete <id>|keep <days>]")
}

func shortDir(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
