// Package tui is PairDesk's terminal user interface, built on Bubble Tea. It
// renders whatever app.App reports and calls back into it; it owns no state
// of its own beyond what is needed to draw.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/tui/theme"
)

// Hooks are optional entry points later phases plug in for the remote-access
// and file commands. A nil hook makes its command print "not available in
// this build". Each hook runs in a tea.Cmd goroutine and may block; the
// returned lines (if any) are shown in the chat as system lines.
type Hooks struct {
	// Board opens an interactive shell on a friend's machine (/board <user>).
	Board func(ctx context.Context, user string, args []string) ([]string, error)
	// Watch opens a read-only view of a friend's terminal (/watch <user>).
	Watch func(ctx context.Context, user string, args []string) ([]string, error)
	// Screen views a friend's screen; control is true for --control (/screen).
	Screen func(ctx context.Context, user string, control bool) ([]string, error)
	// Send drops a file to a friend peer to peer (/send <user> <path>).
	Send func(ctx context.Context, user, path string) ([]string, error)
	// Share turns the current board session into a shared terminal (/share).
	Share func(ctx context.Context) ([]string, error)
	// Logs browses or replays sessions on this machine (/logs [replay <id>]).
	Logs func(ctx context.Context, args []string) ([]string, error)
	// Stop kills any live session on this machine (/stop, ctrl+k).
	Stop func(ctx context.Context) ([]string, error)
	// Search searches the local decrypted history (/search <text>).
	Search func(ctx context.Context, query string) ([]string, error)
	// End ends the current session (/end).
	End func(ctx context.Context) ([]string, error)
	// Fetch downloads a file shared in the current group (/get <id>).
	Fetch func(ctx context.Context, convID, id string) ([]string, error)
	// Unlock verifies the admin passphrase (/admin unlock).
	Unlock func(ctx context.Context, pass string) error
	// Passphrase sets or changes the admin passphrase (/admin pass).
	Passphrase func(ctx context.Context, old, pass string) error
	// Lock relocks admin commands (/admin lock).
	Lock func(ctx context.Context) ([]string, error)
	// Sessions lists live sessions (/admin sessions).
	Sessions func(ctx context.Context) ([]string, error)
	// RevokeAll ends every session and drops held grants (/admin revoke-all).
	RevokeAll func(ctx context.Context) ([]string, error)
}

// Options tune Run.
type Options struct {
	Version string
	Hooks   Hooks
	// Demo is true when the in-memory backend is used; the login screen then
	// says that any code of 000000 works.
	Demo bool
	// ReducedMotion forces animations off regardless of Prefs.
	ReducedMotion bool
	// ProgramReady is called with the running program so the session layer
	// can release and restore the terminal around a raw session.
	ProgramReady func(p *tea.Program)
}

// Run builds the UI, starts the Bubble Tea program in the alternate screen
// (mouse off), pumps a.Events() into it and blocks until the user quits or
// ctx is cancelled.
func Run(ctx context.Context, a *app.App, version string) error {
	return RunWith(ctx, a, Options{Version: version})
}

