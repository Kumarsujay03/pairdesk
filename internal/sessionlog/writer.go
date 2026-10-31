// Package sessionlog records what happened during a remote session on the
// machine where it happened: a timed transcript, every command with its
// output span, every file touched, diffs for edited text files, all
// hash-chained and signed at the end so neither side can quietly edit it.
package sessionlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

const sigDomain = "pairdesk/sessionlog/v1"

// Meta is meta.json.
type Meta struct {
	ID          string            `json:"id"`
	Kind        model.SessionKind `json:"kind"`
	Via         model.SessionVia  `json:"via"`
	Visitor     string            `json:"visitor"`
	VisitorName string            `json:"visitor_name"`
	Host        string            `json:"host"`
	HostName    string            `json:"host_name"`
	Role        string            `json:"role"` // host | visitor: whose copy this is
	SudoTier    model.SudoTier    `json:"sudo_tier"`
	Control     bool              `json:"control,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"`
	EndReason   string            `json:"end_reason,omitempty"`
	Cols        int               `json:"cols,omitempty"`
	Rows        int               `json:"rows,omitempty"`
	Commands    int               `json:"commands"`
	FilesTouched int              `json:"files_touched"`
	Sudo        int               `json:"sudo_commands"`
	// Seal
	CommandsHash   string `json:"commands_hash,omitempty"`
	FilesHash      string `json:"files_hash,omitempty"`
	TranscriptHash string `json:"transcript_hash,omitempty"`
	SignerPubKey   string `json:"signer_pubkey,omitempty"`
	Signature      []byte `json:"signature,omitempty"`
}

// Command is one commands.jsonl entry.
type Command struct {
	Seq       int       `json:"seq"`
	Time      time.Time `json:"ts"`
	Cmd       string    `json:"cmd"`
	Cwd       string    `json:"cwd"`
	Exit      int       `json:"exit"`
	Sudo      bool      `json:"sudo"`
	Typist    string    `json:"typist,omitempty"`
	OutOffset int64     `json:"out_offset"` // byte offset in transcript.cast where output begins
	OutLen    int64     `json:"out_len"`
	Prev      string    `json:"prev"`
	Hash      string    `json:"hash"`
}

// FileEvent is one files.jsonl entry.
type FileEvent struct {
	Seq   int       `json:"seq"`
	Time  time.Time `json:"ts"`
	Path  string    `json:"path"`
	Event string    `json:"event"` // open|create|modify|delete|rename
	ByCmd int       `json:"by_cmd"` // Command.Seq running at the time, 0 if none
	Diff  string    `json:"diff,omitempty"` // relative path under diffs/
	Prev  string    `json:"prev"`
	Hash  string    `json:"hash"`
}

// Writer appends to one session directory.
type Writer struct {
	Dir  string
	meta Meta

	mu        sync.Mutex
	cast      *os.File
	castW     *bufio.Writer
	castOff   int64
	cmds      *os.File
	files     *os.File
	cmdSeq    int
	fileSeq   int
	cmdPrev   [32]byte
	filePrev  [32]byte
	start     time.Time
	curCmd    int
	curCmdOff int64
	lastEnd   int64
	sealed    bool
}

// NewWriter creates the directory and opens the files.
func NewWriter(dir string, meta Meta) (*Writer, error) {
	if err := os.MkdirAll(filepath.Join(dir, "diffs"), 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(dir, 0o700)
	if meta.StartedAt.IsZero() {
		meta.StartedAt = time.Now().UTC()
	}
	w := &Writer{Dir: dir, meta: meta, start: meta.StartedAt}
	var err error
	open := func(name string) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND|os.O_EXCL, 0o600)
	}
	if w.cast, err = open("transcript.cast"); err != nil {
		return nil, err
	}
	w.castW = bufio.NewWriterSize(w.cast, 64<<10)
	if w.cmds, err = open("commands.jsonl"); err != nil {
		return nil, err
	}
	if w.files, err = open("files.jsonl"); err != nil {
		return nil, err
	}
	cols, rows := meta.Cols, meta.Rows
	if cols == 0 {
		cols, rows = 80, 24
	}
	hdr, _ := json.Marshal(map[string]any{
		"version": 2, "width": cols, "height": rows,
		"timestamp": meta.StartedAt.Unix(),
		"title":     fmt.Sprintf("pairdesk %s %s→%s", meta.Kind, meta.VisitorName, meta.HostName),
	})
	if err := w.writeCast(append(hdr, '\n')); err != nil {
		return nil, err
	}
	if err := w.writeMeta(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) writeCast(b []byte) error {
	n, err := w.castW.Write(b)
	w.castOff += int64(n)
	return err
}

// Output records bytes sent to the visitor's terminal (asciicast "o" event).
func (w *Writer) Output(b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		return errors.New("sessionlog: sealed")
	}
	t := time.Since(w.start).Seconds()
	ev, _ := json.Marshal([]any{t, "o", string(b)})
	return w.writeCast(append(ev, '\n'))
}

// Input records bytes typed by the visitor (asciicast "i" event). Callers
// must redact secrets before calling.
func (w *Writer) Input(b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		return errors.New("sessionlog: sealed")
	}
	t := time.Since(w.start).Seconds()
	ev, _ := json.Marshal([]any{t, "i", string(b)})
	return w.writeCast(append(ev, '\n'))
}

