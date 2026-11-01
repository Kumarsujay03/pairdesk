// Package crypto holds every primitive PairDesk uses: device identity,
// pairwise and group keys, authenticated encryption, signatures and
// fingerprint phrases. Nothing else in the codebase touches raw keys.
package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

var (
	ErrBadKey       = errors.New("crypto: bad key")
	ErrBadSignature = errors.New("crypto: bad signature")
	ErrDecrypt      = errors.New("crypto: decryption failed")
)

const (
	KeySize   = 32
	NonceSize = chacha20poly1305.NonceSizeX
)

// Identity is a device's long-lived Ed25519 keypair. The X25519 half used
// for key agreement is derived from it, so one secret backs both.
type Identity struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewIdentity generates a fresh identity from crypto/rand.
func NewIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{priv: priv, pub: pub}, nil
}

// IdentityFromSeed restores an identity from its 32-byte seed.
func IdentityFromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, ErrBadKey
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Identity{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// Seed returns the 32-byte seed. Callers must zero it after persisting.
func (id *Identity) Seed() []byte { return id.priv.Seed() }

// Public returns the Ed25519 public key.
func (id *Identity) Public() PublicKey { return PublicKey(id.pub) }

// Sign signs msg with a domain separator so signatures from different
// contexts can never be replayed against each other.
func (id *Identity) Sign(domain string, msg []byte) []byte {
	return ed25519.Sign(id.priv, domainPrefix(domain, msg))
}

// X25519Private converts the Ed25519 secret into an X25519 scalar.
func (id *Identity) X25519Private() [KeySize]byte {
	full := sha512.Sum512(id.priv.Seed())
	var h [KeySize]byte
	copy(h[:], full[:KeySize])
	h[0] &= 248
	h[31] &= 127
	h[31] |= 64
	return h
}

// PublicKey is an Ed25519 public key.
type PublicKey []byte

// ParsePublicKey decodes the base64 form used in the database.
func ParsePublicKey(s string) (PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrBadKey
	}
	return PublicKey(b), nil
}

// String encodes the key for storage.
func (p PublicKey) String() string { return base64.RawURLEncoding.EncodeToString(p) }

// Equal is constant time.
func (p PublicKey) Equal(o PublicKey) bool { return subtle.ConstantTimeCompare(p, o) == 1 }

// Verify checks a domain-separated signature.
func (p PublicKey) Verify(domain string, msg, sig []byte) error {
	if len(p) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(p), domainPrefix(domain, msg), sig) {
		return ErrBadSignature
	}
	return nil
}

// X25519 converts the Ed25519 public key to its Montgomery form.
func (p PublicKey) X25519() ([KeySize]byte, error) {
	var out [KeySize]byte
	if len(p) != ed25519.PublicKeySize {
		return out, ErrBadKey
	}
	pt, err := new(edwards25519.Point).SetBytes(p)
	if err != nil {
		return out, ErrBadKey
	}
	copy(out[:], pt.BytesMontgomery())
	return out, nil
}

// Fingerprint returns the SHA-256 of the key.
func (p PublicKey) Fingerprint() [32]byte { return sha256.Sum256(p) }

// Phrase renders the first 32 bits of the fingerprint as four words.
// Two humans compare these over a call to pin each other's key.
func (p PublicKey) Phrase() string {
	fp := p.Fingerprint()
	parts := make([]string, 4)
	for i := range parts {
		parts[i] = words[fp[i]]
	}
	return strings.Join(parts, " ")
}

func domainPrefix(domain string, msg []byte) []byte {
	out := append([]byte(domain), 0)
	return append(out, msg...)
}

// Pairwise derives a shared symmetric key between two identities. Both
// sides compute the same key regardless of who calls. The key is bound to
// both public keys (sorted) so it cannot be confused across pairs.
func Pairwise(me *Identity, peer PublicKey) ([KeySize]byte, error) {
	var out [KeySize]byte
	theirs, err := peer.X25519()
	if err != nil {
		return out, err
	}
	mine := me.X25519Private()
	shared, err := curve25519.X25519(mine[:], theirs[:])
	if err != nil {
		return out, err
	}
	a, b := me.Public(), peer
	if strings.Compare(a.String(), b.String()) > 0 {
		a, b = b, a
	}
	salt := append(append([]byte{}, a...), b...)
	r := hkdf.New(sha256.New, shared, salt, []byte("pairdesk/pairwise/v1"))
	if _, err := io.ReadFull(r, out[:]); err != nil {
		return out, err
	}
	return out, nil
}

// NewGroupKey makes a random symmetric key for a group or crew.
func NewGroupKey() ([KeySize]byte, error) {
	var k [KeySize]byte
	_, err := io.ReadFull(rand.Reader, k[:])
	return k, err
}

// Seal encrypts plaintext with XChaCha20-Poly1305. The nonce is random and
// returned separately so the database can store it in its own column.
func Seal(key [KeySize]byte, aad, plaintext []byte) (nonce, ciphertext []byte, err error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

// Open reverses Seal.
func Open(key [KeySize]byte, aad, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	if len(nonce) != NonceSize {
		return nil, ErrDecrypt
	}
	pt, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// WrapKey encrypts a group key for one member using the pairwise key
// between the wrapper and that member. Output is nonce||ciphertext.
func WrapKey(pairwise [KeySize]byte, groupID string, groupKey [KeySize]byte) ([]byte, error) {
	nonce, ct, err := Seal(pairwise, []byte("wrap:"+groupID), groupKey[:])
	if err != nil {
		return nil, err
	}
	return append(nonce, ct...), nil
}

// UnwrapKey reverses WrapKey.
func UnwrapKey(pairwise [KeySize]byte, groupID string, wrapped []byte) ([KeySize]byte, error) {
	var k [KeySize]byte
	if len(wrapped) < NonceSize {
		return k, ErrDecrypt
	}
	pt, err := Open(pairwise, []byte("wrap:"+groupID), wrapped[:NonceSize], wrapped[NonceSize:])
	if err != nil || len(pt) != KeySize {
		return k, ErrDecrypt
	}
	copy(k[:], pt)
	return k, nil
}

// ChainHash extends a hash chain: H(prev || len(line) || line).
func ChainHash(prev [32]byte, line []byte) [32]byte {
	h := sha256.New()
	h.Write(prev[:])
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(line)))
	h.Write(l[:])
	h.Write(line)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// RandomID returns a URL-safe random identifier of n bytes of entropy.
func RandomID(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Zero wipes a byte slice.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
