// Package theme defines the colour themes the PairDesk TUI renders with.
//
// Three themes are built in: "harbor" (the default: deep navy accents,
// teal/cyan primary, amber warnings, red danger), "paper" (a light theme) and
// "mono" (no colour at all, for terminals or people that prefer it).
//
// Users can add their own as JSON files in ~/.config/pairdesk/themes/<name>.json
// and select them with `/theme <name>` or Prefs.Theme. The schema is:
//
//	{
//	  "name":    "sunset",            // optional, defaults to the file name
//	  "base":    "harbor",            // optional built-in to inherit unset colours from
//	  "colors": {
//	    "fg":        "#e6e6e6",       // normal text
//	    "dim":       "#6b7280",       // secondary text, offline dots, timestamps
//	    "primary":   "#2dd4bf",       // primary accent: borders, titles, own name
//	    "accent":    "#67e8f9",       // sender names, highlights
//	    "bg_accent": "#0b1b2b",       // background of status bar and cards
//	    "warn":      "#f59e0b",       // amber: errors inline, announcements
//	    "danger":    "#ef4444",       // red: taken, deny, bans
//	    "ok":        "#22c55e",       // green: available, approve
//	    "border":    "#1e3a5f",       // pane borders
//	    "pulse":     "#ffffff"        // presence-dot pulse colour
//	  }
//	}
//
// Every colour is a hex "#rrggbb" string or an ANSI index "0".."255". An empty
// string means "no colour". Unknown keys are ignored.
package theme

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Colors is the raw palette of a theme.
type Colors struct {
	Fg       string `json:"fg"`
	Dim      string `json:"dim"`
	Primary  string `json:"primary"`
	Accent   string `json:"accent"`
	BgAccent string `json:"bg_accent"`
	Warn     string `json:"warn"`
	Danger   string `json:"danger"`
	OK       string `json:"ok"`
	Border   string `json:"border"`
	Pulse    string `json:"pulse"`
}

// file is the on-disk JSON shape. Pointers so that unset keys inherit.
type file struct {
	Name   string `json:"name"`
	Base   string `json:"base"`
	Colors struct {
		Fg       *string `json:"fg"`
		Dim      *string `json:"dim"`
		Primary  *string `json:"primary"`
		Accent   *string `json:"accent"`
		BgAccent *string `json:"bg_accent"`
		Warn     *string `json:"warn"`
		Danger   *string `json:"danger"`
		OK       *string `json:"ok"`
		Border   *string `json:"border"`
		Pulse    *string `json:"pulse"`
	} `json:"colors"`
}

// Theme is a palette plus the derived lipgloss styles the UI uses.
type Theme struct {
	Name   string
	Colors Colors

	Text       lipgloss.Style
	Dim        lipgloss.Style
	Primary    lipgloss.Style
	Accent     lipgloss.Style
	Warn       lipgloss.Style
	Danger     lipgloss.Style
	OK         lipgloss.Style
	Title      lipgloss.Style // bold primary
	Border     lipgloss.Style // pane border style
	Focused    lipgloss.Style // focused pane border
	StatusBar  lipgloss.Style // bottom bar
	Announce   lipgloss.Style // pinned announcement bar
	CardBox    lipgloss.Style // decision card block
	Online     lipgloss.Style // ● dot
	Offline    lipgloss.Style // ○ dot
	Pulse      lipgloss.Style // ● dot while pulsing
	Italic     lipgloss.Style
	Banner     lipgloss.Style
	Input      lipgloss.Style
	Mono       bool
}

var builtins = map[string]Colors{
	"harbor": {
		Fg: "#e2e8f0", Dim: "#64748b", Primary: "#2dd4bf", Accent: "#67e8f9",
		BgAccent: "#0b1b2b", Warn: "#f59e0b", Danger: "#ef4444", OK: "#22c55e",
		Border: "#1e3a5f", Pulse: "#ffffff",
	},
	"paper": {
		Fg: "#1f2937", Dim: "#9ca3af", Primary: "#0f766e", Accent: "#0369a1",
		BgAccent: "#f3f4f6", Warn: "#b45309", Danger: "#b91c1c", OK: "#15803d",
		Border: "#d1d5db", Pulse: "#000000",
	},
	"mono": {},
}

// Default is the name of the default theme.
const Default = "harbor"

