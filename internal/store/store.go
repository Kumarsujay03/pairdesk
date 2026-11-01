// Package store is PairDesk's local state: the device identity, the saved
// login, pinned peer keys, the local signed copy of grants, and user
// preferences. Everything lives under the XDG config and state dirs with
// owner-only permissions.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

const appName = "pairdesk"

// Paths resolves the directories PairDesk uses.
type Paths struct {
	Config string // ~/.config/pairdesk
	State  string // ~/.local/state/pairdesk
	Cache  string // ~/.cache/pairdesk
}

// DefaultPaths honours XDG variables and PAIRDESK_HOME (used by tests and
// by running several instances on one machine).
func DefaultPaths() (Paths, error) {
	if h := os.Getenv("PAIRDESK_HOME"); h != "" {
		return Paths{Config: filepath.Join(h, "config"), State: filepath.Join(h, "state"), Cache: filepath.Join(h, "cache")}, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = filepath.Join(home, ".cache")
	}
	return Paths{
		Config: filepath.Join(cfg, appName),
		State:  filepath.Join(state, appName),
		Cache:  filepath.Join(cache, appName),
	}, nil
}

// Login is the persisted auth state.
type Login struct {
	Email        string    `json:"email"`
	UserID       string    `json:"user_id"`
	Username     string    `json:"username"`
	RefreshToken string    `json:"refresh_token"`
	AdminToken   string    `json:"admin_token,omitempty"`
	DeviceID     string    `json:"device_id"`
	SavedAt      time.Time `json:"saved_at"`
}

// Prefs are user preferences.
type Prefs struct {
	Theme         string `json:"theme"`
	ReducedMotion bool   `json:"reduced_motion"`
	Notifications bool   `json:"notifications"`
	ShowBadge     bool   `json:"show_badge"`
	LogKeepDays   int    `json:"log_keep_days"`
	ScreenThumbs  bool   `json:"screen_thumbs"`
	// NoAutoUpdate turns off installing new releases at start-up (on by default).
	NoAutoUpdate bool `json:"no_auto_update,omitempty"`
	// LastOpened is when the client last reached the main screen; the daily
	// digest shows on the first open of a calendar day.
	LastOpened time.Time `json:"last_opened"`
}

func defaultPrefs() Prefs {
	return Prefs{Theme: "harbor", Notifications: true, LogKeepDays: 0}
}

// Pin records a verified peer key.
type Pin struct {
	UserID   string    `json:"user_id"`
	Username string    `json:"username"`
	PubKey   string    `json:"pubkey"`
	PinnedAt time.Time `json:"pinned_at"`
	Verified bool      `json:"verified"` // true after /verify, false for TOFU
}

// Store is the on-disk state with an in-process cache.
type Store struct {
	Paths Paths
	mu    sync.Mutex
}

// Open creates the directories if needed.
func Open(p Paths) (*Store, error) {
	for _, d := range []string{p.Config, p.State, p.Cache, filepath.Join(p.State, "sessions"), filepath.Join(p.Config, "themes")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		_ = os.Chmod(d, 0o700)
	}
	return &Store{Paths: p}, nil
}

func (s *Store) cfg(name string) string   { return filepath.Join(s.Paths.Config, name) }
func (s *Store) state(name string) string { return filepath.Join(s.Paths.State, name) }

// --- identity -------------------------------------------------------------

var ErrNoIdentity = errors.New("store: no identity")