// RunWith is Run with hooks and flags.
func RunWith(ctx context.Context, a *app.App, o Options) error {
	m := newModel(ctx, a, o)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	if o.ProgramReady != nil {
		o.ProgramReady(p)
	}
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// --- messages ---------------------------------------------------------------

type evMsg struct{ app.Event }
type animTickMsg time.Time
type slowTickMsg time.Time
type bannerTickMsg struct{}
type debounceMsg struct{ seq int }
type availMsg struct {
	seq       int
	available bool
	err       error
}
type resultMsg struct {
	lines []string
	err   error
	conv  string // conversation the lines belong to ("" = any)
}
type convOpenedMsg struct {
	conv *app.Conversation
	err  error
}
type usersMsg struct {
	users []app.Friend
	err   error
}
type sentMsg struct {
	conv string
	err  error
}
type themeMsg struct {
	th  theme.Theme
	err error
}
type membersMsg struct {
	gid     string
	members []app.Friend
	err     error
}
type quitMsg struct{}

// --- model ------------------------------------------------------------------

type pane int

const (
	paneLeft pane = iota
	paneChat
)

type availState int

const (
	availNone availState = iota
	availChecking
	availOK
	availTaken
	availInvalid
	availError
)

type note struct {
	At   int64
	Text string
	Conv string
	Warn bool
}

type pendingMsg struct {
	Conv string
	Text string
	At   int64
}

type ui struct {
	ctx     context.Context
	app     *app.App
	hooks   Hooks
	th      theme.Theme
	version string
	demo    bool
	reduced bool

	width, height int
	phase         app.Phase
	status        string // transient status-bar message
	statusWarn    bool
	statusUntil   time.Time

	// login flow
	bannerShown int // lines of the banner revealed so far
	email       textinput.Model
	otp         textinput.Model
	user        textinput.Model
	invite      textinput.Model
	regFocus    int // 0 username, 1 invite
	avail       availState
	availSeq    int
	errLine     string
	busy        bool
	spin        spinner.Model

	// key forging
	forge *forgeAnim

	// main layout
	pane         pane
	cursor       int // left pane cursor
	everyone     bool
	allUsers     []app.Friend
	active       string // active conversation id
	vp           viewport.Model
	input        textinput.Model
	sugs         []Suggestion // completion strip above the input
	sugIdx       int
	sugBase      string // line that started a tab-completion cycle
	announcement string
	notes        []note
	pending      []pendingMsg
	cards        []*card
	pulses       map[string]time.Time
	springs      map[string]*spring
	groupMembers map[string]map[string]bool // group id -> member ids (fetched lazily)
	divider      string // message id the "── new ──" divider sits before
	typingFrame  int
	typing       []string
	disconnected bool
	tickPending  bool

	// modal-ish states
	requests  bool
	verify    *verifyState
	sudoAsk   *sudoPrompt
	masked    textinput.Model
	lastWidth int
}

type verifyState struct {
	name, mine, theirs string
}

type sudoPrompt struct {
	name  string
	scope model.GrantScope
	// Generic masked prompt: when onSubmit is set it replaces the grant flow.
	label    string
	onSubmit func(pw string) tea.Cmd
	onCancel func()
}

// askMasked opens the masked prompt with a label and callbacks.
func (m *ui) askMasked(label string, onSubmit func(pw string) tea.Cmd, onCancel func()) {
	m.sudoAsk = &sudoPrompt{label: label, onSubmit: onSubmit, onCancel: onCancel}
	m.input.Blur()
	m.masked.Focus()
	m.layout()
}

func newModel(ctx context.Context, a *app.App, o Options) *ui {
	prefs := a.Store().Prefs()
	th, _ := theme.Load(prefs.Theme, a.Store().Paths.Config)
	m := &ui{
		ctx:     ctx,
		app:     a,
		hooks:   o.Hooks,
		th:      th,
		version: o.Version,
		demo:    o.Demo,
		reduced: prefs.ReducedMotion || o.ReducedMotion,
		width:   80,
		height:  24,
		phase:   a.Phase(),
		pulses:  map[string]time.Time{},
		springs: map[string]*spring{},
		groupMembers: map[string]map[string]bool{},
	}
	m.email = newInput("you@example.com", 64)
	m.otp = newInput("123456", 6)
	m.user = newInput("username", 20)
	m.invite = newInput("invite code", 64)
	m.input = newInput("message or /command", 8000)
	m.input.Prompt = "> "
	m.masked = newInput("sudo password", 256)
	m.masked.EchoMode = textinput.EchoPassword
	m.spin = spinner.New()
	m.spin.Spinner = spinner.Points
	m.spin.Style = th.Primary
	m.vp = viewport.New(50, 10)
	if m.reduced {
		m.bannerShown = len(bannerLines)
	}
	m.applyTheme(th)
	m.focusForPhase()
	return m
}

// maxSuggestions caps the completion strip height.
const maxSuggestions = 6

func newInput(placeholder string, limit int) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = limit
	ti.Prompt = "> "
	return ti
}

