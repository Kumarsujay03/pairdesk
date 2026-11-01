package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/Kumarsujay03/pairdesk/internal/app"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

const leftWidth = 24

// layout recomputes viewport and input sizes from the terminal size.
func (m *ui) layout() {
	w, h := m.width, m.height
	if w < 80 {
		w = 80
	}
	if h < 24 {
		h = 24
	}
	body := h - 1 // status bar
	if m.announcement != "" {
		body--
	}
	right := w - leftWidth
	inner := right - 2 // borders
	m.input.Width = inner - 3
	m.masked.Width = inner - 3
	extra := 1 + 1 + len(m.sugs) // typing line + input line + suggestions
	if cs := m.renderCards(inner); cs != "" {
		extra += lipgloss.Height(cs)
	}
	if mb := m.modalBlock(inner); mb != "" {
		extra += lipgloss.Height(mb)
	}
	vh := body - 2 - extra
	if vh < 3 {
		vh = 3
	}
	atBottom := m.vp.AtBottom()
	m.vp.Width = inner
	m.vp.Height = vh
	if atBottom {
		m.vp.GotoBottom()
	}
	if m.lastWidth != inner {
		m.lastWidth = inner
		m.renderChat(atBottom)
	}
}

// openConv switches the active conversation.
func (m *ui) openConv(c *app.Conversation) {
	if c == nil {
		return
	}
	m.active = c.ID
	m.divider = ""
	if msgs, unread := m.app.Snapshot(c); unread > 0 && unread <= len(msgs) {
		m.divider = msgs[len(msgs)-unread].ID
	}
	m.app.MarkRead(c)
	m.renderChat(true)
	m.vp.GotoBottom()
	m.typing = nil
	m.pane = paneChat
	m.input.Focus()
}

// onMessage handles a new decrypted message.
func (m *ui) onMessage(msg app.Msg) {
	if msg.Mine {
		m.dropPending(msg.ConvID)
	}
	if msg.ConvID != m.active {
		if m.active == "" {
			if c := m.app.Conv(msg.ConvID); c != nil {
				m.openConv(c)
			}
		}
		return
	}
	wasBottom := m.vp.AtBottom()
	if !msg.Mine && !wasBottom && m.divider == "" {
		m.divider = msg.ID
	}
	if !m.reduced && msg.Kind != "reaction" {
		m.springs[msg.ID] = &spring{pos: 10}
	}
	if wasBottom || msg.Mine {
		if c := m.app.Conv(m.active); c != nil {
			m.app.MarkRead(c)
		}
	}
	m.renderChat(wasBottom || msg.Mine)
}

// stepAnimations advances springs and drops finished pulses.
func (m *ui) stepAnimations() {
	changed := false
	for id, s := range m.springs {
		if s.step() {
			delete(m.springs, id)
		}
		changed = true
	}
	now := time.Now()
	for id, t := range m.pulses {
		if now.Sub(t) > pulseDur {
			delete(m.pulses, id)
		}
	}
	for _, c := range m.cards {
		if c.slide != nil && c.slide.step() {
			c.slide = nil
		}
	}
	if m.forge != nil && m.forge.expired() {
		m.forge = nil
	}
	if changed {
		m.renderChat(m.vp.AtBottom())
	}
}

// --- chat rendering -----------------------------------------------------------

type chatLine struct {
	at   int64
	text string
}

// renderChat rebuilds the viewport content for the active conversation.
func (m *ui) renderChat(gotoBottom bool) {
	width := m.vp.Width
	if width < 20 {
		width = 20
	}
	var lines []chatLine
	var msgs []app.Msg
	c := m.app.Conv(m.active)
	if c != nil {
		msgs, _ = m.app.Snapshot(c)
	}
	reactions := map[string][]app.Msg{}
	for _, mm := range msgs {
		if mm.Kind == "reaction" && mm.ReactsTo != "" {
			reactions[mm.ReactsTo] = append(reactions[mm.ReactsTo], mm)
		}
	}
	for _, mm := range msgs {
		if mm.Kind == "reaction" {
			continue
		}
		var b strings.Builder
		if mm.ID == m.divider && m.divider != "" {
			b.WriteString(m.th.Warn.Render(divider("new", width)))
			b.WriteString("\n")
		}
		b.WriteString(m.formatMsg(mm, width))
		for _, r := range reactions[mm.ID] {
			b.WriteString("\n")
			b.WriteString(m.th.Dim.Render(" ↳ ") + r.Text + " " + m.th.Dim.Render(m.senderName(r)))
		}
		text := b.String()
		if s := m.springs[mm.ID]; s != nil {
			text = indent(text, s.offset())
		}
		lines = append(lines, chatLine{at: mm.At, text: text})
	}
	for _, n := range m.notes {
		if n.Conv != "" && n.Conv != m.active {
			continue
		}
		st := m.th.Dim
		if n.Warn {
			st = m.th.Warn
		}
		ts := time.Unix(0, n.At).Format("15:04")
		body := lipgloss.NewStyle().Width(width - 7).Render(n.Text)
		lines = append(lines, chatLine{at: n.At, text: m.th.Dim.Render(ts) + "  " + st.Render(strings.ReplaceAll(body, "\n", "\n       "))})
	}
	for _, p := range m.pending {
		if p.Conv != m.active {
			continue
		}
		ts := time.Unix(0, p.At).Format("15:04")
		lines = append(lines, chatLine{at: p.At, text: m.th.Dim.Render(ts + "  you   " + p.Text)})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at < lines[j].at })
	var out []string
	for _, l := range lines {
		out = append(out, l.text)
	}
	if len(out) == 0 {
		if m.active == "" {
			out = append(out, m.th.Dim.Render("no conversation open. /dm <user> or pick one on the left (tab, then enter)."))
		} else {
			out = append(out, m.th.Dim.Render("nothing here yet. say hi."))
		}
	}
	m.vp.SetContent(strings.Join(out, "\n"))
	if gotoBottom {
		m.vp.GotoBottom()
	}
}

