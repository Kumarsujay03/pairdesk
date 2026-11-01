package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/model"
)

// ErrNotAdmin is returned for admin commands from a non-admin.
var ErrNotAdmin = errors.New("only the admin can do that")

func (a *App) requireAdmin() error {
	if !a.IsAdmin() {
		return ErrNotAdmin
	}
	return nil
}

// AllUsers lists everyone with presence (admin).
// PendingUsers lists accounts waiting for the admin's approval.
func (a *App) PendingUsers(ctx context.Context) ([]model.User, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	us, err := a.be.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.User
	for _, u := range us {
		if !u.Approved && !u.Banned {
			out = append(out, u)
		}
	}
	return out, nil
}

// announceUser tells the admin about an account that just appeared: waiting
// for approval, or simply joined.
func (a *App) announceUser(id string) {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	us, err := a.be.ListUsers(ctx)
	if err != nil {
		return
	}
	for _, u := range us {
		if u.ID != id {
			continue
		}
		if !u.Approved && !u.Banned {
			a.Notify("signup:"+u.ID, "PairDesk", u.Username+" registered and is waiting for approval: /admin approve-user "+u.Username+" (or /admin pending)", "normal")
		} else {
			a.emit(Event{Kind: EvNotice, Text: u.Username + " joined"})
		}
		return
	}
}

func (a *App) AllUsers(ctx context.Context) ([]Friend, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	us, err := a.be.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	for _, u := range us {
		a.users[u.ID] = u
		a.byName[u.Username] = u.ID
	}
	a.mu.Unlock()
	var ids []string
	for _, u := range us {
		if u.ID != a.me.ID {
			ids = append(ids, u.ID)
		}
	}
	_ = a.be.WatchPresence(ctx, ids)
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Friend, 0, len(us))
	for _, u := range us {
		out = append(out, Friend{User: u, Online: a.online[u.ID] || u.ID == a.me.ID})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].User.Username < out[j].User.Username
	})
	return out, nil
}

// SetRegistration switches the registration mode.
func (a *App) SetRegistration(ctx context.Context, mode model.RegistrationMode) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	switch mode {
	case model.RegOpen, model.RegClosed, model.RegInvite:
	default:
		return errors.New("mode must be open, closed or invite")
	}
	return a.be.SetSetting(ctx, "registration", string(mode))
}

// SetApproval toggles the approval queue.
func (a *App) SetApproval(ctx context.Context, on bool) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	return a.be.SetSetting(ctx, "require_approval", on)
}

// SetUserCap sets the hard user ceiling (0 = none).
func (a *App) SetUserCap(ctx context.Context, n int) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	return a.be.SetSetting(ctx, "user_cap", n)
}

// NewInvite mints an invite code.
func (a *App) NewInvite(ctx context.Context, uses int, ttl time.Duration) (model.Invite, error) {
	if err := a.requireAdmin(); err != nil {
		return model.Invite{}, err
	}
	if uses <= 0 {
		uses = 1
	}
	var exp *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		exp = &t
	}
	return a.be.CreateInvite(ctx, uses, exp)
}

// Invites lists codes.
func (a *App) Invites(ctx context.Context) ([]model.Invite, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.be.ListInvites(ctx)
}

// RevokeInvite deletes a code.
func (a *App) RevokeInvite(ctx context.Context, code string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	return a.be.RevokeInvite(ctx, code)
}

// Announce sets the pinned banner (empty clears).
func (a *App) Announce(ctx context.Context, text string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	if err := a.be.SetSetting(ctx, "announcement", text); err != nil {
		return err
	}
	return a.be.SetSetting(ctx, "announcement_id", fmt.Sprintf("%d", time.Now().UnixNano()))
}

// Maintenance toggles maintenance mode.
func (a *App) Maintenance(ctx context.Context, on bool, msg string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	if err := a.be.SetSetting(ctx, "maintenance_msg", msg); err != nil {
		return err
	}
	return a.be.SetSetting(ctx, "maintenance", on)
}

