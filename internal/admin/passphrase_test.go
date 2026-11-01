package admin

import (
	"testing"
	"time"
)

func TestLock(t *testing.T) {
	l := NewLock(t.TempDir())
	if err := l.Require(); err != nil {
		t.Fatal("unset lock should allow")
	}
	if err := l.Set("", "short"); err == nil {
		t.Fatal("short accepted")
	}
	if err := l.Set("", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := l.Require(); err != nil {
		t.Fatal("should be unlocked after set")
	}
	l.Relock()
	if err := l.Require(); err != ErrLocked {
		t.Fatal("should be locked")
	}
	if err := l.Unlock("nope"); err != ErrWrong {
		t.Fatalf("got %v", err)
	}
	if err := l.Unlock("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set("nope", "new passphrase"); err != ErrWrong {
		t.Fatal("changed without old")
	}
	if err := l.Set("correct horse", "new passphrase"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveNeedsThePassphrase(t *testing.T) {
	l := NewLockAt(t.TempDir() + "/x.pass")
	if err := l.Set("", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove("wrong"); err == nil {
		t.Fatal("removed with the wrong passphrase")
	}
	if err := l.Remove("correct horse"); err != nil || l.IsSet() {
		t.Fatalf("remove: %v set=%v", err, l.IsSet())
	}
}

func TestMinLenIsPerLock(t *testing.T) {
	short := NewLockAt(t.TempDir() + "/a.pass")
	short.MinLen = 4
	if err := short.Set("", "1234"); err != nil {
		t.Fatalf("4-char passkey refused: %v", err)
	}
	if err := short.Set("1234", "123"); err == nil {
		t.Fatal("3-char passkey accepted")
	}
	std := NewLockAt(t.TempDir() + "/b.pass")
	if err := std.Set("", "1234567"); err == nil {
		t.Fatal("default lock accepted 7 characters")
	}
}

func TestTrustHoldsOpenUntilRelock(t *testing.T) {
	l := NewLock(t.TempDir())
	if err := l.Set("", "longenough"); err != nil {
		t.Fatal(err)
	}
	l.Relock()
	if err := l.Require(); err == nil {
		t.Fatal("expected locked")
	}
	l.Trust()
	if err := l.Require(); err != nil {
		t.Fatal("trusted lock should be open:", err)
	}
	l.mu.Lock()
	l.unlocked = time.Now().Add(-time.Hour) // the window is irrelevant while held
	l.mu.Unlock()
	if err := l.Require(); err != nil {
		t.Fatal("held lock expired:", err)
	}
	l.Relock()
	if err := l.Require(); err == nil {
		t.Fatal("relock should close a held lock")
	}
	if err := l.Unlock("longenough"); err != nil {
		t.Fatal(err)
	}
	if !l.Held() {
		t.Fatal("unlock after trust should hold again")
	}
}
