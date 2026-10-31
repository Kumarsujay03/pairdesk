// Package admin holds the admin-side local locks: a passphrase that must be
// entered before admin or remote-access commands run, cached for a short
// window, hashed with Argon2id on disk.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	unlockWindow = 15 * time.Minute
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
)

var ErrLocked = errors.New("admin: locked; enter your passphrase")
var ErrWrong = errors.New("admin: wrong passphrase")

type record struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
	T    uint32 `json:"t"`
	M    uint32 `json:"m"`
	P    uint8  `json:"p"`
}

// Lock guards privileged commands.
type Lock struct {
	path     string
	mu       sync.Mutex
	unlocked time.Time
	fails    int
	lastFail time.Time
}

// NewLock uses <configDir>/admin.pass.
func NewLock(configDir string) *Lock { return &Lock{path: filepath.Join(configDir, "admin.pass")} }

// IsSet reports whether a passphrase exists.
func (l *Lock) IsSet() bool { _, err := os.Stat(l.path); return err == nil }

// Set stores a new passphrase (requires the old one when already set).
func (l *Lock) Set(old, pass string) error {
	if len(pass) < 8 {
		return errors.New("admin: passphrase must be at least 8 characters")
	}
	if l.IsSet() {
		if err := l.check(old); err != nil {
			return err
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	r := record{Salt: salt, Hash: argon2.IDKey([]byte(pass), salt, argonTime, argonMemory, argonThreads, argonKeyLen), T: argonTime, M: argonMemory, P: argonThreads}
	b, _ := json.Marshal(r)
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	l.mu.Lock()
	l.unlocked = time.Now()
	l.mu.Unlock()
	return nil
}

func (l *Lock) check(pass string) error {
	b, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	got := argon2.IDKey([]byte(pass), r.Salt, r.T, r.M, r.P, uint32(len(r.Hash)))
	if subtle.ConstantTimeCompare(got, r.Hash) != 1 {
		return ErrWrong
	}
	return nil
}

// Unlock verifies the passphrase and opens the window. Five failures in a
// row impose a 30 second delay.
func (l *Lock) Unlock(pass string) error {
	l.mu.Lock()
	if l.fails >= 5 && time.Since(l.lastFail) < 30*time.Second {
		l.mu.Unlock()
		return errors.New("admin: too many attempts; wait 30 seconds")
	}
	l.mu.Unlock()
	if err := l.check(pass); err != nil {
		l.mu.Lock()
		l.fails++
		l.lastFail = time.Now()
		l.mu.Unlock()
		return err
	}
	l.mu.Lock()
	l.fails = 0
	l.unlocked = time.Now()
	l.mu.Unlock()
	return nil
}

// Require returns nil if unlocked recently, ErrLocked otherwise. When no
// passphrase is set, everything is allowed (the admin is told to set one).
func (l *Lock) Require() error {
	if !l.IsSet() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.unlocked) < unlockWindow {
		return nil
	}
	return ErrLocked
}

// Relock closes the window immediately.
func (l *Lock) Relock() { l.mu.Lock(); l.unlocked = time.Time{}; l.mu.Unlock() }
