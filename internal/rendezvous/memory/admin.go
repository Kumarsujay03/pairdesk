package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Every method in this file is admin only and appends to the insert-only
// admin log, reads included, so the log is a complete trail of /admin use.

// SetSetting updates one instance_settings key (its JSON name) and
// broadcasts the new Settings to everyone connected.
func (c *Client) SetSetting(_ context.Context, key string, value any) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	if err := applySetting(&s.settings, key, value); err != nil {
		return err
	}
	s.logAdmin(me, "setting", key, fmt.Sprint(value))
	settings := s.settings
	s.deliver(rendezvous.Event{Kind: rendezvous.EvSettings, Settings: &settings}, everyone)
	return nil
}

func applySetting(st *model.Settings, key string, value any) error {
	bad := func() error { return fmt.Errorf("memory: setting %q: unexpected value %T", key, value) }
	switch key {
	case "registration":
		switch v := value.(type) {
		case model.RegistrationMode:
			st.Registration = v
		case string:
			st.Registration = model.RegistrationMode(v)
		default:
			return bad()
		}
		switch st.Registration {
		case model.RegOpen, model.RegClosed, model.RegInvite:
		default:
			return fmt.Errorf("memory: setting registration: unknown mode %q", st.Registration)
		}
	case "require_approval", "maintenance":
		v, ok := value.(bool)
		if !ok {
			return bad()
		}
		if key == "maintenance" {
			st.Maintenance = v
		} else {
			st.RequireApproval = v
		}
	case "user_cap", "msgs_per_min":
		v, ok := value.(int)
		if !ok {
			return bad()
		}
		if key == "user_cap" {
			st.UserCap = v
		} else {
			st.MsgsPerMin = v
		}
	case "min_version", "maintenance_msg", "announcement", "announcement_id", "admin_pubkey":
		v, ok := value.(string)
		if !ok {
			return bad()
		}
		switch key {
		case "min_version":
			st.MinVersion = v
		case "maintenance_msg":
			st.MaintenanceMsg = v
		case "announcement":
			st.Announcement = v
		case "announcement_id":
			st.AnnouncementID = v
		case "admin_pubkey":
			st.AdminPubKey = v
		}
	default:
		return fmt.Errorf("memory: unknown setting %q", key)
	}
	return nil
}

func (c *Client) CreateInvite(_ context.Context, uses int, expires *time.Time) (model.Invite, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return model.Invite{}, err
	}
	inv := &model.Invite{Code: inviteCode(), UsesLeft: uses, CreatedBy: me.ID, CreatedAt: s.now()}
	if expires != nil {
		t := *expires
		inv.ExpiresAt = &t
	}
	s.invites[inv.Code] = inv
	s.logAdmin(me, "invite.new", inv.Code, fmt.Sprintf("uses=%d", uses))
	return *inv, nil
}

func inviteCode() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("memory: crypto/rand failed: " + err.Error())
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

func (c *Client) ListInvites(context.Context) ([]model.Invite, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return nil, err
	}
	s.logAdmin(me, "invite.list", "", "")
	out := make([]model.Invite, 0, len(s.invites))
	for _, inv := range s.invites {
		out = append(out, *inv)
	}
	sortBy(out, func(a, b model.Invite) bool { return a.CreatedAt.Before(b.CreatedAt) })
	return out, nil
}

func (c *Client) RevokeInvite(_ context.Context, code string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	if s.invites[code] == nil {
		return rendezvous.ErrNotFound
	}
	delete(s.invites, code)
	s.logAdmin(me, "invite.revoke", code, "")
	return nil
}

// SetUserFlag flips banned, muted or approved. Banning also takes the
// user offline and retires their sessions.
func (c *Client) SetUserFlag(_ context.Context, userID, flag string, value bool) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	u := s.users[userID]
	if u == nil {
		return rendezvous.ErrNotFound
	}
	switch flag {
	case "banned":
		u.Banned = value
	case "muted":
		u.Muted = value
	case "approved":
		u.Approved = value
	default:
		return fmt.Errorf("memory: unknown user flag %q", flag)
	}
	s.logAdmin(me, "user."+flag, userID, fmt.Sprint(value))
	// Tell the user first; a kick drops their session and with it delivery.
	s.emitUser(u)
	if flag == "banned" && value {
		s.kick(u)
	}
	return nil
}

// SetUserRole promotes to helper or demotes to user. There is exactly one
// admin per instance and no call elevates to it.
func (c *Client) SetUserRole(_ context.Context, userID string, role model.Role) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	u := s.users[userID]
	if u == nil {
		return rendezvous.ErrNotFound
	}
	if (role != model.RoleUser && role != model.RoleHelper) || u.Role == model.RoleAdmin {
		return rendezvous.ErrForbidden
	}
	u.Role = role
	s.logAdmin(me, "user.role", userID, string(role))
	s.emitUser(u)
	return nil
}

