package sessionlog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

func TestWriteVerifyTamper(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s1")
	id, _ := crypto.NewIdentity()
	w, err := NewWriter(dir, Meta{ID: "s1", Kind: model.SessionBoard, Via: model.ViaCard, Visitor: "v", Host: "h", Role: "host"})
	if err != nil {
		t.Fatal(err)
	}
	seq := w.CommandStart()
	_ = w.Output([]byte("hello\r\n"))
	_ = w.File(FileEvent{Path: "/tmp/x", Event: "open"})
	if err := w.CommandEnd(Command{Cmd: "echo hello", Cwd: "/tmp", Exit: 0}); err != nil {
		t.Fatal(err)
	}
	_ = w.CommandStart()
	_ = w.Output([]byte("denied\r\n"))
	_ = w.CommandEnd(Command{Cmd: "sudo rm -rf /", Exit: 1, Sudo: true})
	if err := w.Seal(id, "ended by host"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm %o", st.Mode().Perm())
	}
	cmds, err := ReadCommands(dir)
	if err != nil || len(cmds) != 2 || cmds[0].Seq != seq {
		t.Fatalf("cmds %v %+v", err, cmds)
	}
	out, err := CommandOutput(dir, cmds[0])
	if err != nil || out != "hello\r\n" {
		t.Fatalf("output %q %v", out, err)
	}
	files, _ := ReadFiles(dir)
	if len(files) != 1 || files[0].ByCmd != seq {
		t.Fatalf("files %+v", files)
	}
	r, err := Verify(dir, id.Public())
	if err != nil || !r.ChainOK || !r.TranscriptOK || !r.SignatureOK {
		t.Fatalf("verify %+v %v", r, err)
	}
	m, _ := ReadMeta(dir)
	if m.Commands != 2 || m.Sudo != 1 || m.EndedAt == nil {
		t.Fatalf("meta %+v", m)
	}
	// Tamper with a command line.
	p := filepath.Join(dir, "commands.jsonl")
	b, _ := os.ReadFile(p)
	b = []byte(strings.Replace(string(b), "rm -rf /", "ls", 1))
	_ = os.WriteFile(p, b, 0o600)
	r, _ = Verify(dir, id.Public())
	if r.ChainOK {
		t.Fatal("tamper not detected")
	}
	// Tamper with the transcript.
	tp := filepath.Join(dir, "transcript.cast")
	tb, _ := os.ReadFile(tp)
	_ = os.WriteFile(tp, append(tb, '\n'), 0o600)
	r, _ = Verify(dir, id.Public())
	if r.TranscriptOK {
		t.Fatal("transcript tamper not detected")
	}
	// Wrong signer.
	other, _ := crypto.NewIdentity()
	r, _ = Verify(dir, other.Public())
	if r.SignatureOK {
		t.Fatal("wrong key verified")
	}
	if _, err := List(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherDiff(t *testing.T) {
	root := t.TempDir()
	logdir := filepath.Join(t.TempDir(), "log")
	w, err := NewWriter(logdir, Meta{ID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wt, err := NewWatcher(ctx, w, root, []string{logdir})
	if err != nil {
		t.Skip("inotify unavailable:", err)
	}
	f := filepath.Join(root, "notes.txt")
	_ = os.WriteFile(f, []byte("one\ntwo\n"), 0o600)
	time.Sleep(100 * time.Millisecond)
	// Open for read then modify.
	b, _ := os.ReadFile(f)
	_ = b
	_ = os.WriteFile(f, []byte("one\nthree\n"), 0o600)
	time.Sleep(200 * time.Millisecond)
	wt.Close()
	_ = w.Seal(nil, "test")
	files, _ := ReadFiles(logdir)
	var sawCreate, sawModifyWithDiff bool
	for _, fe := range files {
		if fe.Path == f && fe.Event == "create" {
			sawCreate = true
		}
		if fe.Path == f && fe.Event == "modify" && fe.Diff != "" {
			sawModifyWithDiff = true
			d, _ := os.ReadFile(filepath.Join(logdir, fe.Diff))
			if !strings.Contains(string(d), "+three") || !strings.Contains(string(d), "-two") {
				t.Fatalf("diff content: %s", d)
			}
		}
	}
	if !sawCreate || !sawModifyWithDiff {
		t.Fatalf("events: %+v", files)
	}
}
