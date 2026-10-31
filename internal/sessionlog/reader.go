package sessionlog

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
)

// Entry is a session listing row.
type Entry struct {
	Dir  string
	Meta Meta
}

// List returns sessions under root, newest first.
func List(root string) ([]Entry, error) {
	ds, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, d := range ds {
		if !d.IsDir() {
			continue
		}
		m, err := ReadMeta(filepath.Join(root, d.Name()))
		if err != nil {
			continue
		}
		out = append(out, Entry{Dir: filepath.Join(root, d.Name()), Meta: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.StartedAt.After(out[j].Meta.StartedAt) })
	return out, nil
}

// ReadMeta loads meta.json.
func ReadMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// ReadCommands loads commands.jsonl.
func ReadCommands(dir string) ([]Command, error) {
	var out []Command
	err := eachLine(filepath.Join(dir, "commands.jsonl"), func(b []byte) error {
		var c Command
		if err := json.Unmarshal(b, &c); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

// ReadFiles loads files.jsonl.
func ReadFiles(dir string) ([]FileEvent, error) {
	var out []FileEvent
	err := eachLine(filepath.Join(dir, "files.jsonl"), func(b []byte) error {
		var f FileEvent
		if err := json.Unmarshal(b, &f); err != nil {
			return err
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

// CommandOutput extracts the terminal output recorded for one command.
func CommandOutput(dir string, c Command) (string, error) {
	f, err := os.Open(filepath.Join(dir, "transcript.cast"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(c.OutOffset, io.SeekStart); err != nil {
		return "", err
	}
	r := bufio.NewReader(io.LimitReader(f, c.OutLen))
	var sb strings.Builder
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var ev []any
			if json.Unmarshal(line, &ev) == nil && len(ev) == 3 {
				if kind, _ := ev[1].(string); kind == "o" {
					if s, ok := ev[2].(string); ok {
						sb.WriteString(s)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

// VerifyResult is what Verify reports.
type VerifyResult struct {
	ChainOK     bool
	TranscriptOK bool
	SignatureOK bool
	Signer      string
	Problems    []string
}

// Verify recomputes the chains and checks the seal signature. pub may be
// nil, in which case the signer key recorded in meta is used (TOFU).
func Verify(dir string, pub crypto.PublicKey) (VerifyResult, error) {
	var r VerifyResult
	m, err := ReadMeta(dir)
	if err != nil {
		return r, err
	}
	r.Signer = m.SignerPubKey
	cmds, err := ReadCommands(dir)
	if err != nil {
		return r, err
	}
	files, err := ReadFiles(dir)
	if err != nil {
		return r, err
	}
	r.ChainOK = true
	var prev [32]byte
	for i, c := range cmds {
		if c.Prev != hex.EncodeToString(prev[:]) {
			r.ChainOK = false
			r.Problems = append(r.Problems, fmt.Sprintf("command %d: broken link", i+1))
		}
		payload, _ := json.Marshal(stripHash(c))
		h := crypto.ChainHash(prev, payload)
		if c.Hash != hex.EncodeToString(h[:]) {
			r.ChainOK = false
			r.Problems = append(r.Problems, fmt.Sprintf("command %d: hash mismatch", i+1))
		}
		prev = h
	}
	if m.CommandsHash != "" && m.CommandsHash != hex.EncodeToString(prev[:]) {
		r.ChainOK = false
		r.Problems = append(r.Problems, "commands: final hash differs from seal")
	}
	prev = [32]byte{}
	for i, f := range files {
		if f.Prev != hex.EncodeToString(prev[:]) {
			r.ChainOK = false
			r.Problems = append(r.Problems, fmt.Sprintf("file event %d: broken link", i+1))
		}
		payload, _ := json.Marshal(stripFileHash(f))
		h := crypto.ChainHash(prev, payload)
		if f.Hash != hex.EncodeToString(h[:]) {
			r.ChainOK = false
			r.Problems = append(r.Problems, fmt.Sprintf("file event %d: hash mismatch", i+1))
		}
		prev = h
	}
	if m.FilesHash != "" && m.FilesHash != hex.EncodeToString(prev[:]) {
		r.ChainOK = false
		r.Problems = append(r.Problems, "files: final hash differs from seal")
	}
	th, err := fileSHA(filepath.Join(dir, "transcript.cast"))
	if err == nil && m.TranscriptHash != "" {
		r.TranscriptOK = th == m.TranscriptHash
		if !r.TranscriptOK {
			r.Problems = append(r.Problems, "transcript: hash differs from seal")
		}
	}
	if len(m.Signature) > 0 {
		if pub == nil {
			pub, _ = crypto.ParsePublicKey(m.SignerPubKey)
		}
		if pub != nil && pub.Verify(sigDomain, sealBody(m), m.Signature) == nil {
			r.SignatureOK = true
		} else {
			r.Problems = append(r.Problems, "seal: bad signature")
		}
	} else {
		r.Problems = append(r.Problems, "seal: not sealed")
	}
	return r, nil
}

func eachLine(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}
