package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

type cardState int

const (
	cardIdle cardState = iota
	cardApproving
	cardDenying
)

const (
	cardFlashDur = 350 * time.Millisecond
	cardShakeDur = 180 * time.Millisecond
	cardSlideIn  = 12.0
)

// card is a decision block shown above the input.
type card struct {
	app.Card
	state   cardState
	since   time.Time // state change time
	created time.Time
	slide   *spring
	done    bool // reply already sent
	tier    model.SudoTier
}

var shakeFrames = []int{2, -2, 1}

func (m *ui) addCard(c *card) {
	for _, e := range m.cards {
		if e.ID == c.ID {
			return
		}
	}
	c.created = time.Now()
	if !m.reduced {
		c.slide = &spring{pos: cardSlideIn}
	}
	m.cards = append(m.cards, c)
	m.layout()
}

func (m *ui) removeCard(id string) {
	keep := m.cards[:0]
	for _, c := range m.cards {
		if c.ID != id {
			keep = append(keep, c)
		}
	}
	m.cards = keep
	m.layout()
}

// expireCards drops cards past their deadline and finished animations.
func (m *ui) expireCards() {
	now := time.Now()
	keep := m.cards[:0]
	for _, c := range m.cards {
		if c.ExpiresAt > 0 && now.UnixNano() > c.ExpiresAt && c.state == cardIdle {
			m.addNote("", "request from "+c.FromName+" expired", false)
			continue
		}
		if c.state == cardApproving && now.Sub(c.since) > cardFlashDur {
			continue
		}
		if c.state == cardDenying && now.Sub(c.since) > cardShakeDur {
			continue
		}
		keep = append(keep, c)
	}
	if len(keep) != len(m.cards) {
		m.cards = keep
		m.layout()
		m.renderChat(true)
	}
}

// decideCard answers the oldest idle card.
func (m *ui) decideCard(accept bool, sudo bool) bool {
	for _, c := range m.cards {
		if c.state != cardIdle || c.done {
			continue
		}
		tier := model.SudoBlocked
		if sudo && c.Kind == "access" && (c.Scope == model.SessionBoard || c.Scope == model.SessionWatch) {
			tier = model.SudoAsk
		}
		c.done = true
		c.since = time.Now()
		if accept {
			c.state = cardApproving
		} else {
			c.state = cardDenying
		}
		c.tier = tier
		if c.Kind == "sudo" && c.ReplyWithPassword != nil {
			reply := c.ReplyWithPassword
			if !accept {
				go reply(false, "")
			} else {
				m.askMasked("your sudo password for this one command (typed here, never sent to them)",
					func(pw string) tea.Cmd { go reply(true, pw); return nil },
					func() { go reply(false, "") })
			}
		} else if c.Reply != nil {
			reply := c.Reply
			go reply(accept, tier)
		}
		if m.reduced {
			// No flash/shake: drop it on the next slow tick.
			c.since = time.Now().Add(-time.Hour)
		}
		return true
	}
	return false
}

// hasIdleCard reports whether a/d/s should be captured.
func (m *ui) hasIdleCard() bool {
	for _, c := range m.cards {
		if c.state == cardIdle && !c.done {
			return true
		}
	}
	return false
}

func cardTitle(c *card) string {
	switch c.Kind {
	case "friend":
		return "friend request"
	case "access":
		s := string(c.Scope)
		if c.Control {
			s += " with control"
		}
		return "access request · " + s
	case "sudo":
		return "sudo request"
	case "share":
		return "share request"
	case "file":
		return "incoming file"
	case "group":
		return "group invite"
	case "device":
		return "new admin device"
	case "keychange":
		return "device key changed"
	}
	return c.Kind
}

// renderCards draws every card as a block; width is the available width.
func (m *ui) renderCards(width int) string {
	if len(m.cards) == 0 {
		return ""
	}
	var blocks []string
	now := time.Now()
	for _, c := range m.cards {
		inner := width - 4
		if inner < 20 {
			inner = 20
		}
		var lines []string
		lines = append(lines, m.th.Title.Render(cardTitle(c)))
		text := c.Text
		if text == "" {
			text = c.FromName + " is asking"
		}
		if c.Command != "" {
			text += "  " + m.th.Accent.Render("$ "+c.Command)
		}
		lines = append(lines, lipgloss.NewStyle().Width(inner).Render(text))
		keys := "[a]pprove · [d]eny"
		if c.Kind == "access" && (c.Scope == model.SessionBoard || c.Scope == model.SessionWatch) {
			keys = "[a]pprove · [s] approve with sudo · [d]eny"
		}
		if c.Kind == "keychange" {
			keys = "[a]ccept new key · [d]ismiss"
		}
		switch c.state {
		case cardApproving:
			lines = append(lines, m.th.OK.Bold(true).Render("✔ approved"))
		case cardDenying:
			lines = append(lines, m.th.Danger.Render("✘ denied"))
		default:
			lines = append(lines, m.th.Dim.Render(keys+"  (when the input line is empty)"))
			if c.ExpiresAt > 0 {
				total := time.Duration(c.ExpiresAt - c.created.UnixNano())
				left := time.Duration(c.ExpiresAt - now.UnixNano())
				if total <= 0 {
					total = time.Second
				}
				frac := float64(left) / float64(total)
				w := inner - 8
				if w > 40 {
					w = 40
				}
				secs := int(left.Seconds())
				if secs < 0 {
					secs = 0
				}
				lines = append(lines, m.th.Warn.Render(bar(frac, w))+m.th.Dim.Render(fmt.Sprintf(" %3ds", secs)))
			}
		}
		box := m.th.CardBox
		switch c.state {
		case cardApproving:
			box = box.BorderForeground(m.th.OK.GetForeground())
		case cardDenying:
			box = box.BorderForeground(m.th.Danger.GetForeground())
		}
		body := box.Width(inner).Render(strings.Join(lines, "\n"))
		off := 0
		if c.state == cardDenying && !m.reduced {
			frame := int(now.Sub(c.since) / (cardShakeDur / time.Duration(len(shakeFrames))))
			if frame < len(shakeFrames) {
				off = shakeFrames[frame]
			}
		} else if c.slide != nil {
			off = c.slide.offset()
		}
		if off > 0 {
			body = indent(body, off)
		}
		blocks = append(blocks, body)
	}
	return strings.Join(blocks, "\n")
}

func indent(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
