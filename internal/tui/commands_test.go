package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		line    string
		want    Command
		wantErr string
	}{
		{line: "hello", wantErr: "not a command"},
		{line: "/", wantErr: "empty command"},
		{line: "/nope arg", wantErr: "unknown command /nope"},
		{line: "/help", want: Command{Name: "help"}},
		{line: "/add arjun", want: Command{Name: "add", Args: []string{"arjun"}, Rest: "arjun"}},
		{line: "  /DM  priya ", want: Command{Name: "dm", Args: []string{"priya"}, Rest: "priya"}},
		{line: "/me waves at everyone", want: Command{Name: "me", Args: []string{"waves", "at", "everyone"}, Rest: "waves at everyone"}},
		{line: "/group", wantErr: "usage: /group"},
		{line: "/group dance", wantErr: `unknown /group subcommand "dance"`},
		{line: "/group create linux lab", want: Command{Name: "group", Sub: "create", Args: []string{"linux", "lab"}, Rest: "linux lab"}},
		{line: "/group invite arjun", want: Command{Name: "group", Sub: "invite", Args: []string{"arjun"}, Rest: "arjun"}},
		{line: "/grant sujay03 --watch-only --sudo ask", want: Command{Name: "grant", Args: []string{"sujay03"}, Flags: map[string]string{"watch-only": "true", "sudo": "ask"}, Rest: "sujay03 --watch-only --sudo ask"}},
		{line: "/grant sujay03 --sudo=auto", want: Command{Name: "grant", Args: []string{"sujay03"}, Flags: map[string]string{"sudo": "auto"}, Rest: "sujay03 --sudo=auto"}},
		{line: "/screen arjun --control", want: Command{Name: "screen", Args: []string{"arjun"}, Flags: map[string]string{"control": "true"}, Rest: "arjun --control", Admin: true}},
		{line: "/users", want: Command{Name: "users", Admin: true}},
		{line: "/ban kiran being rude", want: Command{Name: "ban", Args: []string{"kiran", "being", "rude"}, Rest: "kiran being rude", Admin: true}},
		{line: "/admin", wantErr: "usage: /admin"},
		{line: "/admin reg invite", want: Command{Name: "admin", Sub: "reg", Args: []string{"invite"}, Rest: "invite", Admin: true}},
		{line: "/admin invite new --uses 5 --expires 7d", want: Command{Name: "admin", Sub: "invite", Args: []string{"new"}, Flags: map[string]string{"uses": "5", "expires": "7d"}, Rest: "new --uses 5 --expires 7d", Admin: true}},
		{line: "/admin announce hello there  friends", want: Command{Name: "admin", Sub: "announce", Args: []string{"hello", "there", "friends"}, Rest: "hello there  friends", Admin: true}},
		{line: "/admin announce", want: Command{Name: "admin", Sub: "announce", Admin: true}},
		{line: "/admin maintenance on back in 10", want: Command{Name: "admin", Sub: "maintenance", Args: []string{"on", "back", "in", "10"}, Rest: "on back in 10", Admin: true}},
		{line: "/admin log --tail 50", want: Command{Name: "admin", Sub: "log", Flags: map[string]string{"tail": "50"}, Rest: "--tail 50", Admin: true}},
		{line: "/admin bogus", wantErr: `unknown /admin subcommand "bogus"`},
		{line: "/theme paper", want: Command{Name: "theme", Args: []string{"paper"}, Rest: "paper"}},
		{line: "/quit", want: Command{Name: "quit"}},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got, err := Parse(tc.line)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v (cmd %+v)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want.Flags == nil {
				tc.want.Flags = map[string]string{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestCommandTail(t *testing.T) {
	c, err := Parse("/ban kiran being rude  today")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tail(1); got != "being rude  today" {
		t.Fatalf("Tail(1) = %q", got)
	}
	c, _ = Parse("/admin maintenance on")
	if got := c.Tail(1); got != "" {
		t.Fatalf("Tail(1) = %q, want empty", got)
	}
}

func TestHelpHidesAdminCommands(t *testing.T) {
	user := strings.Join(helpText(false), "\n")
	admin := strings.Join(helpText(true), "\n")
	if strings.Contains(user, "/admin") || strings.Contains(user, "/ban") {
		t.Fatalf("user help leaks admin commands:\n%s", user)
	}
	if !strings.Contains(admin, "/admin reg") || !strings.Contains(admin, "/ban <user>") {
		t.Fatalf("admin help misses admin commands:\n%s", admin)
	}
}
