package crypto

import (
	"bytes"
	"testing"
)

func TestPairwiseSymmetric(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	ka, err := Pairwise(a, b.Public())
	if err != nil {
		t.Fatal(err)
	}
	kb, err := Pairwise(b, a.Public())
	if err != nil {
		t.Fatal(err)
	}
	if ka != kb {
		t.Fatal("pairwise keys differ")
	}
	c, _ := NewIdentity()
	kc, _ := Pairwise(a, c.Public())
	if kc == ka {
		t.Fatal("different peers produced the same key")
	}
}

func TestSealOpen(t *testing.T) {
	k, _ := NewGroupKey()
	n, ct, err := Seal(k, []byte("aad"), []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := Open(k, []byte("aad"), n, ct)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("open: %v %q", err, pt)
	}
	if _, err := Open(k, []byte("other"), n, ct); err == nil {
		t.Fatal("aad mismatch accepted")
	}
	ct[0] ^= 1
	if _, err := Open(k, []byte("aad"), n, ct); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestWrapUnwrap(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	pw, _ := Pairwise(a, b.Public())
	gk, _ := NewGroupKey()
	w, err := WrapKey(pw, "g1", gk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKey(pw, "g1", w)
	if err != nil || got != gk {
		t.Fatal("unwrap failed")
	}
	if _, err := UnwrapKey(pw, "g2", w); err == nil {
		t.Fatal("group id not bound")
	}
}

func TestSignDomain(t *testing.T) {
	a, _ := NewIdentity()
	sig := a.Sign("grant", []byte("x"))
	if err := a.Public().Verify("grant", []byte("x"), sig); err != nil {
		t.Fatal(err)
	}
	if err := a.Public().Verify("session", []byte("x"), sig); err == nil {
		t.Fatal("domain not separated")
	}
}

func TestSeedRoundTrip(t *testing.T) {
	a, _ := NewIdentity()
	b, err := IdentityFromSeed(a.Seed())
	if err != nil || !a.Public().Equal(b.Public()) {
		t.Fatal("seed round trip")
	}
	s := a.Public().String()
	p, err := ParsePublicKey(s)
	if err != nil || !p.Equal(a.Public()) {
		t.Fatal("parse")
	}
	if len(a.Public().Phrase()) < 7 {
		t.Fatal("phrase")
	}
}

func TestChainHash(t *testing.T) {
	var z [32]byte
	h1 := ChainHash(z, []byte("a"))
	h2 := ChainHash(h1, []byte("b"))
	if bytes.Equal(h1[:], h2[:]) || ChainHash(z, []byte("a")) != h1 {
		t.Fatal("chain")
	}
}
