package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

// onKey dispatches a key press by phase.
func (m *ui) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, m.quit()
	}
	if m.phase != app.PhaseReady {
		return m.onLoginKey(msg)
	}
	return m.onMainKey(msg)
}

func (m *ui) onMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Sudo password prompt captures everything.
	if m.sudoAsk != nil {
		switch key {
		case "esc":
			p := m.sudoAsk
			m.sudoAsk = nil
			m.masked.Reset()
			m.masked.Blur()
			m.input.Focus()
			m.layout()
			if p.onCancel != nil {
				p.onCancel()
			}
			return m, nil
		case "enter":
			p := m.sudoAsk
			pw := m.masked.Value()
			m.sudoAsk = nil
			m.masked.Reset()
			m.masked.Blur()
			m.input.Focus()
			m.layout()
			if p.onSubmit != nil {
				return m, p.onSubmit(pw)
			}
			return m, m.run("", func(ctx context.Context) ([]string, error) {
				if err := m.app.Grant(ctx, p.name, p.scope, model.SudoAuto, pw); err != nil {
					return nil, err
				}
				return []string{"granted " + string(p.scope) + " to " + p.name + " with auto sudo"}, nil
			})
		}
		var cmd tea.Cmd
		m.masked, cmd = m.masked.Update(msg)
		return m, cmd
	}

	switch key {
	case "ctrl+k":
		return m, m.hook("stop", func(ctx context.Context) ([]string, error) {
			if m.hooks.Stop == nil {
				return nil, nil
			}
			return m.hooks.Stop(ctx)
		}, m.hooks.Stop != nil)
	case "tab", "shift+tab":
		if key == "tab" && m.pane == paneChat && len(m.sugs) > 0 {
			// Complete to the highlighted suggestion; tab again cycles through
			// the candidates of the line that started the completion.
			cur := m.input.Value()
			if m.sugBase == "" {
				m.sugBase = cur
			}
			if cur == m.sugs[m.sugIdx].Text && len(m.sugs) > 1 {
				m.sugIdx = (m.sugIdx + 1) % len(m.sugs)
			}
			m.input.SetValue(m.sugs[m.sugIdx].Text)
			m.input.CursorEnd()
			return m, nil
		}
		if m.pane == paneLeft {
			m.pane = paneChat
			m.input.Focus()
		} else {
			m.pane = paneLeft
			m.input.Blur()
		}
		return m, nil
	case "esc":
		switch {
		case m.announcement != "":
			m.announcement = ""
			m.layout()
		case m.verify != nil:
			m.verify = nil
			m.layout()
		case m.requests:
			m.requests = false
			m.layout()
		case m.input.Value() != "":
			m.input.Reset()
		case m.pane == paneLeft:
			m.pane = paneChat
			m.input.Focus()
		}
		return m, nil
	case "pgup":
		m.vp.ViewUp()
		return m, nil
	case "pgdown":
		m.vp.ViewDown()
		m.checkDivider()
		return m, nil
	case "end":
		m.vp.GotoBottom()
		m.checkDivider()
		return m, nil
	case "home":
		m.vp.GotoTop()
		return m, nil
	}

	if m.pane == paneLeft {
		items := m.leftItems()
		switch key {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(items)-1 {
				m.cursor++
			}
		case "enter":
			if m.cursor < len(items) {
				return m, m.openByID(items[m.cursor].conv, items[m.cursor].isGrp)
			}
		}
		return m, nil
	}

	// Single-key answers when the input line is empty.
	if m.input.Value() == "" {
		switch key {
		case "a", "d", "s":
			if m.requests {
				reqs := m.app.PendingRequests()
				if len(reqs) > 0 {
					name := reqs[0].Username
					accept := key == "a"
					return m, m.run("", func(ctx context.Context) ([]string, error) {
						if err := m.app.RespondFriend(ctx, name, accept); err != nil {
							return nil, err
						}
						if accept {
							return []string{"accepted " + name}, nil
						}
						return []string{"denied " + name}, nil
					})
				}
			}
			if m.hasIdleCard() {
				m.decideCard(key != "d", key == "s")
				return m, m.animTick()
			}
		case "y":
			if m.verify != nil {
				name := m.verify.name
				m.verify = nil
				m.layout()
				return m, m.run("", func(ctx context.Context) ([]string, error) {
					if err := m.app.ConfirmVerify(ctx, name); err != nil {
						return nil, err
					}
					return []string{name + " verified"}, nil
				})
			}
		}
	}

	// With the suggestion strip open, up/down pick a row and enter takes it:
	// a command that needs nothing more runs at once, otherwise the line is
	// filled in and the cursor waits for the argument.
	if len(m.sugs) > 0 {
		switch key {
		case "up":
			m.sugIdx = (m.sugIdx + len(m.sugs) - 1) % len(m.sugs)
			return m, nil
		case "down":
			m.sugIdx = (m.sugIdx + 1) % len(m.sugs)
			return m, nil
		case "enter":
			sel := m.sugs[m.sugIdx]
			if strings.TrimSpace(m.input.Value()) != strings.TrimSpace(sel.Text) {
				if sel.Complete() {
					m.input.Reset()
					m.refreshSuggestions(true)
					c, err := Parse(sel.Text)
					if err != nil {
						return m, m.warn(err.Error())
					}
					return m, m.exec(c)
				}
				m.input.SetValue(sel.Text)
				m.input.CursorEnd()
				m.refreshSuggestions(true)
				return m, nil
			}
		}
	}

	if key == "enter" {
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.input.Reset()
		m.refreshSuggestions(true)
		if strings.HasPrefix(text, "/") {
			c, err := Parse(text)
			if err != nil {
				m.addNote("", err.Error(), true)
				m.renderChat(true)
				return m, nil
			}
			return m, m.exec(c)
		}
		return m, m.sendText(text)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.refreshSuggestions(true)
	return m, cmd
}

