package p2p

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/flynn/noise"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/rendezvous"
)

// Signal type strings carried through the relay.
const (
	sigNoise1 = "noise1"
	sigNoise2 = "noise2"
	sigOffer  = "offer"
	sigAnswer = "answer"
	sigICE    = "ice"
	sigBye    = "bye"
)

const prologuePrefix = "pairdesk/v1/"

var (
	ErrHandshake   = errors.New("p2p: noise handshake failed")
	ErrPeerKey     = errors.New("p2p: peer static key does not match pinned identity")
	ErrBadSignal   = errors.New("p2p: rejected signal")
	ErrFingerprint = errors.New("p2p: DTLS fingerprint signature mismatch")
	ErrClosed      = errors.New("p2p: connection closed")
)

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// staticKey derives the Noise static keypair from the Ed25519 identity.
func staticKey(id *crypto.Identity) (noise.DHKey, error) {
	priv := id.X25519Private()
	pub, err := id.Public().X25519()
	if err != nil {
		return noise.DHKey{}, err
	}
	return noise.DHKey{Private: priv[:], Public: pub[:]}, nil
}

// secureSignaler wraps a Signaler with the Noise cipher states produced by
// the IK handshake. Every payload after the handshake is sealed with an
// explicit nonce so the relay may reorder blobs; replays are rejected.
type secureSignaler struct {
	sig       Signaler
	me, peer  string
	sessionID string

	mu   sync.Mutex
	send *noise.CipherState
	recv *noise.CipherState
	seen map[uint64]struct{}
}

func (s *secureSignaler) aad(typ string) []byte {
	return []byte(prologuePrefix + s.sessionID + "/" + typ)
}

func (s *secureSignaler) sendSealed(ctx context.Context, typ string, plaintext []byte) error {
	s.mu.Lock()
	n := s.send.Nonce()
	ct, err := s.send.Encrypt(nil, s.aad(typ), plaintext)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	payload := make([]byte, 8, 8+len(ct))
	binary.BigEndian.PutUint64(payload, n)
	payload = append(payload, ct...)
	return s.sig.Send(ctx, rendezvous.Signal{
		SessionID: s.sessionID, From: s.me, To: s.peer, Type: typ, Payload: payload,
	})
}

func (s *secureSignaler) open(typ string, payload []byte) ([]byte, error) {
	if len(payload) < 8 {
		return nil, ErrBadSignal
	}
	n := binary.BigEndian.Uint64(payload[:8])
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.seen[n]; dup {
		return nil, fmt.Errorf("%w: replayed nonce %d", ErrBadSignal, n)
	}
	s.recv.SetNonce(n)
	pt, err := s.recv.Decrypt(nil, s.aad(typ), payload[8:])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSignal, err)
	}
	s.seen[n] = struct{}{}
	return pt, nil
}

// handshake runs Noise IK over the relay. The initiator sends noise1 and
// waits for noise2; the responder does the reverse. recv yields only
// signals already filtered to this session and peer.
func handshake(ctx context.Context, cfg Config, peer Peer, sessionID string, initiator bool, recv <-chan rendezvous.Signal) (*secureSignaler, error) {
	mine, err := staticKey(cfg.Me)
	if err != nil {
		return nil, err
	}
	theirs, err := peer.PubKey.X25519()
	if err != nil {
		return nil, err
	}
	hcfg := noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeIK,
		Initiator:     initiator,
		Prologue:      []byte(prologuePrefix + sessionID),
		StaticKeypair: mine,
	}
	if initiator {
		hcfg.PeerStatic = theirs[:]
	}
	hs, err := noise.NewHandshakeState(hcfg)
	if err != nil {
		return nil, err
	}

	wait := func(typ string) ([]byte, error) {
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case s, ok := <-recv:
				if !ok {
					return nil, ErrClosed
				}
				if s.Type == typ {
					return s.Payload, nil
				}
				if s.Type == sigBye {
					return nil, fmt.Errorf("%w: aborted by peer", ErrHandshake)
				}
				// Anything else before the handshake completes is noise
				// from a stale attempt; drop it.
			}
		}
	}
	send := func(typ string, payload []byte) error {
		return cfg.Signaler.Send(ctx, rendezvous.Signal{
			SessionID: sessionID, From: cfg.MyID, To: peer.ID, Type: typ, Payload: payload,
		})
	}

	ss := &secureSignaler{sig: cfg.Signaler, me: cfg.MyID, peer: peer.ID, sessionID: sessionID, seen: map[uint64]struct{}{}}
	if initiator {
		msg1, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		if err := send(sigNoise1, msg1); err != nil {
			return nil, err
		}
		msg2, err := wait(sigNoise2)
		if err != nil {
			return nil, err
		}
		_, cs1, cs2, err := hs.ReadMessage(nil, msg2)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		ss.send, ss.recv = cs1, cs2
	} else {
		msg1, err := wait(sigNoise1)
		if err != nil {
			return nil, err
		}
		if _, _, _, err := hs.ReadMessage(nil, msg1); err != nil {
			_ = send(sigBye, nil)
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		if !cryptoEqual(hs.PeerStatic(), theirs[:]) {
			_ = send(sigBye, nil)
			return nil, ErrPeerKey
		}
		msg2, cs1, cs2, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
		}
		if err := send(sigNoise2, msg2); err != nil {
			return nil, err
		}
		ss.send, ss.recv = cs2, cs1
	}
	return ss, nil
}

func cryptoEqual(a, b []byte) bool { return crypto.PublicKey(a).Equal(crypto.PublicKey(b)) }
