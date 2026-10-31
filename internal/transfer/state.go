package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// sidecar is the ".part.json" file next to a partial download.
type sidecar struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
	Have string `json:"have"` // hex bitmap, LSB-first
}

func partPath(dest string) string    { return dest + ".part" }
func sidecarPath(dest string) string { return dest + ".part.json" }

// loadSidecar returns the bitmap recorded for dest, or all-false when there
// is no sidecar or it belongs to another manifest.
func loadSidecar(dest string, m Manifest) []bool {
	n := m.NumChunks()
	b, err := os.ReadFile(sidecarPath(dest))
	if err != nil {
		return make([]bool, n)
	}
	var sc sidecar
	if json.Unmarshal(b, &sc) != nil || sc.ID != m.ID || sc.Size != m.Size {
		return make([]bool, n)
	}
	raw, err := hex.DecodeString(sc.Have)
	if err != nil {
		return make([]bool, n)
	}
	bits, ok := unpackBits(raw, n)
	if !ok {
		return make([]bool, n)
	}
	// Chunks claimed beyond the .part file's extent cannot be real.
	st, err := os.Stat(partPath(dest))
	if err != nil || st.Size() != m.Size {
		return make([]bool, n)
	}
	return bits
}

// writeSidecar atomically replaces the sidecar.
func writeSidecar(dest string, m Manifest, have []bool) error {
	sc := sidecar{ID: m.ID, Size: m.Size, Have: hex.EncodeToString(packBits(have))}
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := sidecarPath(dest) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, sidecarPath(dest))
}

// Have reports which chunks of m a local file already holds. A complete
// file at dest is hashed chunk by chunk; otherwise the ".part" file's
// sidecar bitmap is used. A file that exists but has the wrong size
// contributes nothing.
func Have(dest string, m Manifest) ([]bool, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	_, have, err := locate(dest, m)
	return have, err
}

// locate finds the file to serve chunks from: dest itself when complete,
// otherwise its ".part". It returns os.ErrNotExist when neither exists.
func locate(dest string, m Manifest) (string, []bool, error) {
	if st, err := os.Stat(dest); err == nil && st.Mode().IsRegular() && st.Size() == m.Size {
		have, err := hashHave(dest, m)
		if err != nil {
			return "", nil, err
		}
		return dest, have, nil
	}
	if st, err := os.Stat(partPath(dest)); err == nil && st.Mode().IsRegular() {
		return partPath(dest), loadSidecar(dest, m), nil
	}
	return "", make([]bool, m.NumChunks()), fmt.Errorf("transfer: %s: %w", dest, os.ErrNotExist)
}

// hashHave verifies every chunk of a complete file against the manifest.
func hashHave(path string, m Manifest) ([]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n := m.NumChunks()
	have := make([]bool, n)
	buf := make([]byte, m.ChunkSize)
	for i := 0; i < n; i++ {
		l := m.chunkLen(i)
		if _, err := io.ReadFull(f, buf[:l]); err != nil {
			return nil, err
		}
		have[i] = m.chunkMatches(i, buf[:l])
	}
	return have, nil
}

// hashFile returns the hex SHA-256 of the whole file.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