func (m *ui) applyTheme(th theme.Theme) {
	m.th = th
	m.spin.Style = th.Primary
	for _, ti := range []*textinput.Model{&m.email, &m.otp, &m.user, &m.invite, &m.input, &m.masked} {
		ti.PromptStyle = th.Primary
		ti.TextStyle = th.Input
		ti.PlaceholderStyle = th.Dim
		ti.Cursor.Style = th.Primary
	}
}

// focusForPhase puts the keyboard focus on the right input.
func (m *ui) focusForPhase() {
	m.email.Blur()
	m.otp.Blur()
	m.user.Blur()
	m.invite.Blur()
	m.input.Blur()
	switch m.phase {
	case app.PhaseNeedLogin:
		m.email.Focus()
	case app.PhaseNeedOTP:
		m.otp.Focus()
	case app.PhaseNeedUsername:
		if m.regFocus == 1 {
			m.invite.Focus()
		} else {
			m.user.Focus()
		}
	case app.PhaseReady:
		if m.pane == paneChat {
			m.input.Focus()
		}
	}
}

// --- tea.Model --------------------------------------------------------------

func (m *ui) Init() tea.Cmd {
	cmds := []tea.Cmd{m.waitEvent(), textinput.Blink, m.slowTick()}
	if m.bannerShown < len(bannerLines) {
		cmds = append(cmds, bannerTick())
	}
	if m.phase == app.PhasePendingApproval {
		cmds = append(cmds, m.spin.Tick)
	}
	if m.phase == app.PhaseReady {
		cmds = append(cmds, m.enterReady())
	}
	return tea.Batch(cmds...)
}

// waitEvent pumps one app event into the program.
func (m *ui) waitEvent() tea.Cmd {
	ch := m.app.Events()
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case e, ok := <-ch:
			if !ok {
				return quitMsg{}
			}
			return evMsg{e}
		case <-ctx.Done():
			return quitMsg{}
		}
	}
}

func bannerTick() tea.Cmd {
	return tea.Tick(30*time.Millisecond, func(time.Time) tea.Msg { return bannerTickMsg{} })
}

func (m *ui) slowTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return slowTickMsg(t) })
}

// animTick schedules the fast (30fps) animation tick if one is not already
// pending and something is animating.
func (m *ui) animTick() tea.Cmd {
	if m.reduced || m.tickPending || !m.animating() {
		return nil
	}
	m.tickPending = true
	return tea.Tick(33*time.Millisecond, func(t time.Time) tea.Msg { return animTickMsg(t) })
}

func (m *ui) animating() bool {
	if len(m.springs) > 0 || m.forge != nil && !m.forge.finished() {
		return true
	}
	now := time.Now()
	for _, t := range m.pulses {
		if now.Sub(t) < pulseDur {
			return true
		}
	}
	for _, c := range m.cards {
		if c.state != cardIdle {
			return true
		}
	}
	return false
}

