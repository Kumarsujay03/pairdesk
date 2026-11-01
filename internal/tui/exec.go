package tui

import (
	"github.com/Kumarsujay03/pairdesk/internal/update"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/tui/theme"
)

// say prints lines into the active conversation right away.
func (m *ui) say(lines ...string) tea.Cmd {
	if len(lines) > 0 {
		m.addNote("", strings.Join(lines, "\n"), false)
	}
	m.renderChat(true)
	return nil
}

func (m *ui) warn(text string) tea.Cmd {
	m.addNote("", text, true)
	m.renderChat(true)
	return nil
}

func (m *ui) usage(c Command) tea.Cmd { return m.warn(Usage(c.Name)) }

// ok wraps a no-output app call into a one-line confirmation.
func ok(line string, fn func(ctx context.Context) error) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		if err := fn(ctx); err != nil {
			return nil, err
		}
		return []string{line}, nil
	}
}

// exec runs a parsed command.
func (m *ui) exec(c Command) tea.Cmd {
	a := m.app
	if c.Admin && !a.IsAdmin() && !(a.IsHelperOrAdmin() && helperCommands[c.Name]) {
		if (c.Name == "board" || c.Name == "watch" || c.Name == "screen" || c.Name == "share") && a.IsHelperOrAdmin() {
			// helpers may use the remote-access commands on their friends
		} else {
			return m.warn(app.ErrNotAdmin.Error())
		}
	}
	switch c.Name {
	case "version":
		return m.say("pairdesk " + m.version)
	case "update":
		return m.run("", func(ctx context.Context) ([]string, error) {
			tag, err := update.Apply(ctx, m.version, nil)
			if err != nil {
				if err.Error() == "already up to date" {
					return []string{"already up to date (" + tag + ")"}, nil
				}
				return nil, err
			}
			return []string{"installed " + tag + ". quit and start pairdesk again to use it."}, nil
		})
	case "help":
		return m.say(helpFor(a.IsAdmin(), a.IsHelperOrAdmin())...)
	case "quit":
		return m.quit()
	case "add":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		return m.run("", ok("friend request sent to "+name, func(ctx context.Context) error { return a.AddFriend(ctx, name) }))
	case "requests":
		m.requests = true
		m.layout()
		return nil
	case "remove":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		return m.run("", ok("removed "+name, func(ctx context.Context) error { return a.RemoveFriend(ctx, name) }))
	case "dm":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		return m.openDM(c.Arg(0))
	case "friends":
		m.everyone = false
		m.pane = paneLeft
		m.input.Blur()
		m.cursor = 0
		fs := a.Friends()
		lines := []string{fmt.Sprintf("%d friends", len(fs))}
		for _, f := range fs {
			d := "○"
			if f.Online {
				d = "●"
			}
			lines = append(lines, "  "+d+" "+f.User.Username)
		}
		return m.say(lines...)
	case "users":
		m.everyone = !m.everyone
		m.cursor = 0
		if !m.everyone {
			return m.say("friends pane")
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()
			us, err := a.AllUsers(ctx)
			return usersMsg{users: us, err: err}
		}
	case "me":
		if c.Rest == "" {
			return m.usage(c)
		}
		conv := a.Conv(m.active)
		if conv == nil {
			return m.warn("no conversation open")
		}
		text := c.Rest
		return m.run("", func(ctx context.Context) ([]string, error) { return nil, a.SendMe(ctx, conv, text) })
	case "react":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		conv := a.Conv(m.active)
		if conv == nil {
			return m.warn("no conversation open")
		}
		emoji := c.Arg(0)
		return m.run("", func(ctx context.Context) ([]string, error) { return nil, a.React(ctx, conv, emoji) })
	case "verify":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()
			theirs, err := a.Verify(ctx, name)
			return verifyMsg{name: name, theirs: theirs, mine: a.MyPhrase(), err: err}
		}
	case "theme":
		name := c.Arg(0)
		if name == "" {
			return m.say("themes: " + strings.Join(theme.Names(), ", ") + " (current: " + m.th.Name + ")")
		}
		dir := a.Store().Paths.Config
		return func() tea.Msg {
			th, err := theme.Load(name, dir)
			return themeMsg{th: th, err: err}
		}
	case "badge":
		on, err := onOff(c.Arg(0))
		if err != nil {
			return m.usage(c)
		}
		if err := a.SetBadge(on); err != nil {
			return m.warn(err.Error())
		}
		return m.say("admin badge " + c.Arg(0))
	case "grant":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		scope := model.ScopeBoard
		if _, ok := c.Flag("watch-only"); ok {
			scope = model.ScopeWatch
		}
		sudo := model.SudoBlocked
		if v, ok := c.Flag("sudo"); ok {
			switch v {
			case "ask":
				sudo = model.SudoAsk
			case "auto":
				sudo = model.SudoAuto
			default:
				return m.warn("--sudo must be ask or auto")
			}
		}
		if sudo == model.SudoAuto {
			m.sudoAsk = &sudoPrompt{name: name, scope: scope}
			m.input.Blur()
			m.masked.Focus()
			m.layout()
			return nil
		}
		return m.run("", ok(fmt.Sprintf("granted %s to %s (sudo %s)", scope, name, sudo), func(ctx context.Context) error {
			return a.Grant(ctx, name, scope, sudo, "")
		}))
	case "revoke":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		return m.run("", ok("revoked grant to "+name, func(ctx context.Context) error { return a.Revoke(ctx, name, false) }))
	case "grants":
		return m.run("", func(ctx context.Context) ([]string, error) {
			var lines []string
			given := a.GrantsGiven()
			lines = append(lines, fmt.Sprintf("grants you gave (%d):", len(given)))
			for _, g := range given {
				lines = append(lines, fmt.Sprintf("  %s  %s  sudo %s  since %s", a.Name(g.Grantee), g.Scope, g.Sudo, g.CreatedAt.Format("2006-01-02")))
			}
			held, err := a.GrantsHeld(ctx)
			if err != nil {
				return lines, err
			}
			lines = append(lines, fmt.Sprintf("grants you hold (%d):", len(held)))
			for _, g := range held {
				lines = append(lines, fmt.Sprintf("  %s  %s  sudo %s", a.Name(g.Grantor), g.Scope, g.Sudo))
			}
			return lines, nil
		})
	case "group":
		return m.execGroup(c)
	case "ban":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		reason := c.Tail(1)
		line := "banned " + name
		if reason != "" {
			line += " (" + reason + ")"
		}
		return m.run("", ok(line, func(ctx context.Context) error { return a.SetUserFlag(ctx, name, "banned", true) }))
	case "unban":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		name := c.Arg(0)
		return m.run("", ok("unbanned "+name, func(ctx context.Context) error { return a.SetUserFlag(ctx, name, "banned", false) }))
	case "admin":
		return m.execAdmin(c)
	case "get":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		id, conv := c.Arg(0), m.active
		return m.hook("get", func(ctx context.Context) ([]string, error) { return m.hooks.Fetch(ctx, conv, id) }, m.hooks.Fetch != nil)

	// --- hooks ---
	case "board":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		user, args := c.Arg(0), c.Args[1:]
		return m.hook("board", func(ctx context.Context) ([]string, error) { return m.hooks.Board(ctx, user, args) }, m.hooks.Board != nil)
	case "watch":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		user, args := c.Arg(0), c.Args[1:]
		return m.hook("watch", func(ctx context.Context) ([]string, error) { return m.hooks.Watch(ctx, user, args) }, m.hooks.Watch != nil)
	case "screen":
		if c.Arg(0) == "" {
			return m.usage(c)
		}
		user := c.Arg(0)
		_, control := c.Flag("control")
		return m.hook("screen", func(ctx context.Context) ([]string, error) { return m.hooks.Screen(ctx, user, control) }, m.hooks.Screen != nil)
	case "send":
		if c.Arg(1) == "" {
			return m.usage(c)
		}
		user, path := c.Arg(0), c.Arg(1)
		return m.hook("send", func(ctx context.Context) ([]string, error) { return m.hooks.Send(ctx, user, path) }, m.hooks.Send != nil)
	case "share":
		return m.hook("share", func(ctx context.Context) ([]string, error) { return m.hooks.Share(ctx) }, m.hooks.Share != nil)
	case "logs":
		args := c.Args
		return m.hook("logs", func(ctx context.Context) ([]string, error) { return m.hooks.Logs(ctx, args) }, m.hooks.Logs != nil)
	case "stop":
		return m.hook("stop", func(ctx context.Context) ([]string, error) { return m.hooks.Stop(ctx) }, m.hooks.Stop != nil)
	case "end":
		return m.hook("end", func(ctx context.Context) ([]string, error) { return m.hooks.End(ctx) }, m.hooks.End != nil)
	case "search":
		if c.Rest == "" {
			return m.usage(c)
		}
		q := c.Rest
		return m.hook("search", func(ctx context.Context) ([]string, error) { return m.hooks.Search(ctx, q) }, m.hooks.Search != nil)
	}
	return m.warn("unknown command /" + c.Name)
}

