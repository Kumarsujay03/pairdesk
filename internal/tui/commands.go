package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Command is a parsed slash command.
type Command struct {
	Name  string            // "add", "group", "admin", ...
	Sub   string            // for group/admin: "create", "reg", ...
	Args  []string          // positional tokens after name (and sub), flags removed
	Flags map[string]string // --key value; boolean flags map to "true"
	Rest  string            // raw text after name (and sub), flags included
	Admin bool              // true if the command is admin-only
}

// Arg returns positional argument i or "".
func (c Command) Arg(i int) string {
	if i < len(c.Args) {
		return c.Args[i]
	}
	return ""
}

// Flag reports whether a flag was given and its value.
func (c Command) Flag(name string) (string, bool) {
	v, ok := c.Flags[name]
	return v, ok
}

// Tail returns Rest with the first n positional args stripped from the
// front, used for free-text trailing arguments like a ban reason.
func (c Command) Tail(n int) string {
	rest := c.Rest
	for i := 0; i < n && i < len(c.Args); i++ {
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), c.Args[i]))
	}
	return strings.TrimSpace(rest)
}

type cmdSpec struct {
	admin bool
	subs  []string // nil = no subcommand
	usage string
}

// boolFlags are flags that take no value.
var boolFlags = map[string]bool{"watch-only": true, "control": true}

var commands = map[string]cmdSpec{
	"help":     {usage: "/help"},
	"add":      {usage: "/add <user>"},
	"requests": {usage: "/requests"},
	"remove":   {usage: "/remove <user>"},
	"dm":       {usage: "/dm <user>"},
	"group":    {subs: []string{"create", "invite", "remove", "leave", "delete", "transfer", "members"}, usage: "/group create|invite|remove|leave|delete|transfer|members"},
	"friends":  {usage: "/friends"},
	"users":    {admin: true, usage: "/users"},
	"me":       {usage: "/me <text>"},
	"react":    {usage: "/react <emoji>"},
	"verify":   {usage: "/verify <user>"},
	"theme":    {usage: "/theme <name>"},
	"badge":    {admin: true, usage: "/badge on|off"},
	"grant":    {usage: "/grant <user> [--watch-only] [--sudo ask|auto]"},
	"revoke":   {usage: "/revoke <user>"},
	"grants":   {usage: "/grants"},
	"quit":     {usage: "/quit"},
	"end":      {usage: "/end"},
	"stop":     {usage: "/stop"},
	"board":    {admin: true, usage: "/board <user>"},
	"watch":    {admin: true, usage: "/watch <user>"},
	"screen":   {admin: true, usage: "/screen <user> [--control]"},
	"share":    {admin: true, usage: "/share"},
	"send":     {usage: "/send <user> <path>"},
	"logs":     {usage: "/logs [replay <id>]"},
	"search":   {usage: "/search <text>"},
	"ban":      {admin: true, usage: "/ban <user> [reason]"},
	"unban":    {admin: true, usage: "/unban <user>"},
	"admin": {admin: true, subs: []string{
		"reg", "approve", "cap", "invite", "kick", "mute", "unmute", "approve-user",
		"helper", "reset-key", "rename", "announce", "maintenance", "min-version",
		"limits", "stats", "log", "devices", "groups", "badge", "inspect",
		"unlock", "lock", "pass", "sessions", "revoke-all",
	}, usage: "/admin <subcommand>"},
	"get": {usage: "/get <file-id>"},
}

// ErrNotCommand is returned by Parse for lines that do not start with "/".
var ErrNotCommand = errors.New("not a command")

// ErrAdminOnly is returned by Parse (via the helper below) when a non-admin
// uses an admin command.
var ErrAdminOnly = errors.New("only the admin can do that")

// Parse splits a slash-command line. It validates the command name and the
// subcommand for /group and /admin but not argument counts; the executor
// does that so it can print usage.
func Parse(line string) (Command, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return Command{}, ErrNotCommand
	}
	body := strings.TrimPrefix(line, "/")
	if body == "" {
		return Command{}, errors.New("empty command; try /help")
	}
	fields := strings.Fields(body)
	name := strings.ToLower(fields[0])
	spec, ok := commands[name]
	if !ok {
		return Command{}, fmt.Errorf("unknown command /%s; try /help", name)
	}
	c := Command{Name: name, Flags: map[string]string{}, Admin: spec.admin}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), fields[0]))
	toks := fields[1:]
	if spec.subs != nil {
		if len(toks) == 0 {
			return Command{}, fmt.Errorf("usage: %s", spec.usage)
		}
		sub := strings.ToLower(toks[0])
		found := false
		for _, s := range spec.subs {
			if s == sub {
				found = true
				break
			}
		}
		if !found {
			return Command{}, fmt.Errorf("unknown /%s subcommand %q; %s", name, sub, spec.usage)
		}
		c.Sub = sub
		rest = strings.TrimSpace(strings.TrimPrefix(rest, toks[0]))
		toks = toks[1:]
	}
	c.Rest = rest
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, "--") && len(t) > 2 {
			key := strings.TrimPrefix(t, "--")
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				c.Flags[key[:eq]] = key[eq+1:]
				continue
			}
			if boolFlags[key] {
				c.Flags[key] = "true"
				continue
			}
			if i+1 < len(toks) && !strings.HasPrefix(toks[i+1], "--") {
				c.Flags[key] = toks[i+1]
				i++
			} else {
				c.Flags[key] = "true"
			}
			continue
		}
		c.Args = append(c.Args, t)
	}
	return c, nil
}

// Usage returns the usage line of a command.
func Usage(name string) string {
	if s, ok := commands[name]; ok {
		return "usage: " + s.usage
	}
	return ""
}

// helpText lists commands for /help. Admin commands are shown only to the
// admin.
func helpText(admin bool) []string {
	names := make([]string, 0, len(commands))
	for n, s := range commands {
		if s.admin && !admin {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	out := []string{"commands:"}
	for _, n := range names {
		out = append(out, "  "+commands[n].usage)
	}
	if admin {
		out = append(out,
			"  /admin reg open|closed|invite · approve on|off · cap <n>",
			"  /admin invite new [--uses n] [--expires 7d] · invite list · invite revoke <code>",
			"  /admin kick|mute|unmute|approve-user|reset-key <user> · rename <old> <new>",
			"  /admin helper add|remove <user> · announce <text> · maintenance on|off [msg]",
			"  /admin min-version <v> · limits msgs <n> · stats · log [n] · devices [approve <id>] · groups",
		)
	}
	out = append(out, "keys: tab panes · enter send · esc clear · pgup/pgdn scroll · ctrl+k stop session · ctrl+c quit")
	return out
}