func (m *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case quitMsg:
		return m, tea.Quit
	case evMsg:
		cmd := m.onEvent(msg.Event)
		return m, tea.Batch(cmd, m.waitEvent(), m.animTick())
	case bannerTickMsg:
		if m.bannerShown < len(bannerLines) {
			m.bannerShown++
			if m.bannerShown < len(bannerLines) {
				return m, bannerTick()
			}
		}
		return m, nil
	case animTickMsg:
		m.tickPending = false
		m.stepAnimations()
		return m, m.animTick()
	case slowTickMsg:
		m.typingFrame++
		if m.phase == app.PhaseReady && m.active != "" {
			m.typing = m.app.TypingIn(m.active)
			for i, id := range m.typing {
				m.typing[i] = m.app.Name(id)
			}
		}
		m.expireCards()
		if !m.statusUntil.IsZero() && time.Now().After(m.statusUntil) {
			m.status = ""
		}
		return m, m.slowTick()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.phase == app.PhasePendingApproval || m.busy {
			return m, cmd
		}
		return m, nil
	case debounceMsg:
		if msg.seq != m.availSeq || m.phase != app.PhaseNeedUsername {
			return m, nil
		}
		return m, m.checkUsername(msg.seq)
	case availMsg:
		if msg.seq != m.availSeq {
			return m, nil
		}
		switch {
		case msg.err != nil:
			m.avail = availError
			m.errLine = msg.err.Error()
		case msg.available:
			m.avail = availOK
		default:
			m.avail = availTaken
		}
		return m, nil
	case regRetryMsg:
		m.busy = false
		switch {
		case msg.err != nil:
			m.errLine = msg.err.Error()
		case msg.open:
			m.phase = app.PhaseNeedUsername
			m.focusForPhase()
		default:
			m.errLine = "still closed"
		}
		return m, nil
	case resultMsg:
		m.busy = false
		if msg.err != nil {
			m.addNote(msg.conv, msg.err.Error(), true)
			if m.phase != app.PhaseReady {
				m.errLine = msg.err.Error()
			}
		}
		if len(msg.lines) > 0 {
			m.addNote(msg.conv, strings.Join(msg.lines, "\n"), false)
		}
		m.renderChat(true)
		return m, nil
	case sentMsg:
		m.dropPending(msg.conv)
		if msg.err != nil {
			m.addNote(msg.conv, msg.err.Error(), true)
		}
		m.renderChat(true)
		return m, nil
	case convOpenedMsg:
		if msg.err != nil {
			m.addNote("", msg.err.Error(), true)
			m.renderChat(true)
			return m, nil
		}
		m.openConv(msg.conv)
		return m, nil
	case membersMsg:
		if msg.err == nil {
			set := map[string]bool{}
			for _, f := range msg.members {
				set[f.User.ID] = true
			}
			m.groupMembers[msg.gid] = set
		}
		return m, nil
	case verifyMsg:
		if msg.err != nil {
			m.addNote("", msg.err.Error(), true)
			m.renderChat(true)
			return m, nil
		}
		m.verify = &verifyState{name: msg.name, mine: msg.mine, theirs: msg.theirs}
		m.layout()
		return m, nil
	case usersMsg:
		if msg.err != nil {
			m.addNote("", msg.err.Error(), true)
		} else {
			m.allUsers = msg.users
		}
		return m, nil
	case themeMsg:
		if msg.err != nil {
			m.addNote("", msg.err.Error(), true)
		} else {
			m.applyTheme(msg.th)
			p := m.app.Store().Prefs()
			p.Theme = msg.th.Name
			_ = m.app.Store().SavePrefs(p)
			m.addNote("", "theme: "+msg.th.Name, false)
		}
		m.renderChat(true)
		return m, nil
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	// Everything else (cursor blink etc.) goes to the focused input.
	return m, m.updateInputs(msg)
}

