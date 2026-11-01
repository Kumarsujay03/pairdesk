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
	desc  string
}

// boolFlags are flags that take no value.
var boolFlags = map[string]bool{"watch-only": true, "control": true}

var commands = map[string]cmdSpec{
	"help":     {usage: "/help", desc: "list commands"},
	"add":      {usage: "/add <user>", desc: "send a friend request"},
	"requests": {usage: "/requests", desc: "pending friend requests"},
	"remove":   {usage: "/remove <user>", desc: "remove a friend"},
	"dm":       {usage: "/dm <user>", desc: "open a direct chat"},
	"group":    {subs: []string{"create", "invite", "remove", "leave", "delete", "transfer", "members"}, usage: "/group create|invite|remove|leave|delete|transfer|members", desc: "create and manage groups"},
	"friends":  {usage: "/friends", desc: "list friends"},
	"users":    {admin: true, usage: "/users", desc: "everyone on this instance"},
	"me":       {usage: "/me <text>", desc: "say something in third person"},
	"react":    {usage: "/react <emoji>", desc: "react to the last message"},
	"verify":   {usage: "/verify <user>", desc: "compare key fingerprints with a friend"},
	"theme":    {usage: "/theme <name>", desc: "switch colour theme"},
	"badge":    {admin: true, usage: "/badge on|off", desc: "show or hide your admin badge"},
	"grant":    {usage: "/grant <user> [--watch-only] [--sudo ask|auto]", desc: "let the admin open your terminal while you are online"},
	"revoke":   {usage: "/revoke <user>", desc: "withdraw a standing grant"},
	"grants":   {usage: "/grants", desc: "grants you gave and hold"},
	"quit":     {usage: "/quit", desc: "exit pairdesk"},
	"end":      {usage: "/end", desc: "end the current session"},
	"stop":     {usage: "/stop", desc: "kill switch: end any session on this machine"},
	"board":    {admin: true, usage: "/board <user>", desc: "open a friend's terminal (needs consent or a grant)"},
	"watch":    {admin: true, usage: "/watch <user>", desc: "view a friend's terminal read-only"},
	"screen":   {admin: true, usage: "/screen <user> [--control]", desc: "view a friend's screen (always asks)"},
	"share":    {admin: true, usage: "/share", desc: "share your terminal into a session"},
	"send":     {usage: "/send <user> <path>", desc: "offer a file to a friend"},
	"logs":     {usage: "/logs [replay <id>]", desc: "browse, replay and verify session logs"},
	"search":   {usage: "/search <text>", desc: "search your message history"},
	"ban":      {admin: true, usage: "/ban <user> [reason]", desc: "ban a user"},
	"unban":    {admin: true, usage: "/unban <user>", desc: "lift a ban"},
	"admin": {admin: true, subs: []string{
		"reg", "approve", "cap", "invite", "kick", "mute", "unmute", "approve-user",
		"helper", "reset-key", "rename", "announce", "maintenance", "min-version",
		"limits", "stats", "log", "devices", "groups", "badge", "inspect",
		"unlock", "lock", "pass", "sessions", "revoke-all",
	}, usage: "/admin <subcommand>", desc: "instance administration"},
	"get": {usage: "/get <file-id>", desc: "download an offered file"},
	"update":  {usage: "/update", desc: "install the latest release"},
	"version": {usage: "/version", desc: "show the running version"},
}

// subUsage documents subcommands for suggestions and /help.
var subUsage = map[string]map[string]string{
	"group": {
		"create": "/group create <name>", "invite": "/group invite <user>", "remove": "/group remove <user>",
		"leave": "/group leave", "delete": "/group delete", "transfer": "/group transfer <user>", "members": "/group members",
	},
	"admin": {
		"reg": "/admin reg open|closed|invite", "approve": "/admin approve on|off", "cap": "/admin cap <n>",
		"invite": "/admin invite new [--uses n] [--expires 7d] | list | revoke <code>",
		"kick": "/admin kick <user>", "mute": "/admin mute <user>", "unmute": "/admin unmute <user>",
		"approve-user": "/admin approve-user <user>", "helper": "/admin helper add|remove <user>",
		"reset-key": "/admin reset-key <user>", "rename": "/admin rename <old> <new>", "announce": "/admin announce <text>",
		"maintenance": "/admin maintenance on|off [msg]", "min-version": "/admin min-version <v>",
		"limits": "/admin limits msgs <n>", "stats": "/admin stats", "log": "/admin log [n]",
		"devices": "/admin devices [approve <id>]", "groups": "/admin groups", "badge": "/admin badge on|off",
		"inspect": "/admin inspect <user>", "unlock": "/admin unlock", "lock": "/admin lock",
		"pass": "/admin pass", "sessions": "/admin sessions", "revoke-all": "/admin revoke-all",
	},
}

// Suggestion is one completion candidate for the input line.
type Suggestion struct {
	Text  string // what tab inserts, e.g. "/admin reg "
	Usage string
	Desc  string
}

// visible reports whether a command may be shown to this user. Admin-only
// commands are never suggested to plain users; helpers see the session
// commands they can run.
func visible(name string, admin, helper bool) bool {
	spec := commands[name]
	if !spec.admin || admin {
		return true
	}
	return helper && helperCommands[name]
}

// Suggest returns completions for a partially typed command line, sorted by
// name. It returns nil for lines that are not commands or are already past
// the point where completion helps.
func Suggest(line string, admin, helper bool) []Suggestion {
	if !strings.HasPrefix(line, "/") {
		return nil
	}
	body := strings.TrimPrefix(line, "/")
	fields := strings.Fields(body)
	trailing := strings.HasSuffix(body, " ")
	var out []Suggestion
	switch {
	case len(fields) == 0 || (len(fields) == 1 && !trailing):
		prefix := ""
		if len(fields) == 1 {
			prefix = strings.ToLower(fields[0])
		}
		names := make([]string, 0, len(commands))
		for n := range commands {
			if strings.HasPrefix(n, prefix) && visible(n, admin, helper) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, Suggestion{Text: "/" + n + " ", Usage: commands[n].usage, Desc: commands[n].desc})
		}
	case len(fields) == 1 || (len(fields) == 2 && !trailing):
		name := strings.ToLower(fields[0])
		spec, ok := commands[name]
		if !ok || spec.subs == nil || !visible(name, admin, helper) {
			return nil
		}
		prefix := ""
		if len(fields) == 2 {
			prefix = strings.ToLower(fields[1])
		}
		for _, sub := range spec.subs {
			if strings.HasPrefix(sub, prefix) {
				out = append(out, Suggestion{Text: "/" + name + " " + sub + " ", Usage: subUsage[name][sub]})
			}
		}
	}
	return out
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
func helpText(admin bool) []string { return helpFor(admin, admin) }

func helpFor(admin, helper bool) []string {
	names := make([]string, 0, len(commands))
	for n := range commands {
		if visible(n, admin, helper) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := []string{"commands (type / and use tab to complete):"}
	for _, n := range names {
		out = append(out, fmt.Sprintf("  %-44s %s", commands[n].usage, commands[n].desc))
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