// refreshSuggestions recomputes the completion strip for the current input.
// The index is kept while the candidate list is unchanged so tab can cycle.
func (m *ui) refreshSuggestions(reset bool) {
	var sugs []Suggestion
	if reset {
		m.sugBase = ""
	}
	if m.pane == paneChat && m.sudoAsk == nil {
		sugs = Suggest(m.input.Value(), m.app.IsAdmin(), m.app.IsHelperOrAdmin())
	}
	if len(sugs) > maxSuggestions {
		sugs = sugs[:maxSuggestions]
	}
	if reset || m.sugIdx >= len(sugs) {
		m.sugIdx = 0
	}
	changed := len(sugs) != len(m.sugs)
	m.sugs = sugs
	if changed {
		m.layout()
	}
}

// checkDivider clears the unread divider once the user reaches the bottom.
func (m *ui) checkDivider() {
	if m.divider != "" && m.vp.AtBottom() {
		m.divider = ""
		m.renderChat(true)
	}
}

// sendText sends a chat message to the active conversation.
func (m *ui) sendText(text string) tea.Cmd {
	c := m.app.Conv(m.active)
	if c == nil {
		m.addNote("", "no conversation open; /dm <user> first", true)
		m.renderChat(true)
		return nil
	}
	m.divider = ""
	m.pending = append(m.pending, pendingMsg{Conv: c.ID, Text: text, At: time.Now().UnixNano()})
	m.renderChat(true)
	ctx := m.ctx
	conv := c.ID
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return sentMsg{conv: conv, err: m.app.Send(cctx, c, text)}
	}
}

// openByID opens a DM or group conversation by conversation id.
func (m *ui) openByID(convID string, group bool) tea.Cmd {
	c := m.app.Conv(convID)
	if c == nil && !group {
		// Admin "everyone" pane: DM with a non-friend.
		for _, u := range m.allUsers {
			if model.DMConvID(m.app.Me().ID, u.User.ID) == convID {
				name := u.User.Username
				return m.openDM(name)
			}
		}
		return nil
	}
	if c == nil {
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := m.app.LoadHistory(cctx, c)
		return convOpenedMsg{conv: c, err: err}
	}
}

func (m *ui) openDM(name string) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, err := m.app.OpenDM(cctx, name)
		return convOpenedMsg{conv: c, err: err}
	}
}

// run executes fn in the background and shows its lines/error in the chat.
func (m *ui) run(conv string, fn func(context.Context) ([]string, error)) tea.Cmd {
	if conv == "" {
		conv = m.active
	}
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		lines, err := fn(cctx)
		return resultMsg{lines: lines, err: err, conv: conv}
	}
}

// hook runs an optional hook, or says it is missing.
func (m *ui) hook(name string, fn func(context.Context) ([]string, error), present bool) tea.Cmd {
	if !present {
		m.addNote("", "/"+name+" is not available in this build", true)
		m.renderChat(true)
		return nil
	}
	ctx := m.ctx
	conv := m.active
	return func() tea.Msg {
		lines, err := fn(ctx)
		return resultMsg{lines: lines, err: err, conv: conv}
	}
}