func divider(label string, width int) string {
	side := (width - len(label) - 2) / 2
	if side < 2 {
		side = 2
	}
	return strings.Repeat("─", side) + " " + label + " " + strings.Repeat("─", side)
}

func (m *ui) senderName(mm app.Msg) string {
	if mm.Mine {
		return "you"
	}
	return m.app.Name(mm.Sender)
}

// formatMsg renders one message as "HH:MM  name   text", wrapped.
func (m *ui) formatMsg(mm app.Msg, width int) string {
	ts := time.Unix(0, mm.At).Format("15:04")
	name := m.senderName(mm)
	dim := mm.Mine && !mm.Decrypted
	nameStyle := m.th.Accent
	if mm.Mine {
		nameStyle = m.th.Primary
	}
	if dim {
		nameStyle = m.th.Dim
	}
	switch mm.Kind {
	case "me":
		head := m.th.Dim.Render(ts) + "  " + m.th.Dim.Render("*") + " " + nameStyle.Render(name) + " "
		body := wrapTo(mm.Text, width-lipgloss.Width(head), lipgloss.Width(head))
		return head + m.th.Italic.Render(body)
	case "system":
		return m.th.Dim.Render(ts) + "  " + m.th.Dim.Render(wrapTo(mm.Text, width-7, 7))
	case "file":
		head := m.th.Dim.Render(ts) + "  " + nameStyle.Render(name) + "   "
		return head + m.th.Dim.Render("📎 ") + wrapTo(mm.Text, width-lipgloss.Width(head)-3, lipgloss.Width(head)+3)
	}
	head := m.th.Dim.Render(ts) + "  " + nameStyle.Render(name) + "   "
	body := wrapTo(mm.Text, width-lipgloss.Width(head), lipgloss.Width(head))
	if dim {
		body = m.th.Dim.Render(body)
	} else {
		body = m.inlineCode(body)
	}
	return head + body
}

// inlineCode tints `code` spans.
func (m *ui) inlineCode(s string) string {
	if !strings.Contains(s, "`") {
		return s
	}
	parts := strings.Split(s, "`")
	if len(parts) < 3 {
		return s
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			b.WriteString(m.th.Accent.Render(p))
		} else {
			b.WriteString(p)
		}
	}
	return b.String()
}

// wrapTo hard-wraps text to width and indents continuation lines.
func wrapTo(text string, width, indentN int) string {
	if width < 10 {
		width = 10
	}
	wrapped := ansi.Wrap(text, width, "")
	return strings.ReplaceAll(wrapped, "\n", "\n"+strings.Repeat(" ", indentN))
}

// --- left pane ---------------------------------------------------------------

type leftItem struct {
	label string
	conv  string // conversation id to open ("" = header)
	user  string // user id for pulses
	group model.Group
	isGrp bool
}

func (m *ui) leftItems() []leftItem {
	var items []leftItem
	if m.everyone && m.app.IsAdmin() {
		for _, u := range m.allUsers {
			if u.User.ID == m.app.Me().ID {
				continue
			}
			items = append(items, leftItem{label: u.User.Username, user: u.User.ID, conv: model.DMConvID(m.app.Me().ID, u.User.ID)})
		}
	} else {
		for _, f := range m.app.Friends() {
			items = append(items, leftItem{label: f.User.Username, user: f.User.ID, conv: model.DMConvID(m.app.Me().ID, f.User.ID)})
		}
	}
	for _, g := range m.app.Groups() {
		items = append(items, leftItem{label: g.Group.Name, conv: g.Group.ID, group: g.Group, isGrp: true})
	}
	return items
}

