package store

import (
	"os"
	"testing"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
)

func tmpStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("PAIRDESK_HOME", t.TempDir())
	p, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIdentityLifecycle(t *testing.T) {
	s := tmpStore(t)
	if _, err := s.Identity(); err != ErrNoIdentity {
		t.Fatalf("want ErrNoIdentity, got %v", err)
	}
	id, err := s.CreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(s.cfg("identity.key"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %o", st.Mode().Perm())
	}
	again, err := s.Identity()
	if err != nil || !again.Public().Equal(id.Public()) {
		t.Fatal("reload mismatch")
	}
}

func TestPinsAndKeyChange(t *testing.T) {
	s := tmpStore(t)
	if err := s.Pin(Pin{UserID: "u1", PubKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(Pin{UserID: "u1", PubKey: "k2"}); err != ErrKeyChanged {
		t.Fatalf("want ErrKeyChanged, got %v", err)
	}
	if err := s.Pin(Pin{UserID: "u1", PubKey: "k1", Verified: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Pin(Pin{UserID: "u1", PubKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	pins, _ := s.Pins()
	if !pins["u1"].Verified {
		t.Fatal("verified flag lost")
	}
}

func TestSudoSecret(t *testing.T) {
	s := tmpStore(t)
	id, _ := s.CreateIdentity()
	if err := s.SaveSudoSecret(id, "hunter2"); err != nil {
		t.Fatal(err)
	}
	pw, ok := s.SudoSecret(id)
	if !ok || pw != "hunter2" {
		t.Fatal("round trip")
	}
	other, _ := crypto.NewIdentity()
	if _, ok := s.SudoSecret(other); ok {
		t.Fatal("other identity decrypted the secret")
	}
	raw, _ := os.ReadFile(s.cfg("sudo.enc"))
	if string(raw) == "" || containsStr(raw, "hunter2") {
		t.Fatal("secret stored in plaintext")
	}
}

func containsStr(b []byte, s string) bool {
	return len(b) >= len(s) && (string(b) == s || indexOf(b, s) >= 0)
}

func indexOf(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}