// Identity loads the device identity, or returns ErrNoIdentity.
func (s *Store) Identity() (*crypto.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.cfg("identity.key"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, err
	}
	defer crypto.Zero(raw)
	var f struct {
		Seed []byte `json:"seed"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	defer crypto.Zero(f.Seed)
	return crypto.IdentityFromSeed(f.Seed)
}

// CreateIdentity forges a new identity and persists it with 0600.
func (s *Store) CreateIdentity() (*crypto.Identity, error) {
	id, err := crypto.NewIdentity()
	if err != nil {
		return nil, err
	}
	seed := id.Seed()
	defer crypto.Zero(seed)
	raw, _ := json.Marshal(map[string]any{"seed": seed, "created_at": time.Now().UTC()})
	defer crypto.Zero(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(s.cfg("identity.key"), raw, 0o600); err != nil {
		return nil, err
	}
	return id, nil
}

// DeleteIdentity retires the key after an admin reset-key. The file is
// renamed, not removed, so old history stays recoverable by the owner.
func (s *Store) DeleteIdentity() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Rename(s.cfg("identity.key"), s.cfg(fmt.Sprintf("identity.key.retired-%d", time.Now().Unix())))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// AdminPin is the admin identity this device trusts: user id and key,
// recorded on first sight and never silently replaced.
type AdminPin struct {
	UserID   string    `json:"user_id"`
	PubKey   string    `json:"pubkey"`
	PinnedAt time.Time `json:"pinned_at"`
}

func (s *Store) AdminPin() (AdminPin, bool) {
	var p AdminPin
	if err := s.readJSON(s.cfg("admin.pin"), &p); err != nil || p.PubKey == "" {
		return p, false
	}
	return p, true
}

func (s *Store) SetAdminPin(p AdminPin) error {
	p.PinnedAt = time.Now().UTC()
	return s.writeJSON(s.cfg("admin.pin"), p, 0o600)
}

// --- login / prefs / pins / grants ------------------------------------------

func (s *Store) Login() (Login, error) {
	var l Login
	err := s.readJSON(s.cfg("login.json"), &l)
	return l, err
}
func (s *Store) SaveLogin(l Login) error {
	l.SavedAt = time.Now().UTC()
	return s.writeJSON(s.cfg("login.json"), l, 0o600)
}

// ForgetSession drops the session tokens but keeps what ties this machine
// to the account as a device: the admin device token and device id. Signing
// in again on the same machine is then still an approved admin device.
func (s *Store) ForgetSession() error {
	l, err := s.Login()
	if err != nil || (l.AdminToken == "" && l.DeviceID == "") {
		return s.ClearLogin()
	}
	keep := Login{AdminToken: l.AdminToken, DeviceID: l.DeviceID, Email: l.Email}
	return s.SaveLogin(keep)
}

func (s *Store) ClearLogin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.cfg("login.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) Prefs() Prefs {
	p := defaultPrefs()
	_ = s.readJSON(s.cfg("prefs.json"), &p)
	return p
}
func (s *Store) SavePrefs(p Prefs) error { return s.writeJSON(s.cfg("prefs.json"), p, 0o600) }

func (s *Store) Pins() (map[string]Pin, error) {
	m := map[string]Pin{}
	err := s.readJSON(s.cfg("pins.json"), &m)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	return m, err
}

// Pin records a peer key. If a different key is already pinned for the user
// it returns ErrKeyChanged and does not overwrite; callers must surface a
// warning and call ForcePin after the user accepts.
var ErrKeyChanged = errors.New("store: peer key changed")

func (s *Store) Pin(p Pin) error {
	pins, err := s.Pins()
	if err != nil {
		return err
	}
	if old, ok := pins[p.UserID]; ok && old.PubKey != p.PubKey {
		return ErrKeyChanged
	}
	if old, ok := pins[p.UserID]; ok && old.Verified && !p.Verified {
		p.Verified = true
		p.PinnedAt = old.PinnedAt
	}
	if p.PinnedAt.IsZero() {
		p.PinnedAt = time.Now().UTC()
	}
	pins[p.UserID] = p
	return s.writeJSON(s.cfg("pins.json"), pins, 0o600)
}

func (s *Store) ForcePin(p Pin) error {
	pins, err := s.Pins()
	if err != nil {
		return err
	}
	p.PinnedAt = time.Now().UTC()
	pins[p.UserID] = p
	return s.writeJSON(s.cfg("pins.json"), pins, 0o600)
}

// Grants is the local signed copy of grants this device has given or holds.
// The host enforces from GrantsGiven; the visitor consults GrantsHeld only to
// decide whether to expect a card.
type Grants struct {
	Given []model.Grant `json:"given"`
	Held  []model.Grant `json:"held"`
}

func (s *Store) Grants() (Grants, error) {
	var g Grants
	err := s.readJSON(s.cfg("grants.json"), &g)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	return g, err
}
func (s *Store) SaveGrants(g Grants) error { return s.writeJSON(s.cfg("grants.json"), g, 0o600) }

// SudoSecret stores the auto-tier sudo password encrypted with a key held in
// the OS keyring when available; otherwise it is encrypted with the device
// identity and the caller is told so. This file is only ever read by this
// device's own client.
func (s *Store) SaveSudoSecret(id *crypto.Identity, password string) error {
	key := localKey(id)
	nonce, ct, err := crypto.Seal(key, []byte("sudo"), []byte(password))
	if err != nil {
		return err
	}
	return s.writeJSON(s.cfg("sudo.enc"), map[string][]byte{"n": nonce, "c": ct}, 0o600)
}

func (s *Store) SudoSecret(id *crypto.Identity) (string, bool) {
	var f map[string][]byte
	if err := s.readJSON(s.cfg("sudo.enc"), &f); err != nil {
		return "", false
	}
	pt, err := crypto.Open(localKey(id), []byte("sudo"), f["n"], f["c"])
	if err != nil {
		return "", false
	}
	return string(pt), true
}

func (s *Store) ClearSudoSecret() error {
	err := os.Remove(s.cfg("sudo.enc"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func localKey(id *crypto.Identity) [crypto.KeySize]byte {
	// Deterministic per-device key derived from the identity scalar.
	return id.X25519Private()
}

// SessionDir returns the directory for one session's logs.
func (s *Store) SessionDir(id string) string { return filepath.Join(s.Paths.State, "sessions", id) }

// --- helpers --------------------------------------------------------------

func (s *Store) readJSON(path string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (s *Store) writeJSON(path string, v any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAtomic(path, raw, mode)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: rename: %w", err)
	}
	return nil
}
