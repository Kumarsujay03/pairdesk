package tui

import (
	"context"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,20}$`)

// banner renders the revealed part of the block-letter banner plus the
// version line.
func (m *ui) banner() string {
	var b strings.Builder
	for i := 0; i < m.bannerShown && i < len(bannerLines); i++ {
		b.WriteString(m.th.Banner.Render(bannerLines[i]))
		b.WriteString("\n")
	}
	for i := m.bannerShown; i < len(bannerLines); i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.th.Dim.Render("pairdesk " + m.version))
	return b.String()
}

// forgeBlock renders the key-forging progress / phrase reveal, or "".
func (m *ui) forgeBlock() string {
	f := m.forge
	if f == nil || f.expired() {
		return ""
	}
	var b strings.Builder
	if f.progress() < 1 {
		b.WriteString(m.th.Dim.Render("forging device key ") + m.th.Primary.Render(bar(f.progress(), 24)))
		return b.String()
	}
	b.WriteString(m.th.Dim.Render("your device key phrase: "))
	n := f.revealed()
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(m.th.Accent.Bold(true).Render(f.words[i]))
	}
	return b.String()
}

func (m *ui) errBlock() string {
	if m.errLine == "" {
		return ""
	}
	return m.th.Warn.Render(m.errLine)
}

func (m *ui) loginScreen(lines ...string) string {
	all := []string{m.banner(), ""}
	if fb := m.forgeBlock(); fb != "" {
		all = append(all, fb, "")
	}
	all = append(all, lines...)
	if eb := m.errBlock(); eb != "" {
		all = append(all, "", eb)
	}
	body := lipgloss.JoinVertical(lipgloss.Left, all...)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, body)
}

func (m *ui) viewLogin() string {
	hint := "enter your email; we send a one-time code"
	if m.demo {
		hint = "demo mode: any email works and the code is 000000"
	}
	busy := ""
	if m.busy {
		busy = m.spin.View() + " sending…"
	}
	return m.loginScreen(
		m.th.Title.Render("log in"),
		m.th.Dim.Render(hint),
		"",
		m.email.View(),
		busy,
		m.th.Dim.Render("enter continue · ctrl+c quit"),
	)
}

func (m *ui) viewOTP() string {
	busy := ""
	if m.busy {
		busy = m.spin.View() + " checking…"
	}
	hint := "enter the 6-digit code from your email"
	if m.demo {
		hint = "demo mode: the code is 000000"
	}
	return m.loginScreen(
		m.th.Title.Render("one-time code"),
		m.th.Dim.Render(hint),
		"",
		m.otp.View(),
		busy,
		m.th.Dim.Render("enter verify · esc back · ctrl+c quit"),
	)
}

func (m *ui) availText() string {
	switch m.avail {
	case availChecking:
		return m.th.Dim.Render("checking…")
	case availOK:
		return m.th.OK.Render("✔ available")
	case availTaken:
		return m.th.Danger.Render("✘ taken")
	case availInvalid:
		return m.th.Dim.Render("– invalid (3-20 chars of a-z 0-9 _)")
	case availError:
		return m.th.Warn.Render("? could not check")
	}
	return ""
}

func (m *ui) viewUsername() string {
	lines := []string{
		m.th.Title.Render("choose a username"),
		m.th.Dim.Render("3 to 20 chars, a-z 0-9 _ · this is the name friends use to find you"),
		"",
		m.user.View() + "  " + m.availText(),
	}
	if m.app.Settings().Registration == model.RegInvite {
		lines = append(lines, m.invite.View()+"  "+m.th.Dim.Render("invite code required · tab to switch"))
	}
	busy := ""
	if m.busy {
		busy = m.spin.View() + " registering…"
	}
	lines = append(lines, busy, m.th.Dim.Render("enter register · ctrl+c quit"))
	return m.loginScreen(lines...)
}

// viewBoot is shown while the saved session is being resumed: the banner
// reveals itself, the spinner turns and the current step is named.
func (m *ui) viewBoot() string {
	if m.locked {
		return m.loginScreen(
			m.th.Title.Render("locked"),
			m.th.Dim.Render("enter your passkey to open pairdesk"),
			"",
			m.passkey.View(),
			m.th.Dim.Render("enter continue · ctrl+c quit"),
		)
	}
	if m.bootErr != "" {
		return m.loginScreen(
			m.th.Warn.Render("could not start: "+m.bootErr),
			m.th.Dim.Render("r retry · q quit"),
		)
	}
	step := m.bootStep
	if step == "" {
		step = "starting"
	}
	return m.loginScreen(
		m.spin.View()+" "+m.th.Text.Render(step+"…"),
		m.th.Dim.Render("ctrl+c quit"),
	)
}

func (m *ui) viewPending() string {
	return m.loginScreen(
		m.spin.View()+" "+m.th.Text.Render("waiting for the admin to let you in"),
		m.th.Dim.Render("this screen refreshes on its own · ctrl+c quit"),
	)
}

func (m *ui) viewWall(title, body, keys string) string {
	return m.loginScreen(
		m.th.Title.Render(title),
		m.th.Text.Render(body),
		"",
		m.th.Dim.Render(keys),
	)
}

// --- key handling for the login phases --------------------------------------

func (m *ui) onLoginKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.phase {
	case app.PhaseBoot:
		if m.locked {
			if key == "enter" {
				pw := m.passkey.Value()
				m.passkey.Reset()
				if err := m.applock.Unlock(pw); err != nil {
					m.errLine = err.Error()
					return m, nil
				}
				m.errLine = ""
				m.locked = false
				m.passkey.Blur()
				return m, tea.Batch(m.runBoot(), m.spin.Tick)
			}
			var cmd tea.Cmd
			m.passkey, cmd = m.passkey.Update(msg)
			return m, cmd
		}
		switch {
		case key == "q" && m.bootErr != "":
			return m, m.quit()
		case key == "r" && m.bootErr != "":
			return m, tea.Batch(m.runBoot(), m.spin.Tick)
		}
		return m, nil
	case app.PhaseNeedLogin:
		if key == "enter" && !m.busy {
			email := strings.TrimSpace(m.email.Value())
			if email == "" {
				return m, nil
			}
			m.busy = true
			m.errLine = ""
			return m, tea.Batch(m.spin.Tick, m.call(func(ctx context.Context) error {
				return m.app.RequestLogin(ctx, email)
			}))
		}
	case app.PhaseNeedOTP:
		if key == "esc" {
			m.phase = app.PhaseNeedLogin
			m.errLine = ""
			m.focusForPhase()
			return m, nil
		}
		if key == "enter" && !m.busy {
			code := strings.TrimSpace(m.otp.Value())
			if len(code) != 6 {
				m.errLine = "the code is 6 digits"
				return m, nil
			}
			m.busy = true
			m.errLine = ""
			return m, tea.Batch(m.spin.Tick, m.call(func(ctx context.Context) error {
				return m.app.VerifyOTP(ctx, code)
			}))
		}
	case app.PhaseRegClosed:
		if key == "r" && !m.busy {
			m.busy = true
			m.errLine = ""
			return m, tea.Batch(m.spin.Tick, m.retryRegistration())
		}
		if key == "q" {
			return m, m.quit()
		}
	case app.PhaseNeedUsername:
		switch key {
		case "tab", "shift+tab", "down", "up":
			if m.app.Settings().Registration == model.RegInvite {
				m.regFocus = 1 - m.regFocus
				m.focusForPhase()
			}
			return m, nil
		case "enter":
			if m.busy {
				return m, nil
			}
			name := strings.ToLower(strings.TrimSpace(m.user.Value()))
			if m.avail != availOK {
				if m.avail == availChecking {
					m.errLine = "still checking that name…"
				} else {
					m.errLine = "pick a username that shows ✔ available"
				}
				return m, nil
			}
			invite := strings.TrimSpace(m.invite.Value())
			if m.app.Settings().Registration == model.RegInvite && invite == "" {
				m.errLine = "an invite code is required"
				m.regFocus = 1
				m.focusForPhase()
				return m, nil
			}
			m.busy = true
			m.errLine = ""
			return m, tea.Batch(m.spin.Tick, m.call(func(ctx context.Context) error {
				return m.app.Register(ctx, name, invite)
			}))
		}
		before := m.user.Value()
		cmd := m.updateInputs(msg)
		if m.user.Value() != before {
			return m, tea.Batch(cmd, m.scheduleCheck())
		}
		return m, cmd
	case app.PhaseBanned, app.PhaseMaintenance, app.PhaseUpdateRequired:
		if key == "q" {
			return m, m.quit()
		}
		return m, nil
	case app.PhasePendingApproval:
		return m, nil
	}
	return m, m.updateInputs(msg)
}

// scheduleCheck debounces the live availability check by 300ms.
func (m *ui) scheduleCheck() tea.Cmd {
	m.availSeq++
	seq := m.availSeq
	m.errLine = ""
	name := strings.ToLower(strings.TrimSpace(m.user.Value()))
	if name == "" {
		m.avail = availNone
		return nil
	}
	if !usernameRe.MatchString(name) {
		m.avail = availInvalid
		return nil
	}
	m.avail = availChecking
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return debounceMsg{seq} })
}

// checkUsername asks the app whether the current name is free.
func (m *ui) checkUsername(seq int) tea.Cmd {
	name := strings.ToLower(strings.TrimSpace(m.user.Value()))
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ok, err := m.app.CheckUsername(cctx, name)
		return availMsg{seq: seq, available: ok, err: err}
	}
}

// call runs an app method in the background; its error (if any) lands in
// the error line / notes.
func (m *ui) call(fn func(context.Context) error) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return resultMsg{err: fn(cctx)}
	}
}

type regRetryMsg struct {
	open bool
	err  error
}

// retryRegistration re-reads instance settings; if registration opened up
// the username screen is shown (Register then drives the app phase).
func (m *ui) retryRegistration() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		s, err := m.app.Backend().Settings(cctx)
		if err != nil {
			return regRetryMsg{err: err}
		}
		return regRetryMsg{open: s.Registration != model.RegClosed}
	}
}