func (m *ui) dot(userID string, online bool) string {
	if !online {
		return m.th.Offline.Render("○")
	}
	if t, ok := m.pulses[userID]; ok && time.Since(t) < pulseDur && !m.reduced {
		return m.th.Pulse.Render("●")
	}
	return m.th.Online.Render("●")
}

func (m *ui) viewLeft(height int) string {
	inner := leftWidth - 2
	var lines []string
	items := m.leftItems()
	idx := 0
	title := "friends"
	if m.everyone && m.app.IsAdmin() {
		title = fmt.Sprintf("everyone (%d)", len(m.allUsers))
	}
	users := 0
	for _, it := range items {
		if !it.isGrp {
			users++
		}
	}
	if users == 0 {
		lines = append(lines, m.th.Dim.Render("  nobody yet"))
	}
	for _, it := range items {
		if it.isGrp {
			continue
		}
		online := m.app.Online(it.user)
		if m.everyone {
			for _, u := range m.allUsers {
				if u.User.ID == it.user {
					online = u.Online
				}
			}
		}
		label := it.label
		var tag string
		if m.everyone {
			for _, u := range m.allUsers {
				if u.User.ID == it.user && u.User.Banned {
					tag = "banned"
				}
			}
		}
		if n := m.app.Unread(m.app.Conv(it.conv)); n > 0 {
			tag = fmt.Sprintf("(%d)", n)
		}
		lines = append(lines, m.leftRow(idx, m.dot(it.user, online), label, tag, it.conv == m.active, inner))
		idx++
	}
	lines = append(lines, "", m.th.Title.Render("groups"))
	grpCount := 0
	for _, it := range items {
		if !it.isGrp {
			continue
		}
		grpCount++
		online := m.groupOnline(it.group)
		label := fmt.Sprintf("#%s", it.label)
		tag := fmt.Sprintf("(%d●)", online)
		if n := m.app.Unread(m.app.Conv(it.conv)); n > 0 {
			tag += fmt.Sprintf(" %d", n)
		}
		lines = append(lines, m.leftRow(idx, " ", label, tag, it.conv == m.active, inner))
		idx++
	}
	if grpCount == 0 {
		lines = append(lines, m.th.Dim.Render("  /group create"))
	}
	style := m.th.Border
	if m.pane == paneLeft {
		style = m.th.Focused
	}
	return boxed(style, title, strings.Join(lines, "\n"), leftWidth, height, m)
}

func (m *ui) leftRow(idx int, dot, label, tag string, active bool, inner int) string {
	cur := " "
	if m.pane == paneLeft && idx == m.cursor {
		cur = m.th.Primary.Render(">")
	}
	lab := label
	if active {
		lab = m.th.Primary.Bold(true).Render(label)
	}
	line := cur + dot + " " + lab
	if tag != "" {
		gap := inner - lipgloss.Width(line) - lipgloss.Width(tag)
		if gap < 1 {
			gap = 1
		}
		line += strings.Repeat(" ", gap) + m.th.Dim.Render(tag)
	}
	return ansi.Truncate(line, inner, "")
}

// groupOnline counts online members we know about (friends + admin cache).
func (m *ui) groupOnline(g model.Group) int {
	n := 0
	for _, f := range m.app.Friends() {
		if f.Online && m.memberOf(g, f.User.ID) {
			n++
		}
	}
	return n
}

// memberOf is a best-effort check without a network call: the app caches
// members only through GroupMembers, so we count friends who are online and
// whose group membership we cached via /group members. Fall back to "unknown"
// = count everyone online (the admin sees all).
func (m *ui) memberOf(g model.Group, uid string) bool {
	if set, ok := m.groupMembers[g.ID]; ok {
		return set[uid]
	}
	return true
}

func boxed(style lipgloss.Style, title, content string, width, height int, m *ui) string {
	inner := width - 2
	top := style.GetBorderTopForeground()
	tb := lipgloss.NewStyle().Foreground(top)
	t := " " + title + " "
	if lipgloss.Width(t) > inner-2 {
		t = ansi.Truncate(t, inner-2, "…")
	}
	topLine := tb.Render("╭") + tb.Render("─") + m.th.Title.Render(t) + tb.Render(strings.Repeat("─", inner-1-lipgloss.Width(t))) + tb.Render("╮")
	body := style.BorderTop(false).Width(inner).Height(height - 2).MaxHeight(height - 1).Render(content)
	return topLine + "\n" + body
}

// --- right pane ---------------------------------------------------------------