// helperCommands may also be run by helpers, not only the admin.
var helperCommands = map[string]bool{"board": true, "watch": true, "screen": true, "share": true}

type verifyMsg struct {
	name, mine, theirs string
	err                error
}

func (m *ui) currentGroup() (model.Group, error) {
	c := m.app.Conv(m.active)
	if c == nil || c.Type != model.ConvGroup {
		return model.Group{}, errors.New("open a group first (pick one on the left)")
	}
	g, ok := m.groupByID(c.ID)
	if !ok {
		return model.Group{}, errors.New("that group is gone")
	}
	return g, nil
}

func (m *ui) execGroup(c Command) tea.Cmd {
	a := m.app
	switch c.Sub {
	case "create":
		if c.Rest == "" {
			return m.warn("usage: /group create <name>")
		}
		name := c.Rest
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			defer cancel()
			conv, err := a.CreateGroup(ctx, name)
			return convOpenedMsg{conv: conv, err: err}
		}
	case "members":
		g, err := m.currentGroup()
		if err != nil {
			return m.warn(err.Error())
		}
		return m.run("", func(ctx context.Context) ([]string, error) {
			ms, err := a.GroupMembers(ctx, g)
			if err != nil {
				return nil, err
			}
			lines := []string{fmt.Sprintf("#%s members (%d):", g.Name, len(ms))}
			for _, f := range ms {
				d := "○"
				if f.Online {
					d = "●"
				}
				tag := ""
				if f.User.ID == g.Owner {
					tag = "  owner"
				}
				lines = append(lines, "  "+d+" "+f.User.Username+tag)
			}
			return lines, nil
		})
	case "leave":
		g, err := m.currentGroup()
		if err != nil {
			return m.warn(err.Error())
		}
		return m.run("", ok("left #"+g.Name, func(ctx context.Context) error { return a.LeaveGroup(ctx, g) }))
	case "delete":
		g, err := m.currentGroup()
		if err != nil {
			return m.warn(err.Error())
		}
		return m.run("", ok("deleted #"+g.Name, func(ctx context.Context) error { return a.DeleteGroup(ctx, g) }))
	case "invite", "remove", "transfer":
		if c.Arg(0) == "" {
			return m.warn("usage: /group " + c.Sub + " <user>")
		}
		g, err := m.currentGroup()
		if err != nil {
			return m.warn(err.Error())
		}
		name := c.Arg(0)
		switch c.Sub {
		case "invite":
			return m.run("", ok("invited "+name+" to #"+g.Name, func(ctx context.Context) error { return a.InviteToGroup(ctx, g, name) }))
		case "remove":
			return m.run("", ok("removed "+name+" from #"+g.Name, func(ctx context.Context) error { return a.RemoveFromGroup(ctx, g, name) }))
		default:
			return m.run("", ok("#"+g.Name+" now belongs to "+name, func(ctx context.Context) error { return a.TransferGroup(ctx, g, name) }))
		}
	}
	return m.usage(c)
}

func onOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "yes":
		return true, nil
	case "off", "false", "no":
		return false, nil
	}
	return false, errors.New("expected on or off")
}

func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func (m *ui) execAdmin(c Command) tea.Cmd {
	a := m.app
	switch c.Sub {
	case "reg":
		mode := model.RegistrationMode(strings.ToLower(c.Arg(0)))
		if mode == "" {
			return m.warn("usage: /admin reg open|closed|invite")
		}
		return m.run("", ok("registration: "+string(mode), func(ctx context.Context) error { return a.SetRegistration(ctx, mode) }))
	case "approve":
		on, err := onOff(c.Arg(0))
		if err != nil {
			return m.warn("usage: /admin approve on|off")
		}
		return m.run("", ok("approval queue "+c.Arg(0), func(ctx context.Context) error { return a.SetApproval(ctx, on) }))
	case "cap":
		n, err := strconv.Atoi(c.Arg(0))
		if err != nil || n < 0 {
			return m.warn("usage: /admin cap <n> (0 = no cap)")
		}
		return m.run("", ok(fmt.Sprintf("user cap: %d", n), func(ctx context.Context) error { return a.SetUserCap(ctx, n) }))
	case "invite":
		switch c.Arg(0) {
		case "new":
			uses := 1
			if v, ok := c.Flag("uses"); ok {
				n, err := strconv.Atoi(v)
				if err != nil || n <= 0 {
					return m.warn("--uses needs a positive number")
				}
				uses = n
			}
			var ttl time.Duration
			if v, ok := c.Flag("expires"); ok {
				d, err := parseTTL(v)
				if err != nil {
					return m.warn("--expires looks like 7d or 12h")
				}
				ttl = d
			}
			return m.run("", func(ctx context.Context) ([]string, error) {
				inv, err := a.NewInvite(ctx, uses, ttl)
				if err != nil {
					return nil, err
				}
				line := fmt.Sprintf("invite code: %s  (%d uses", inv.Code, inv.UsesLeft)
				if inv.ExpiresAt != nil {
					line += ", expires " + inv.ExpiresAt.Local().Format("2006-01-02 15:04")
				}
				return []string{line + ")"}, nil
			})
		case "list":
			return m.run("", func(ctx context.Context) ([]string, error) {
				ivs, err := a.Invites(ctx)
				if err != nil {
					return nil, err
				}
				lines := []string{fmt.Sprintf("%d invite codes", len(ivs))}
				for _, iv := range ivs {
					exp := "never expires"
					if iv.ExpiresAt != nil {
						exp = "expires " + iv.ExpiresAt.Local().Format("2006-01-02 15:04")
					}
					lines = append(lines, fmt.Sprintf("  %s  %d uses left  %s", iv.Code, iv.UsesLeft, exp))
				}
				return lines, nil
			})
		case "revoke":
			code := c.Arg(1)
			if code == "" {
				return m.warn("usage: /admin invite revoke <code>")
			}
			return m.run("", ok("revoked invite "+code, func(ctx context.Context) error { return a.RevokeInvite(ctx, code) }))
		}
		return m.warn("usage: /admin invite new [--uses n] [--expires 7d] | list | revoke <code>")
	case "kick", "mute", "unmute", "approve-user", "reset-key":
		name := c.Arg(0)
		if name == "" {
			return m.warn("usage: /admin " + c.Sub + " <user>")
		}
		switch c.Sub {
		case "kick":
			return m.run("", ok("kicked "+name, func(ctx context.Context) error { return a.Kick(ctx, name) }))
		case "mute":
			return m.run("", ok("muted "+name, func(ctx context.Context) error { return a.SetUserFlag(ctx, name, "muted", true) }))
		case "unmute":
			return m.run("", ok("unmuted "+name, func(ctx context.Context) error { return a.SetUserFlag(ctx, name, "muted", false) }))
		case "approve-user":
			return m.run("", ok("approved "+name, func(ctx context.Context) error { return a.SetUserFlag(ctx, name, "approved", true) }))
		default:
			return m.run("", ok("reset device key of "+name+"; their friends will see a key-change warning", func(ctx context.Context) error { return a.ResetKey(ctx, name) }))
		}
	case "helper":
		name := c.Arg(1)
		if (c.Arg(0) != "add" && c.Arg(0) != "remove") || name == "" {
			return m.warn("usage: /admin helper add|remove <user>")
		}
		on := c.Arg(0) == "add"
		line := name + " is now a helper"
		if !on {
			line = name + " is no longer a helper"
		}
		return m.run("", ok(line, func(ctx context.Context) error { return a.SetHelper(ctx, name, on) }))
	case "rename":
		if c.Arg(1) == "" {
			return m.warn("usage: /admin rename <old> <new>")
		}
		oldN, newN := c.Arg(0), c.Arg(1)
		return m.run("", ok("renamed "+oldN+" to "+newN, func(ctx context.Context) error { return a.Rename(ctx, oldN, newN) }))
	case "unlock":
		if m.hooks.Unlock == nil {
			return m.warn("not available in this build")
		}
		m.askMasked("admin passphrase", func(pw string) tea.Cmd {
			return m.run("", func(ctx context.Context) ([]string, error) {
				if err := m.hooks.Unlock(ctx, pw); err != nil {
					return nil, err
				}
				return []string{"admin commands unlocked for 15 minutes"}, nil
			})
		}, nil)
		return nil
	case "lock":
		return m.hook("lock", func(ctx context.Context) ([]string, error) { return m.hooks.Lock(ctx) }, m.hooks.Lock != nil)
	case "pass":
		if m.hooks.Passphrase == nil {
			return m.warn("not available in this build")
		}
		m.askMasked("current admin passphrase (leave empty if none yet)", func(old string) tea.Cmd {
			m.askMasked("new admin passphrase (8+ chars)", func(pw string) tea.Cmd {
				return m.run("", func(ctx context.Context) ([]string, error) {
					if err := m.hooks.Passphrase(ctx, old, pw); err != nil {
						return nil, err
					}
					return []string{"admin passphrase set; admin and remote commands now ask for it every 15 minutes"}, nil
				})
			}, nil)
			return nil
		}, nil)
		return nil
	case "sessions":
		return m.hook("sessions", func(ctx context.Context) ([]string, error) { return m.hooks.Sessions(ctx) }, m.hooks.Sessions != nil)
	case "revoke-all":
		return m.hook("revoke-all", func(ctx context.Context) ([]string, error) { return m.hooks.RevokeAll(ctx) }, m.hooks.RevokeAll != nil)
	case "announce":
		text := c.Rest
		line := "announcement set"
		if text == "" {
			line = "announcement cleared"
		}
		return m.run("", ok(line, func(ctx context.Context) error { return a.Announce(ctx, text) }))
	case "maintenance":
		on, err := onOff(c.Arg(0))
		if err != nil {
			return m.warn("usage: /admin maintenance on|off [msg]")
		}
		msg := c.Tail(1)
		return m.run("", ok("maintenance "+c.Arg(0), func(ctx context.Context) error { return a.Maintenance(ctx, on, msg) }))
	case "min-version":
		if c.Arg(0) == "" {
			return m.warn("usage: /admin min-version <v>")
		}
		v := c.Arg(0)
		return m.run("", ok("minimum client version: "+v, func(ctx context.Context) error { return a.SetMinVersion(ctx, v) }))
	case "limits":
		if c.Arg(0) != "msgs" {
			return m.warn("usage: /admin limits msgs <n>")
		}
		n, err := strconv.Atoi(strings.TrimSuffix(c.Arg(1), "/min"))
		if err != nil || n < 0 {
			return m.warn("usage: /admin limits msgs <n>")
		}
		return m.run("", ok(fmt.Sprintf("rate limit: %d messages/min", n), func(ctx context.Context) error { return a.SetRateLimit(ctx, n) }))
	case "stats":
		return m.run("", func(ctx context.Context) ([]string, error) {
			s, err := a.Stats(ctx)
			if err != nil {
				return nil, err
			}
			return []string{
				fmt.Sprintf("users: %d total, %d online, %d today", s.UsersTotal, s.UsersOnline, s.UsersToday),
				fmt.Sprintf("messages today: %d · sessions this week: %d", s.MsgsToday, s.SessionsWeek),
				fmt.Sprintf("database: %.1f MB of 500 MB", float64(s.DBBytes)/1e6),
			}, nil
		})
	case "log":
		n := 20
		if c.Arg(0) != "" {
			if v, err := strconv.Atoi(c.Arg(0)); err == nil && v > 0 {
				n = v
			}
		}
		if v, ok := c.Flag("tail"); ok {
			if t, err := strconv.Atoi(v); err == nil && t > 0 {
				n = t
			}
		}
		return m.run("", func(ctx context.Context) ([]string, error) {
			es, err := a.AdminLog(ctx, n)
			if err != nil {
				return nil, err
			}
			lines := []string{fmt.Sprintf("last %d admin actions:", len(es))}
			for _, e := range es {
				l := fmt.Sprintf("  %s  %s %s", e.At.Local().Format("01-02 15:04"), e.Action, e.Target)
				if e.Detail != "" {
					l += "  " + e.Detail
				}
				lines = append(lines, l)
			}
			return lines, nil
		})
	case "devices":
		if c.Arg(0) == "approve" {
			id := c.Arg(1)
			if id == "" {
				return m.warn("usage: /admin devices approve <id>")
			}
			return m.run("", func(ctx context.Context) ([]string, error) {
				tok, err := a.ApproveDevice(ctx, id)
				if err != nil {
					return nil, err
				}
				if tok != "" {
					return []string{"approved device " + id + "; its token: " + tok}, nil
				}
				return []string{"approved device " + id}, nil
			})
		}
		return m.run("", func(ctx context.Context) ([]string, error) {
			ds, err := a.AdminDevices(ctx)
			if err != nil {
				return nil, err
			}
			lines := []string{fmt.Sprintf("%d admin devices:", len(ds))}
			for _, d := range ds {
				st := "pending  (/admin devices approve " + d.DeviceID + ")"
				if d.Approved {
					st = "approved " + d.ApprovedAt.Local().Format("2006-01-02")
				}
				lines = append(lines, "  "+d.DeviceID+"  "+st)
			}
			return lines, nil
		})
	case "groups":
		return m.run("", func(ctx context.Context) ([]string, error) {
			gs, err := a.AllGroups(ctx)
			if err != nil {
				return nil, err
			}
			lines := []string{fmt.Sprintf("%d groups:", len(gs))}
			for _, g := range gs {
				lines = append(lines, fmt.Sprintf("  #%s  owner %s  epoch %d", g.Name, a.Name(g.Owner), g.KeyEpoch))
			}
			return lines, nil
		})
	case "badge":
		on, err := onOff(c.Arg(0))
		if err != nil {
			return m.warn("usage: /admin badge on|off")
		}
		if err := a.SetBadge(on); err != nil {
			return m.warn(err.Error())
		}
		return m.say("admin badge " + c.Arg(0))
	case "inspect":
		name := c.Arg(0)
		if name == "" {
			return m.warn("usage: /admin inspect <user>")
		}
		return m.run("", func(ctx context.Context) ([]string, error) {
			u, err := a.UserByName(ctx, name)
			if err != nil {
				return nil, err
			}
			return []string{
				fmt.Sprintf("%s  role %s  joined %s  last seen %s", u.Username, u.Role, u.CreatedAt.Local().Format("2006-01-02"), u.LastSeen.Local().Format("2006-01-02 15:04")),
				fmt.Sprintf("  approved %v  banned %v  muted %v  key %s", u.Approved, u.Banned, u.Muted, short(u.PubKey)),
			}, nil
		})
	}
	return m.warn("unknown /admin subcommand; /help lists them")
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	if s == "" {
		return "(none)"
	}
	return s
}