func (m *ui) updateInputs(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd
	switch m.phase {
	case app.PhaseNeedLogin:
		m.email, cmd = m.email.Update(msg)
		cmds = append(cmds, cmd)
	case app.PhaseNeedOTP:
		m.otp, cmd = m.otp.Update(msg)
		cmds = append(cmds, cmd)
	case app.PhaseNeedUsername:
		m.user, cmd = m.user.Update(msg)
		cmds = append(cmds, cmd)
		m.invite, cmd = m.invite.Update(msg)
		cmds = append(cmds, cmd)
	case app.PhaseReady:
		if m.sudoAsk != nil {
			m.masked, cmd = m.masked.Update(msg)
		} else {
			m.input, cmd = m.input.Update(msg)
		}
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// --- events -----------------------------------------------------------------

func (m *ui) onEvent(e app.Event) tea.Cmd {
	switch e.Kind {
	case app.EvPhase:
		old := m.phase
		m.phase = e.Phase
		m.errLine = ""
		m.busy = false
		m.focusForPhase()
		var cmds []tea.Cmd
		if e.Phase == app.PhasePendingApproval {
			cmds = append(cmds, m.spin.Tick)
		}
		if e.Phase == app.PhaseReady && old != app.PhaseReady {
			cmds = append(cmds, m.enterReady())
		}
		return tea.Batch(cmds...)
	case app.EvKeyForged:
		m.forge = newForge(e.Text, m.reduced)
		m.addNote("", "device key forged: "+e.Text, false)
		return nil
	case app.EvMessage:
		if e.Msg == nil {
			return nil
		}
		m.onMessage(*e.Msg)
		return nil
	case app.EvFriendRequest:
		name := m.app.Name(e.UserID)
		m.addCard(&card{Card: app.Card{
			ID: "friend:" + e.UserID, Kind: "friend", From: e.UserID, FromName: name,
			Text: name + " wants to be your friend",
			Reply: func(accept bool, _ model.SudoTier) {
				_ = m.app.RespondFriend(m.ctx, name, accept)
			},
		}})
		return nil
	case app.EvFriendAccepted:
		m.addNote("", m.app.Name(e.UserID)+" is now your friend", false)
		m.removeCard("friend:" + e.UserID)
		m.renderChat(true)
		return nil
	case app.EvFriendRemoved:
		m.addNote("", m.app.Name(e.UserID)+" is no longer your friend", false)
		m.removeCard("friend:" + e.UserID)
		m.renderChat(true)
		return nil
	case app.EvPresence:
		if e.Online {
			m.pulses[e.UserID] = time.Now()
		}
		if m.everyone {
			for i := range m.allUsers {
				if m.allUsers[i].User.ID == e.UserID {
					m.allUsers[i].Online = e.Online
				}
			}
		}
		return nil
	case app.EvGroupChanged:
		if m.active == e.GroupID && m.app.Conv(e.GroupID) == nil {
			m.active = ""
			m.addNote("", "that group is gone", false)
		}
		m.renderChat(true)
		delete(m.groupMembers, e.GroupID)
		return m.fetchMembers(e.GroupID)
	case app.EvGroupInvite:
		m.addNote("", "you were added to #"+e.Text, false)
		m.renderChat(true)
		return nil
	case app.EvGrantChanged:
		if e.Text != "" {
			m.addNote("", e.Text, true)
			m.renderChat(true)
		}
		return nil
	case app.EvSettings:
		return nil
	case app.EvAnnouncement:
		m.announcement = e.Text
		m.layout()
		return nil
	case app.EvKeyChanged:
		name := e.Text
		m.addCard(&card{Card: app.Card{
			ID: "keychange:" + e.UserID, Kind: "keychange", From: e.UserID, FromName: name,
			Text: name + "'s device key changed. [a]ccept new key / [d]ismiss",
			Reply: func(accept bool, _ model.SudoTier) {
				if accept {
					_ = m.app.AcceptKeyChange(m.ctx, name)
				}
			},
		}})
		return nil
	case app.EvNotice:
		m.addNote("", e.Text, false)
		m.renderChat(true)
		return nil
	case app.EvError:
		text := e.Text
		if e.Err != nil {
			text = e.Err.Error()
		}
		m.addNote("", text, true)
		m.setStatus(text, true)
		m.renderChat(true)
		return nil
	case app.EvTyping:
		if e.Text == m.active {
			m.typing = m.app.TypingIn(m.active)
			for i, id := range m.typing {
				m.typing[i] = m.app.Name(id)
			}
		}
		return nil
	case app.EvCard:
		if e.Card != nil {
			c := *e.Card
			m.addCard(&card{Card: c})
		}
		return nil
	case app.EvSession:
		if e.Session != nil {
			s := e.Session
			line := fmt.Sprintf("session %s with %s: %s", s.Kind, s.PeerName, s.State)
			if s.Error != "" {
				line += " (" + s.Error + ")"
			}
			m.addNote("", line, s.Error != "")
			m.renderChat(true)
		}
		return nil
	case app.EvDisconnected:
		m.disconnected = true
		m.setStatus("disconnected, reconnecting…", true)
		return nil
	case app.EvReconnected:
		m.disconnected = false
		m.setStatus("reconnected", false)
		return nil
	case app.EvKicked:
		m.addNote("", "you were signed out by the admin; restart pairdesk to log in again", true)
		m.renderChat(true)
		return nil
	case app.EvBanned:
		m.phase = app.PhaseBanned
		return nil
	}
	return nil
}

// enterReady runs once when the main screen appears.
func (m *ui) enterReady() tea.Cmd {
	if m.announcement == "" {
		m.announcement = m.app.Settings().Announcement
	}
	m.pane = paneChat
	m.input.Focus()
	m.layout()
	if m.active == "" {
		if cs := m.app.Conversations(); len(cs) > 0 {
			m.openConv(cs[0])
		}
	}
	if len(m.app.Friends()) == 0 && m.active == "" {
		m.addNote("", "no friends yet. /add <username> to send a request, /help for everything else", false)
	}
	m.renderChat(true)
	var cmds []tea.Cmd
	for _, g := range m.app.Groups() {
		cmds = append(cmds, m.fetchMembers(g.Group.ID))
	}
	return tea.Batch(cmds...)
}

// fetchMembers caches a group's member set for the online count.
func (m *ui) fetchMembers(gid string) tea.Cmd {
	g, ok := m.groupByID(gid)
	if !ok {
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ms, err := m.app.GroupMembers(cctx, g)
		return membersMsg{gid: gid, members: ms, err: err}
	}
}

func (m *ui) groupByID(gid string) (model.Group, bool) {
	for _, g := range m.app.Groups() {
		if g.Group.ID == gid {
			return g.Group, true
		}
	}
	return model.Group{}, false
}

// --- notes, status, pending ------------------------------------------------

func (m *ui) addNote(conv, text string, warn bool) {
	if conv == "" {
		conv = m.active
	}
	m.notes = append(m.notes, note{At: time.Now().UnixNano(), Text: text, Conv: conv, Warn: warn})
	if len(m.notes) > 500 {
		m.notes = m.notes[len(m.notes)-500:]
	}
}

func (m *ui) setStatus(text string, warn bool) {
	m.status = text
	m.statusWarn = warn
	m.statusUntil = time.Now().Add(5 * time.Second)
}

func (m *ui) dropPending(conv string) {
	keep := m.pending[:0]
	for _, p := range m.pending {
		if p.Conv != conv {
			keep = append(keep, p)
		}
	}
	m.pending = keep
}

// --- quitting ----------------------------------------------------------------

func (m *ui) quit() tea.Cmd {
	m.app.Close()
	return tea.Quit
}

// --- view --------------------------------------------------------------------

func (m *ui) View() string {
	if m.width < 80 || m.height < 24 {
		return m.th.Warn.Render(fmt.Sprintf("pairdesk needs at least 80x24 (you have %dx%d)", m.width, m.height))
	}
	switch m.phase {
	case app.PhaseReady:
		return m.viewMain()
	case app.PhaseNeedLogin:
		return m.viewLogin()
	case app.PhaseNeedOTP:
		return m.viewOTP()
	case app.PhaseRegClosed:
		return m.viewWall("registration is closed right now",
			"the admin has closed registration. try again later.",
			"r retry · q quit")
	case app.PhaseNeedUsername:
		return m.viewUsername()
	case app.PhasePendingApproval:
		return m.viewPending()
	case app.PhaseBanned:
		return m.viewWall("you have been banned", "this account cannot use this instance.", "q quit")
	case app.PhaseMaintenance:
		msg := m.app.Settings().MaintenanceMsg
		if msg == "" {
			msg = "back soon."
		}
		return m.viewWall("maintenance", msg, "q quit")
	case app.PhaseUpdateRequired:
		return m.viewWall("update required",
			fmt.Sprintf("this instance needs pairdesk %s or newer (you have %s). run pairdesk --update.", m.app.Settings().MinVersion, m.version),
			"q quit")
	}
	return m.th.Dim.Render("starting…")
}

// centered places lines in the middle of the screen.
func (m *ui) centered(lines ...string) string {
	body := strings.Join(lines, "\n")
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, body)
}