// SetMinVersion refuses older clients.
func (a *App) SetMinVersion(ctx context.Context, v string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	return a.be.SetSetting(ctx, "min_version", strings.TrimPrefix(v, "v"))
}

// SetRateLimit sets messages per minute per user.
func (a *App) SetRateLimit(ctx context.Context, perMin int) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	return a.be.SetSetting(ctx, "msgs_per_min", perMin)
}

// Ban / Unban / Mute / Approve ------------------------------------------------

func (a *App) SetUserFlag(ctx context.Context, name, flag string, on bool) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if u.ID == a.me.ID {
		return errors.New("not on yourself")
	}
	return a.be.SetUserFlag(ctx, u.ID, flag, on)
}

// SetHelper promotes or demotes a helper.
func (a *App) SetHelper(ctx context.Context, name string, on bool) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	role := model.RoleUser
	if on {
		role = model.RoleHelper
	}
	return a.be.SetUserRole(ctx, u.ID, role)
}

// Kick forces a user to log in again.
func (a *App) Kick(ctx context.Context, name string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	return a.be.KickUser(ctx, u.ID)
}

// ResetKey invalidates a user's device key.
func (a *App) ResetKey(ctx context.Context, name string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	return a.be.ResetUserKey(ctx, u.ID)
}

// Rename changes a username.
func (a *App) Rename(ctx context.Context, oldName, newName string) error {
	if err := a.requireAdmin(); err != nil {
		return err
	}
	newName = strings.ToLower(strings.TrimSpace(newName))
	if !usernameRe.MatchString(newName) {
		return errors.New("username must be 3-20 chars of a-z, 0-9 or _")
	}
	u, err := a.UserByName(ctx, oldName)
	if err != nil {
		return err
	}
	if err := a.be.RenameUser(ctx, u.ID, newName); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.byName, u.Username)
	u.Username = newName
	a.users[u.ID] = u
	a.byName[newName] = u.ID
	a.mu.Unlock()
	return nil
}

// AdminLog returns recent admin actions.
func (a *App) AdminLog(ctx context.Context, n int) ([]model.AdminLogEntry, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.be.ListAdminLog(ctx, n)
}

// Stats returns instance counters.
func (a *App) Stats(ctx context.Context) (model.Stats, error) {
	if err := a.requireAdmin(); err != nil {
		return model.Stats{}, err
	}
	s, err := a.be.Stats(ctx)
	if err != nil {
		return s, err
	}
	if s.UsersOnline == 0 {
		// Backend did not count presence (Supabase): derive from what we watch.
		a.mu.RLock()
		for id, on := range a.online {
			if on && id != a.me.ID {
				s.UsersOnline++
			}
		}
		a.mu.RUnlock()
		s.UsersOnline++ // me
	}
	return s, nil
}

// AdminDevices lists admin devices.
func (a *App) AdminDevices(ctx context.Context) ([]model.AdminDevice, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.be.ListAdminDevices(ctx)
}

// ApproveDevice approves another admin device. On Supabase this returns a
// token the new device needs; we deliver it as an encrypted DM to ourselves
// which the other device (same account, same pairwise key with self) reads.
func (a *App) ApproveDevice(ctx context.Context, deviceID string) (string, error) {
	if err := a.requireAdmin(); err != nil {
		return "", err
	}
	if ab, ok := a.be.(interface {
		ApproveAdminDeviceToken(ctx context.Context, deviceID string) (string, error)
	}); ok {
		return ab.ApproveAdminDeviceToken(ctx, deviceID)
	}
	return "", a.be.ApproveAdminDevice(ctx, deviceID)
}

// AllGroups lists every group (admin oversight: names and owners only).
func (a *App) AllGroups(ctx context.Context) ([]model.Group, error) {
	if err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.be.ListGroups(ctx)
}

// SetBadge toggles the [admin] tag.
func (a *App) SetBadge(on bool) error {
	p := a.st.Prefs()
	p.ShowBadge = on
	return a.st.SavePrefs(p)
}

// Prefs / SavePrefs passthrough.