// Resize records a terminal size change.
func (w *Writer) Resize(cols, rows int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := time.Since(w.start).Seconds()
	ev, _ := json.Marshal([]any{t, "r", fmt.Sprintf("%dx%d", cols, rows)})
	w.meta.Cols, w.meta.Rows = cols, rows
	return w.writeCast(append(ev, '\n'))
}

// CommandStart marks the beginning of a command's output span and returns
// its sequence number for file-event correlation.
func (w *Writer) CommandStart() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.castW.Flush()
	w.cmdSeq++
	w.curCmd = w.cmdSeq
	w.curCmdOff = w.castOff
	return w.curCmd
}

// CurrentCommand returns the running command's seq (0 if none).
func (w *Writer) CurrentCommand() int { w.mu.Lock(); defer w.mu.Unlock(); return w.curCmd }

// CommandEnd writes the completed command entry.
func (w *Writer) CommandEnd(c Command) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		return errors.New("sessionlog: sealed")
	}
	_ = w.castW.Flush()
	if c.Seq == 0 {
		c.Seq = w.curCmd
	}
	if c.Seq == 0 {
		w.cmdSeq++
		c.Seq = w.cmdSeq
		c.OutOffset = w.lastEnd
	} else {
		c.OutOffset = w.curCmdOff
	}
	c.OutLen = w.castOff - c.OutOffset
	w.lastEnd = w.castOff
	if c.Time.IsZero() {
		c.Time = time.Now().UTC()
	}
	w.curCmd = 0
	w.meta.Commands++
	if c.Sudo {
		w.meta.Sudo++
	}
	c.Prev = hex.EncodeToString(w.cmdPrev[:])
	payload, _ := json.Marshal(stripHash(c))
	h := crypto.ChainHash(w.cmdPrev, payload)
	w.cmdPrev = h
	c.Hash = hex.EncodeToString(h[:])
	line, _ := json.Marshal(c)
	_, err := w.cmds.Write(append(line, '\n'))
	return err
}

// File writes a file event.
func (w *Writer) File(f FileEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		return errors.New("sessionlog: sealed")
	}
	w.fileSeq++
	f.Seq = w.fileSeq
	if f.Time.IsZero() {
		f.Time = time.Now().UTC()
	}
	if f.ByCmd == 0 {
		f.ByCmd = w.curCmd
	}
	w.meta.FilesTouched++
	f.Prev = hex.EncodeToString(w.filePrev[:])
	payload, _ := json.Marshal(stripFileHash(f))
	h := crypto.ChainHash(w.filePrev, payload)
	w.filePrev = h
	f.Hash = hex.EncodeToString(h[:])
	line, _ := json.Marshal(f)
	_, err := w.files.Write(append(line, '\n'))
	return err
}

// WriteDiff stores a diff file and returns its relative name.
func (w *Writer) WriteDiff(name string, content []byte) (string, error) {
	rel := filepath.Join("diffs", fmt.Sprintf("%04d-%s", w.fileSeq+1, sanitize(name)))
	return rel, os.WriteFile(filepath.Join(w.Dir, rel), content, 0o600)
}

// Seal closes the files, hashes everything and signs with the identity.
func (w *Writer) Seal(id *crypto.Identity, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		return nil
	}
	w.sealed = true
	_ = w.castW.Flush()
	_ = w.cast.Sync()
	_ = w.cast.Close()
	_ = w.cmds.Sync()
	_ = w.cmds.Close()
	_ = w.files.Sync()
	_ = w.files.Close()
	now := time.Now().UTC()
	w.meta.EndedAt = &now
	w.meta.EndReason = reason
	w.meta.CommandsHash = hex.EncodeToString(w.cmdPrev[:])
	w.meta.FilesHash = hex.EncodeToString(w.filePrev[:])
	th, err := fileSHA(filepath.Join(w.Dir, "transcript.cast"))
	if err != nil {
		return err
	}
	w.meta.TranscriptHash = th
	if id != nil {
		w.meta.SignerPubKey = id.Public().String()
		w.meta.Signature = id.Sign(sigDomain, sealBody(w.meta))
	}
	return w.writeMeta()
}

// Meta returns a copy of the current metadata.
func (w *Writer) Meta() Meta { w.mu.Lock(); defer w.mu.Unlock(); return w.meta }

func (w *Writer) writeMeta() error {
	b, _ := json.MarshalIndent(w.meta, "", "  ")
	tmp := filepath.Join(w.Dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(w.Dir, "meta.json"))
}

func sealBody(m Meta) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": m.ID, "kind": m.Kind, "visitor": m.Visitor, "host": m.Host,
		"started": m.StartedAt.Unix(), "ended": m.EndedAt.Unix(),
		"commands": m.CommandsHash, "files": m.FilesHash, "transcript": m.TranscriptHash,
		"n_cmd": m.Commands, "n_files": m.FilesTouched,
	})
	return b
}

func stripHash(c Command) Command        { c.Hash = ""; return c }
func stripFileHash(f FileEvent) FileEvent { f.Hash = ""; return f }

func fileSHA(path string) (string, error) {
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

func sanitize(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name) && len(out) < 60; i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
