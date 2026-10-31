// Package search keeps a local, encrypted copy of decrypted chat history so
// the user can search it. The server only ever has ciphertext, so search
// has to happen here. Each line on disk is sealed with a key derived from
// the device identity; nothing is readable without the identity file.
package search

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

// Record is one stored message.
type Record struct {
	ID       string         `json:"id"`
	ConvType model.ConvType `json:"ct"`
	ConvID   string         `json:"c"`
	Sender   string         `json:"s"`
	Text     string         `json:"t"`
	Kind     string         `json:"k"`
	At       int64          `json:"at"`
}

// Hit is a search result.
type Hit struct {
	Record
	Score int
}

// Index is the on-disk history plus an in-memory view.
type Index struct {
	dir string
	key [crypto.KeySize]byte

	mu   sync.RWMutex
	recs []Record
	seen map[string]bool
	loaded bool
}

// Open prepares the index directory. Loading is lazy.
func Open(dir string, id *crypto.Identity) (*Index, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Index{dir: dir, key: deriveKey(id), seen: map[string]bool{}}, nil
}

func deriveKey(id *crypto.Identity) [crypto.KeySize]byte {
	priv := id.X25519Private()
	// Domain-separate from other local uses of the scalar.
	var k [crypto.KeySize]byte
	h := crypto.ChainHash(priv, []byte("pairdesk/search/v1"))
	copy(k[:], h[:])
	return k
}

func (ix *Index) path() string { return filepath.Join(ix.dir, "history.enc") }

// Append stores a message (idempotent by id).
func (ix *Index) Append(r Record) error {
	if err := ix.load(); err != nil {
		return err
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.seen[r.ID] || r.Kind == "typing" || r.Kind == "reaction" {
		return nil
	}
	pt, _ := json.Marshal(r)
	nonce, ct, err := crypto.Seal(ix.key, []byte("hist"), pt)
	if err != nil {
		return err
	}
	line := base64.RawStdEncoding.EncodeToString(append(nonce, ct...)) + "\n"
	f, err := os.OpenFile(ix.path(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	ix.recs = append(ix.recs, r)
	ix.seen[r.ID] = true
	return nil
}

func (ix *Index) load() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.loaded {
		return nil
	}
	ix.loaded = true
	f, err := os.Open(ix.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for sc.Scan() {
		raw, err := base64.RawStdEncoding.DecodeString(sc.Text())
		if err != nil || len(raw) < crypto.NonceSize {
			continue
		}
		pt, err := crypto.Open(ix.key, []byte("hist"), raw[:crypto.NonceSize], raw[crypto.NonceSize:])
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(pt, &r) != nil || ix.seen[r.ID] {
			continue
		}
		ix.recs = append(ix.recs, r)
		ix.seen[r.ID] = true
	}
	return sc.Err()
}

// Query finds messages matching every word in q (case-insensitive), with an
// optional conversation filter. Results are newest first.
func (ix *Index) Query(q, convID string, limit int) ([]Hit, error) {
	if err := ix.load(); err != nil {
		return nil, err
	}
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var hits []Hit
	for _, r := range ix.recs {
		if convID != "" && r.ConvID != convID {
			continue
		}
		lt := strings.ToLower(r.Text)
		score := 0
		ok := true
		for _, w := range words {
			if !strings.Contains(lt, w) {
				ok = false
				break
			}
			score += strings.Count(lt, w)
			if strings.Contains(" "+lt+" ", " "+w+" ") {
				score += 2
			}
		}
		if ok {
			hits = append(hits, Hit{Record: r, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].At > hits[j].At
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Prune drops records older than keep (0 = keep everything) and rewrites.
func (ix *Index) Prune(keep time.Duration) error {
	if keep <= 0 {
		return nil
	}
	if err := ix.load(); err != nil {
		return err
	}
	cutoff := time.Now().Add(-keep).UnixNano()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var kept []Record
	for _, r := range ix.recs {
		if r.At >= cutoff {
			kept = append(kept, r)
		}
	}
	tmp := ix.path() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range kept {
		pt, _ := json.Marshal(r)
		nonce, ct, err := crypto.Seal(ix.key, []byte("hist"), pt)
		if err != nil {
			f.Close()
			return err
		}
		w.WriteString(base64.RawStdEncoding.EncodeToString(append(nonce, ct...)) + "\n")
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	ix.recs = kept
	ix.seen = map[string]bool{}
	for _, r := range kept {
		ix.seen[r.ID] = true
	}
	return os.Rename(tmp, ix.path())
}
