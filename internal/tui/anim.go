package tui

import (
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/harmonica"
)

// bannerLines is the block-letter banner drawn line by line at launch.
var bannerLines = []string{
	` ____       _      ____            _    `,
	`|  _ \ __ _(_)_ __|  _ \  ___  ___| | __`,
	`| |_) / _` + "`" + ` | | '__| | | |/ _ \/ __| |/ /`,
	`|  __/ (_| | | |  | |_| |  __/\__ \   < `,
	`|_|   \__,_|_|_|  |____/ \___||___/_|\_\`,
}

// pulseDur is how long a presence dot stays bright after coming online.
const pulseDur = 600 * time.Millisecond

// spring animates one message's x offset into place.
type spring struct {
	pos, vel float64
}

var msgSpring = harmonica.NewSpring(harmonica.FPS(30), 9.0, 0.55)

// step advances the spring toward 0 and reports whether it has settled.
func (s *spring) step() bool {
	s.pos, s.vel = msgSpring.Update(s.pos, s.vel, 0)
	return math.Abs(s.pos) < 0.4 && math.Abs(s.vel) < 0.4
}

func (s *spring) offset() int {
	if s == nil || s.pos < 0.5 {
		return 0
	}
	return int(s.pos + 0.5)
}

// forgeAnim shows "forging key" then reveals the phrase word by word.
type forgeAnim struct {
	words   []string
	start   time.Time
	instant bool
}

const (
	forgeBarDur  = 900 * time.Millisecond
	forgeWordDur = 350 * time.Millisecond
	forgeLinger  = 5 * time.Second
)

func newForge(phrase string, instant bool) *forgeAnim {
	return &forgeAnim{words: strings.Fields(phrase), start: time.Now(), instant: instant}
}

// progress is 0..1 for the bar.
func (f *forgeAnim) progress() float64 {
	if f.instant {
		return 1
	}
	p := float64(time.Since(f.start)) / float64(forgeBarDur)
	if p > 1 {
		p = 1
	}
	return p
}

// revealed is how many words are visible.
func (f *forgeAnim) revealed() int {
	if f.instant {
		return len(f.words)
	}
	after := time.Since(f.start) - forgeBarDur
	if after < 0 {
		return 0
	}
	n := int(after/forgeWordDur) + 1
	if n > len(f.words) {
		n = len(f.words)
	}
	return n
}

// finished reports that every word is visible (the block may still linger).
func (f *forgeAnim) finished() bool { return f.revealed() == len(f.words) }

// expired reports that the block should disappear.
func (f *forgeAnim) expired() bool {
	total := forgeLinger
	if !f.instant {
		total += forgeBarDur + time.Duration(len(f.words))*forgeWordDur
	}
	return time.Since(f.start) > total
}

// bar renders a progress bar of width w.
func bar(frac float64, w int) string {
	if w < 1 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(math.Round(frac * float64(w)))
	return strings.Repeat("█", n) + strings.Repeat("░", w-n)
}

// typingDots animates "…" for the typing indicator.
func typingDots(frame int) string {
	return strings.Repeat(".", frame%3+1) + strings.Repeat(" ", 2-frame%3)
}