// Names lists the built-in theme names, sorted.
func Names() []string {
	out := make([]string, 0, len(builtins))
	for k := range builtins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Builtin returns a built-in theme by name.
func Builtin(name string) (Theme, bool) {
	c, ok := builtins[name]
	if !ok {
		return Theme{}, false
	}
	return build(name, c), true
}

// Load resolves a theme by name: built-ins first, then
// <configDir>/themes/<name>.json. An empty name means Default. On any error
// the default theme is returned alongside the error so the UI always has
// something to draw with.
func Load(name, configDir string) (Theme, error) {
	if name == "" {
		name = Default
	}
	if t, ok := Builtin(name); ok {
		return t, nil
	}
	if configDir == "" {
		t, _ := Builtin(Default)
		return t, fmt.Errorf("no theme named %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		t, _ := Builtin(Default)
		return t, errors.New("theme names cannot contain path separators")
	}
	path := filepath.Join(configDir, "themes", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t, _ := Builtin(Default)
		if errors.Is(err, os.ErrNotExist) {
			return t, fmt.Errorf("no theme named %q (built-in: %s; or %s)", name, strings.Join(Names(), ", "), path)
		}
		return t, err
	}
	t, err := Parse(raw)
	if err != nil {
		d, _ := Builtin(Default)
		return d, fmt.Errorf("%s: %w", path, err)
	}
	if t.Name == "" {
		t.Name = name
	}
	return t, nil
}

// Parse builds a theme from the JSON described in the package comment.
func Parse(raw []byte) (Theme, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return Theme{}, err
	}
	base := Default
	if f.Base != "" {
		base = f.Base
	}
	c, ok := builtins[base]
	if !ok {
		return Theme{}, fmt.Errorf("unknown base theme %q", f.Base)
	}
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&c.Fg, f.Colors.Fg)
	set(&c.Dim, f.Colors.Dim)
	set(&c.Primary, f.Colors.Primary)
	set(&c.Accent, f.Colors.Accent)
	set(&c.BgAccent, f.Colors.BgAccent)
	set(&c.Warn, f.Colors.Warn)
	set(&c.Danger, f.Colors.Danger)
	set(&c.OK, f.Colors.OK)
	set(&c.Border, f.Colors.Border)
	set(&c.Pulse, f.Colors.Pulse)
	return build(f.Name, c), nil
}

func color(s string) lipgloss.TerminalColor {
	if s == "" {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color(s)
}

func build(name string, c Colors) Theme {
	mono := c == Colors{}
	t := Theme{Name: name, Colors: c, Mono: mono}
	t.Text = lipgloss.NewStyle().Foreground(color(c.Fg))
	t.Dim = lipgloss.NewStyle().Foreground(color(c.Dim))
	if mono {
		t.Dim = lipgloss.NewStyle().Faint(true)
	}
	t.Primary = lipgloss.NewStyle().Foreground(color(c.Primary))
	t.Accent = lipgloss.NewStyle().Foreground(color(c.Accent))
	if mono {
		t.Accent = lipgloss.NewStyle().Bold(true)
	}
	t.Warn = lipgloss.NewStyle().Foreground(color(c.Warn))
	t.Danger = lipgloss.NewStyle().Foreground(color(c.Danger))
	t.OK = lipgloss.NewStyle().Foreground(color(c.OK))
	t.Title = t.Primary.Bold(true)
	t.Border = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color(c.Border))
	t.Focused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color(c.Primary))
	t.StatusBar = lipgloss.NewStyle().Foreground(color(c.Dim)).Background(color(c.BgAccent))
	t.Announce = lipgloss.NewStyle().Foreground(color(c.Warn)).Background(color(c.BgAccent)).Bold(true)
	if mono {
		t.Announce = lipgloss.NewStyle().Reverse(true)
		t.StatusBar = lipgloss.NewStyle().Faint(true)
	}
	t.CardBox = lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(color(c.Warn)).Padding(0, 1)
	t.Online = lipgloss.NewStyle().Foreground(color(c.OK))
	t.Offline = t.Dim
	t.Pulse = lipgloss.NewStyle().Foreground(color(c.Pulse)).Bold(true)
	t.Italic = lipgloss.NewStyle().Italic(true)
	t.Banner = t.Primary.Bold(true)
	t.Input = lipgloss.NewStyle().Foreground(color(c.Fg))
	return t
}
