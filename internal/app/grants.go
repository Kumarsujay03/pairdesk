package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
	"github.com/Kumarsujay03/pairdesk/internal/store"
)

const grantDomain = "pairdesk/grant/v1"

func grantBody(g model.Grant) []byte {
	b, _ := json.Marshal(struct {
		Grantor, Grantee, Scope, Sudo, GranteePubKey string
		CreatedAt                                   int64
	}{g.Grantor, g.Grantee, string(g.Scope), string(g.Sudo), g.GranteePubKey, g.CreatedAt.Unix()})
	return b
}

// Grant gives a helper or admin standing access. The signed copy on disk is
// what the host enforces; the backend row only lets the visitor know.
func (a *App) Grant(ctx context.Context, name string, scope model.GrantScope, sudo model.SudoTier, sudoPassword string) error {
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	if u.Role != model.RoleAdmin && u.Role != model.RoleHelper {
		return fmt.Errorf("%s is not an admin or helper", u.Username)
	}
	if !a.IsFriend(u.ID) && u.Role != model.RoleAdmin {
		return fmt.Errorf("%s must be your friend first", u.Username)
	}
	if u.PubKey == "" {
		return errors.New("that user has no device key")
	}
	if sudo == model.SudoAuto {
		if sudoPassword == "" {
			return errors.New("auto sudo needs your password stored locally")
		}
		if err := a.st.SaveSudoSecret(a.id, sudoPassword); err != nil {
			return err
		}
	}
	g := model.Grant{Grantor: a.me.ID, Grantee: u.ID, Scope: scope, Sudo: sudo, GranteePubKey: u.PubKey, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	g.Signature = a.id.Sign(grantDomain, grantBody(g))
	// Replace any existing grant to the same grantee.
	_ = a.Revoke(ctx, name, true)
	saved, err := a.be.PutGrant(ctx, g)
	if err != nil {
		return err
	}
	g.ID = saved.ID
	a.mu.Lock()
	a.grants.Given = append(a.grants.Given, g)
	gr := a.grants
	a.mu.Unlock()
	if err := a.st.SaveGrants(gr); err != nil {
		return err
	}
	a.emit(Event{Kind: EvGrantChanged})
	return nil
}

// Revoke withdraws a grant. Live sessions from that grantee are ended by the
// session layer, which listens for EvGrantChanged.
func (a *App) Revoke(ctx context.Context, name string, quiet bool) error {
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	a.mu.Lock()
	var keep []model.Grant
	var removed []model.Grant
	for _, g := range a.grants.Given {
		if g.Grantee == u.ID {
			removed = append(removed, g)
		} else {
			keep = append(keep, g)
		}
	}
	a.grants.Given = keep
	gr := a.grants
	a.mu.Unlock()
	if len(removed) == 0 {
		if quiet {
			return nil
		}
		return fmt.Errorf("no grant to %s", u.Username)
	}
	for _, g := range removed {
		if g.ID != "" {
			_ = a.be.RevokeGrant(ctx, g.ID)
		}
		if g.Sudo == model.SudoAuto {
			_ = a.st.ClearSudoSecret()
		}
	}
	if err := a.st.SaveGrants(gr); err != nil {
		return err
	}
	a.mu.RLock()
	h := a.onRevoke
	a.mu.RUnlock()
	if h != nil {
		h(u.ID)
	}
	a.emit(Event{Kind: EvGrantChanged, UserID: u.ID})
	return nil
}

// SetRevokeHook is called with the grantee id whenever a grant is withdrawn
// or suspended; the session layer ends that grantee's live sessions.
func (a *App) SetRevokeHook(h func(userID string)) {
	a.mu.Lock()
	a.onRevoke = h
	a.mu.Unlock()
}

// GrantFor returns the valid local grant this host has given a visitor, if
// any. It re-verifies the signature and that the visitor's pinned key still
// matches, so a tampered file or a changed key yields nothing.
func (a *App) GrantFor(visitor string, visitorPub crypto.PublicKey) *model.Grant {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, g := range a.grants.Given {
		if g.Grantee != visitor || g.RevokedAt != nil {
			continue
		}
		if g.GranteePubKey != visitorPub.String() {
			continue
		}
		if err := a.id.Public().Verify(grantDomain, grantBody(g), g.Signature); err != nil {
			continue
		}
		gg := g
		return &gg
	}
	return nil
}

// GrantsGiven lists grants I have given.
func (a *App) GrantsGiven() []model.Grant { a.mu.RLock(); defer a.mu.RUnlock(); return append([]model.Grant{}, a.grants.Given...) }

// GrantsHeld lists grants others gave me (from the backend hint).
func (a *App) GrantsHeld(ctx context.Context) ([]model.Grant, error) {
	all, err := a.be.ListGrants(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Grant
	for _, g := range all {
		if g.Grantee == a.me.ID && g.RevokedAt == nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// HeldGrantFrom reports whether the backend says host has granted me scope.
func (a *App) HeldGrantFrom(ctx context.Context, host string, scope model.GrantScope) (*model.Grant, error) {
	hs, err := a.GrantsHeld(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range hs {
		if g.Grantor == host && (g.Scope == scope || g.Scope == model.ScopeBoard) {
			gg := g
			return &gg, nil
		}
	}
	return nil, nil
}

// suspendGrantsFor drops local grants to a user whose key changed.
func (a *App) suspendGrantsFor(userID string) {
	a.mu.Lock()
	var keep []model.Grant
	changed := false
	for _, g := range a.grants.Given {
		if g.Grantee == userID {
			changed = true
			if g.ID != "" {
				go a.be.RevokeGrant(a.ctx, g.ID)
			}
			continue
		}
		keep = append(keep, g)
	}
	a.grants.Given = keep
	gr := a.grants
	a.mu.Unlock()
	if changed {
		_ = a.st.SaveGrants(gr)
		a.mu.RLock()
		h := a.onRevoke
		a.mu.RUnlock()
		if h != nil {
			h(userID)
		}
		a.emit(Event{Kind: EvGrantChanged, UserID: userID, Text: "grant suspended: device key changed"})
	}
}

// Verify pins a user's key as verified after the phrases matched.
func (a *App) Verify(ctx context.Context, name string) (string, error) {
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return "", err
	}
	pk, err := crypto.ParsePublicKey(u.PubKey)
	if err != nil {
		return "", errors.New("that user has no device key")
	}
	return pk.Phrase(), nil
}

// ConfirmVerify marks the key verified.
func (a *App) ConfirmVerify(ctx context.Context, name string) error {
	u, err := a.UserByName(ctx, name)
	if err != nil {
		return err
	}
	return a.st.ForcePin(store.Pin{UserID: u.ID, Username: u.Username, PubKey: u.PubKey, Verified: true})
}

// AcceptKeyChange re-pins a changed key after the user agreed.
func (a *App) AcceptKeyChange(ctx context.Context, name string) error {
	if name == "admin" {
		return a.AcceptAdminKeyChange()
	}
	u, err := a.be.UserByName(ctx, strings.ToLower(strings.TrimPrefix(name, "@")))
	if err != nil {
		return err
	}
	if err := a.st.ForcePin(store.Pin{UserID: u.ID, Username: u.Username, PubKey: u.PubKey}); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.suspect, u.ID)
	delete(a.pairwise, u.ID)
	a.users[u.ID] = u
	a.byName[u.Username] = u.ID
	a.mu.Unlock()
	return nil
}

// MyPhrase is my own fingerprint phrase.
func (a *App) MyPhrase() string { return a.id.Public().Phrase() }
