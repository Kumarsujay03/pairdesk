package transfer

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
)

// ChunkSize is the chunk length used by BuildManifest and the upper bound
// accepted from any manifest or frame.
const ChunkSize = 256 * 1024

// ErrBadManifest is returned for a manifest that is internally inconsistent
// or whose ID does not match the one a seed is serving.
var ErrBadManifest = errors.New("transfer: bad manifest")

// Manifest describes one file: whole-file hash plus a hash per chunk. It is
// what /send puts in the encrypted DM card.
type Manifest struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	ChunkSize int       `json:"chunk_size"`
	Chunks    []string  `json:"chunks"` // hex SHA-256 per chunk
	From      string    `json:"from"`
	CreatedAt time.Time `json:"created_at"`
}

// BuildManifest streams the file at path once, hashing each chunk and the
// whole file. ID is a fresh random identifier.
func BuildManifest(path string, from string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Manifest{}, err
	}
	if !st.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("transfer: %s is not a regular file", path)
	}
	m := Manifest{
		ID:        crypto.RandomID(12),
		Name:      filepath.Base(path),
		Size:      st.Size(),
		ChunkSize: ChunkSize,
		From:      from,
		CreatedAt: time.Now().UTC(),
	}
	whole := sha256.New()
	r := bufio.NewReaderSize(f, ChunkSize)
	buf := make([]byte, ChunkSize)
	var total int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			sum := sha256.Sum256(buf[:n])
			m.Chunks = append(m.Chunks, hex.EncodeToString(sum[:]))
			whole.Write(buf[:n])
			total += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
	}
	if total != m.Size {
		return Manifest{}, fmt.Errorf("transfer: %s changed while hashing", path)
	}
	m.SHA256 = hex.EncodeToString(whole.Sum(nil))
	return m, nil
}

// NumChunks is ceil(Size / ChunkSize).
func (m Manifest) NumChunks() int {
	if m.ChunkSize <= 0 || m.Size <= 0 {
		return 0
	}
	return int((m.Size + int64(m.ChunkSize) - 1) / int64(m.ChunkSize))
}

// chunkLen is the byte length of chunk i.
func (m Manifest) chunkLen(i int) int {
	off := int64(i) * int64(m.ChunkSize)
	rem := m.Size - off
	if rem > int64(m.ChunkSize) {
		return m.ChunkSize
	}
	return int(rem)
}

// Validate checks the manifest is internally consistent and that nothing
// in it could drive an unbounded allocation.
func (m Manifest) Validate() error {
	switch {
	case m.ID == "" || len(m.ID) > MaxIDLen:
		return fmt.Errorf("%w: bad id", ErrBadManifest)
	case m.Size < 0:
		return fmt.Errorf("%w: negative size", ErrBadManifest)
	case m.ChunkSize <= 0 || m.ChunkSize > ChunkSize:
		return fmt.Errorf("%w: chunk size %d", ErrBadManifest, m.ChunkSize)
	case len(m.Chunks) != m.NumChunks():
		return fmt.Errorf("%w: %d chunk hashes for %d chunks", ErrBadManifest, len(m.Chunks), m.NumChunks())
	}
	if _, err := decodeHash(m.SHA256); err != nil {
		return fmt.Errorf("%w: whole-file hash", ErrBadManifest)
	}
	for i, h := range m.Chunks {
		if _, err := decodeHash(h); err != nil {
			return fmt.Errorf("%w: chunk %d hash", ErrBadManifest, i)
		}
	}
	return nil
}

func decodeHash(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, ErrBadManifest
	}
	copy(out[:], b)
	return out, nil
}

// chunkMatches reports whether data is chunk i of the manifest.
func (m Manifest) chunkMatches(i int, data []byte) bool {
	if i < 0 || i >= len(m.Chunks) || len(data) != m.chunkLen(i) {
		return false
	}
	want, err := decodeHash(m.Chunks[i])
	if err != nil {
		return false
	}
	return sha256.Sum256(data) == want
}
