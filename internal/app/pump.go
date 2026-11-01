package app

import (
	"context"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// SignalHandler receives P2P signaling; the session layer registers one.
type SignalHandler func(rendezvous.Signal)

// SetSignalHandler installs the session layer's handler.
func (a *App) SetSignalHandler(h SignalHandler) {
	a.mu.Lock()
	a.signalH = h
	a.mu.Unlock()
}

// pump translates backend events into app events.
func (a *App) pump(ch <-chan rendezvous.Event) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			a.handle(e)
		}
	}
}

func (a *App) handle(e rendezvous.Event) {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	switch e.Kind {
	case rendezvous.EvMessage:
		if e.Message != nil && !e.Deleted {
			a.ingest(*e.Message, false)
		}
	case rendezvous.EvFriendship:
		if e.Friendship == nil {
			return
		}
		f := *e.Friendship
		other := f.Other(a.me.ID)
		_ = a.cacheUsers(ctx, []string{other})
		a.mu.Lock()
		if e.Deleted {
			delete(a.friends, pairKey(f.Requester, f.Addressee))
		} else {
			a.friends[pairKey(f.Requester, f.Addressee)] = f
		}
		a.mu.Unlock()
		switch {
		case e.Deleted:
			a.emit(Event{Kind: EvFriendRemoved, UserID: other})
		case f.Status == model.FriendPending && f.Addressee == a.me.ID:
			a.emit(Event{Kind: EvFriendRequest, UserID: other})
		case f.Status == model.FriendAccepted:
			a.ensureConv(model.ConvDM, model.DMConvID(a.me.ID, other))
			_ = a.be.WatchPresence(ctx, []string{other})
			a.emit(Event{Kind: EvFriendAccepted, UserID: other})
		}
	case rendezvous.EvPresence:
		a.mu.Lock()
		a.online[e.UserID] = e.Online
		a.mu.Unlock()
		a.emit(Event{Kind: EvPresence, UserID: e.UserID, Online: e.Online})
	case rendezvous.EvGroup:
		if e.Group == nil {
			return
		}
		a.mu.Lock()
		_, known := a.groups[e.Group.ID]
		if e.Deleted {
			delete(a.groups, e.Group.ID)
			delete(a.groupKeys, e.Group.ID)
			delete(a.convs, e.Group.ID)
		} else {
			a.groups[e.Group.ID] = *e.Group
		}
		a.mu.Unlock()
		if !e.Deleted {
			_ = a.loadGroupKey(ctx, *e.Group)
			a.ensureConv(model.ConvGroup, e.Group.ID)
			if !known && e.Group.Owner != a.me.ID {
				a.emit(Event{Kind: EvGroupInvite, GroupID: e.Group.ID, Text: e.Group.Name})
			}
		}
		a.emit(Event{Kind: EvGroupChanged, GroupID: e.Group.ID})
	case rendezvous.EvGroupMember:
		if e.GroupMember == nil {
			return
		}
		m := *e.GroupMember
		if m.UserID == a.me.ID {
			if e.Deleted {
				a.mu.Lock()
				delete(a.groups, m.GroupID)
				delete(a.groupKeys, m.GroupID)
				delete(a.convs, m.GroupID)
				a.mu.Unlock()
				a.emit(Event{Kind: EvGroupChanged, GroupID: m.GroupID})
				return
			}
			// Added to a group or key rotated: fetch the group and unwrap.
			gs, err := a.be.ListGroups(ctx)
			if err == nil {
				for _, g := range gs {
					if g.ID == m.GroupID {
						a.mu.Lock()
						a.groups[g.ID] = g
						a.mu.Unlock()
						_ = a.loadGroupKey(ctx, g)
						a.ensureConv(model.ConvGroup, g.ID)
					}
				}
			}
		}
		a.emit(Event{Kind: EvGroupChanged, GroupID: m.GroupID})
	case rendezvous.EvGrant:
		a.emit(Event{Kind: EvGrantChanged})
	case rendezvous.EvSettings:
		if e.Settings == nil {
			return
		}
		a.mu.Lock()
		old := a.settings
		a.settings = *e.Settings
		a.mu.Unlock()
		a.checkAdminPin(*e.Settings)
		if e.Settings.AnnouncementID != old.AnnouncementID && e.Settings.Announcement != "" {
			a.emit(Event{Kind: EvAnnouncement, Text: e.Settings.Announcement})
		}
		if e.Settings.Maintenance && !a.IsAdmin() {
			a.setPhase(PhaseMaintenance)
		} else if !e.Settings.Maintenance && old.Maintenance && a.Phase() == PhaseMaintenance {
			a.setPhase(PhaseReady)
		}
		a.emit(Event{Kind: EvSettings})
	case rendezvous.EvUser:
		if e.User == nil || e.User.ID != a.me.ID {
			if e.User != nil {
				a.storeUser(*e.User)
			}
			return
		}
		// Realtime only delivers the columns a user may select, which hides
		// the moderation columns on our own row; fetch the full row instead.
		if fresh, err := a.fetchMe(); err == nil {
			e.User = &fresh
		}
		a.mu.Lock()
		old := a.me
		a.me = *e.User
		a.users[a.me.ID] = a.me
		a.mu.Unlock()
		switch {
		case e.User.Banned:
			a.setPhase(PhaseBanned)
			a.emit(Event{Kind: EvBanned})
		case !e.User.KickedAt.IsZero() && e.User.KickedAt.After(a.login.SavedAt):
			a.emit(Event{Kind: EvKicked})
		case e.User.PubKey == "" && old.PubKey != "":
			_ = a.st.DeleteIdentity()
			a.emit(Event{Kind: EvNotice, Text: "the admin reset your device key; restart pairdesk to forge a new one"})
		case e.User.Role != old.Role:
			a.emit(Event{Kind: EvNotice, Text: "your role is now " + string(e.User.Role)})
		}
	case rendezvous.EvSignal:
		a.mu.RLock()
		h := a.signalH
		a.mu.RUnlock()
		if h != nil && e.Signal != nil {
			h(*e.Signal)
		}
	case rendezvous.EvDisconnected:
		a.emit(Event{Kind: EvDisconnected, Err: e.Err})
	case rendezvous.EvReconnected:
		_ = a.be.SetOnline(ctx, true)
		a.emit(Event{Kind: EvReconnected})
	}
}

// fetchMe reads our own full row with a short timeout.
func (a *App) fetchMe() (model.User, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	return a.be.Me(ctx)
}