func (c *Client) RenameUser(_ context.Context, userID, newName string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	u := s.users[userID]
	if u == nil {
		return rendezvous.ErrNotFound
	}
	name := strings.ToLower(newName)
	if !usernameRE.MatchString(name) {
		return ErrInvalidUsername
	}
	if other := s.userByName(name); other != nil && other.ID != userID {
		return rendezvous.ErrUsernameTaken
	}
	s.logAdmin(me, "user.rename", userID, u.Username+" -> "+name)
	u.Username = name
	s.emitUser(u)
	return nil
}

// ResetUserKey clears the user's device key; their next launch forges a
// new one and friends get a key-change warning.
func (c *Client) ResetUserKey(_ context.Context, userID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	u := s.users[userID]
	if u == nil {
		return rendezvous.ErrNotFound
	}
	u.PubKey = ""
	s.logAdmin(me, "user.reset-key", userID, "")
	s.emitUser(u)
	return nil
}

// KickUser forces sign-out everywhere: refresh tokens die, live clients
// lose their session and see EvDisconnected, presence goes offline.
func (c *Client) KickUser(_ context.Context, userID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	u := s.users[userID]
	if u == nil {
		return rendezvous.ErrNotFound
	}
	s.kick(u)
	s.logAdmin(me, "user.kick", userID, "")
	return nil
}

func (s *Store) kick(u *model.User) {
	for tok, id := range s.refresh {
		if id == u.ID {
			delete(s.refresh, tok)
		}
	}
	for cl := range s.clients {
		if cl.session != nil && cl.session.UserID == u.ID {
			if cl.events != nil {
				cl.push(rendezvous.Event{Kind: rendezvous.EvDisconnected, Err: rendezvous.ErrUnauthorized})
			}
			cl.session = nil
		}
	}
	if s.online[u.ID] {
		delete(s.online, u.ID)
		s.deliverPresence(u.ID, false)
	}
}

// ListAdminLog returns the newest entries first; limit <= 0 means all.
func (c *Client) ListAdminLog(_ context.Context, limit int) ([]model.AdminLogEntry, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return nil, err
	}
	s.logAdmin(me, "log.list", "", "")
	n := len(s.adminLog)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]model.AdminLogEntry, 0, n)
	for i := len(s.adminLog) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.adminLog[i])
	}
	return out, nil
}

func (c *Client) Stats(context.Context) (model.Stats, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return model.Stats{}, err
	}
	s.logAdmin(me, "stats", "", "")
	now := s.now()
	y, mo, d := now.Date()
	dayStart := time.Date(y, mo, d, 0, 0, 0, 0, now.Location())
	weekAgo := now.Add(-7 * 24 * time.Hour)
	st := model.Stats{UsersTotal: len(s.users), UsersOnline: len(s.online)}
	for _, u := range s.users {
		if !u.LastSeen.Before(dayStart) {
			st.UsersToday++
		}
	}
	for _, m := range s.messages {
		if !m.CreatedAt.Before(dayStart) {
			st.MsgsToday++
		}
		st.DBBytes += int64(len(m.Ciphertext) + len(m.Nonce))
	}
	for _, l := range s.sessionLogs {
		if !l.StartedAt.Before(weekAgo) {
			st.SessionsWeek++
		}
	}
	return st, nil
}

func (c *Client) ListAdminDevices(context.Context) ([]model.AdminDevice, error) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return nil, err
	}
	s.logAdmin(me, "devices.list", "", "")
	out := make([]model.AdminDevice, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, *d)
	}
	sortBy(out, func(a, b model.AdminDevice) bool { return a.DeviceID < b.DeviceID })
	return out, nil
}

// RegisterAdminDevice enrols the calling admin's device. The very first
// device is approved automatically; later ones wait for ApproveAdminDevice
// from an approved device.
func (c *Client) RegisterAdminDevice(_ context.Context, deviceID, pubkey string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	if s.devices[deviceID] != nil {
		return rendezvous.ErrConflict
	}
	d := &model.AdminDevice{DeviceID: deviceID, PubKey: pubkey}
	if len(s.devices) == 0 {
		d.Approved = true
		d.ApprovedAt = s.now()
	}
	s.devices[deviceID] = d
	s.logAdmin(me, "device.register", deviceID, fmt.Sprintf("approved=%v", d.Approved))
	return nil
}

func (c *Client) ApproveAdminDevice(_ context.Context, deviceID string) error {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	me, err := c.admin()
	if err != nil {
		return err
	}
	d := s.devices[deviceID]
	if d == nil {
		return rendezvous.ErrNotFound
	}
	if !d.Approved {
		d.Approved = true
		d.ApprovedAt = s.now()
	}
	s.logAdmin(me, "device.approve", deviceID, "")
	return nil
}

func (s *Store) emitUser(u *model.User) {
	row := copyUser(u)
	s.deliver(rendezvous.Event{Kind: rendezvous.EvUser, User: &row}, s.selfAndAdmins(u.ID))
}
