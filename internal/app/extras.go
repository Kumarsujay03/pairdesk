package app

import (
	"path/filepath"

	"github.com/Kumarsujay03/pairdesk/internal/notify"
	"github.com/Kumarsujay03/pairdesk/internal/search"
)

// initExtras opens the search index and notifier once the identity exists.
func (a *App) initExtras() {
	ix, err := search.Open(filepath.Join(a.st.Paths.State, "history"), a.id)
	if err != nil {
		a.log.Warn("search index", "err", err)
	} else {
		a.search = ix
	}
	a.notifier = notify.New(a.st.Prefs().Notifications)
}

// Search queries local decrypted history.
func (a *App) Search(q, convID string, limit int) ([]search.Hit, error) {
	if a.search == nil {
		return nil, nil
	}
	return a.search.Query(q, convID, limit)
}

// SetFocusCheck lets the UI tell the app whether a conversation is on
// screen, so notifications are only sent for things the user is not looking at.
func (a *App) SetFocusCheck(f func(convID string) bool) {
	a.mu.Lock()
	a.focused = f
	a.mu.Unlock()
}

// Notify shows an alert inside the client and, when desktop notifications
// are on (/notify on), as a desktop pop-up too.
func (a *App) Notify(key, title, body, urgency string) {
	a.emit(Event{Kind: EvNotice, Text: body})
	if a.notifier != nil {
		a.notifier.Send(key, title, body, urgency)
	}
}

// NotificationsOn reports whether desktop pop-ups are enabled.
func (a *App) NotificationsOn() bool { return a.st.Prefs().Notifications }

// SetNotifications toggles desktop notifications.
func (a *App) SetNotifications(on bool) error {
	p := a.st.Prefs()
	p.Notifications = on
	if a.notifier != nil {
		a.notifier.Enabled = on
	}
	return a.st.SavePrefs(p)
}

func (a *App) index(m Msg) {
	if a.search == nil || !m.Decrypted {
		return
	}
	_ = a.search.Append(search.Record{ID: m.ID, ConvType: m.ConvType, ConvID: m.ConvID, Sender: m.Sender, Text: m.Text, Kind: m.Kind, At: m.At})
}

func (a *App) maybeNotify(m Msg) {
	if m.Mine || a.notifier == nil || !a.notifier.Enabled {
		return
	}
	a.mu.RLock()
	f := a.focused
	a.mu.RUnlock()
	if f != nil && f(m.ConvID) {
		return
	}
	c := a.Conv(m.ConvID)
	title := a.Name(m.Sender)
	if c != nil && c.Type != "dm" {
		title = a.ConvTitle(c) + " · " + title
	}
	body := m.Text
	if len(body) > 120 {
		body = body[:117] + "..."
	}
	a.notifier.Send("msg:"+m.ConvID, title, body, "normal")
}