func (m *ui) convTitle() string {
	c := m.app.Conv(m.active)
	if c == nil {
		return "pairdesk"
	}
	if c.Type == model.ConvGroup {
		return m.app.ConvTitle(c)
	}
	return "dm · " + m.app.ConvTitle(c)
}

func (m *ui) typingLine() string {
	if len(m.typing) == 0 || m.active == "" {
		return ""
	}
	who := strings.Join(m.typing, ", ")
	verb := " is typing"
	if len(m.typing) > 1 {
		verb = " are typing"
	}
	dots := "…"
	if !m.reduced {
		dots = typingDots(m.typingFrame)
	}
	return m.th.Dim.Render(who + verb + dots)
}

// modalBlock renders the /requests list, /verify prompt or sudo password
// prompt, if any.
func (m *ui) modalBlock(width int) string {
	switch {
	case m.sudoAsk != nil:
		label := "sudo password for " + m.sudoAsk.name + " (kept in your keyring, never shown to them)"
		if m.sudoAsk.label != "" {
			label = m.sudoAsk.label
		}
		return m.th.Warn.Render(label) + "\n" + m.masked.View() + "\n" + m.th.Dim.Render("enter · esc cancel")
	case m.verify != nil:
		v := m.verify
		return strings.Join([]string{
			m.th.Title.Render("verify " + v.name),
			m.th.Dim.Render("yours:  ") + m.th.Accent.Bold(true).Render(v.mine),
			m.th.Dim.Render("theirs: ") + m.th.Accent.Bold(true).Render(v.theirs),
			m.th.Dim.Render("compare out of band (call them). [y] they match · esc cancel"),
		}, "\n")
	case m.requests:
		reqs := m.app.PendingRequests()
		if len(reqs) == 0 {
			return m.th.Dim.Render("no pending friend requests · esc close")
		}
		lines := []string{m.th.Title.Render(fmt.Sprintf("pending requests (%d)", len(reqs)))}
		for i, u := range reqs {
			mark := "  "
			if i == 0 {
				mark = m.th.Primary.Render("> ")
			}
			lines = append(lines, mark+u.Username)
		}
		lines = append(lines, m.th.Dim.Render("[a]ccept · [d]eny the first · esc close"))
		return strings.Join(lines, "\n")
	}
	return ""
}

func (m *ui) viewRight(height int) string {
	right := m.width - leftWidth
	inner := right - 2
	var parts []string
	parts = append(parts, m.vp.View())
	if cs := m.renderCards(inner); cs != "" {
		parts = append(parts, cs)
	}
	if mb := m.modalBlock(inner); mb != "" {
		parts = append(parts, mb)
	}
	tl := m.typingLine()
	if tl == "" {
		tl = " "
	}
	parts = append(parts, tl)
	for i, sg := range m.sugs {
		line := ansi.Truncate(fmt.Sprintf("%-40s %s", sg.Usage, sg.Desc), inner-2, "…")
		if i == m.sugIdx {
			parts = append(parts, m.th.Accent.Render("▸ "+line))
		} else {
			parts = append(parts, m.th.Dim.Render("  "+line))
		}
	}
	if m.sudoAsk == nil {
		parts = append(parts, m.input.View())
	} else {
		parts = append(parts, m.th.Dim.Render("…"))
	}
	style := m.th.Border
	if m.pane == paneChat {
		style = m.th.Focused
	}
	return boxed(style, m.convTitle(), strings.Join(parts, "\n"), right, height, m)
}

func (m *ui) viewStatus() string {
	left := " " + m.th.Online.Render("●") + " online  " + m.th.Offline.Render("○") + " offline  ·  tab panes  ·  ctrl+c quit  ·  /help"
	if m.app.IsAdmin() {
		left += "  ·  /users everyone"
	}
	right := ""
	if m.status != "" {
		if m.statusWarn {
			right = m.th.Warn.Render(m.status)
		} else {
			right = m.th.OK.Render(m.status)
		}
	} else if m.disconnected {
		right = m.th.Warn.Render("offline")
	} else {
		right = m.th.Dim.Render(m.app.Me().Username + " · " + m.th.Name)
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
	}
	return m.th.StatusBar.Width(m.width).Render(left + strings.Repeat(" ", gap) + right)
}

func (m *ui) viewMain() string {
	var rows []string
	body := m.height - 1
	if m.announcement != "" {
		text := " " + "admin: " + m.announcement + "   (esc dismiss)"
		rows = append(rows, m.th.Announce.Width(m.width).Render(ansi.Truncate(text, m.width, "…")))
		body--
	}
	left := m.viewLeft(body)
	right := m.viewRight(body)
	rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, left, right))
	rows = append(rows, m.viewStatus())
	return strings.Join(rows, "\n")
}
