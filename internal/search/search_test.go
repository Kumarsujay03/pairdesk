package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kumarsujay03/pairdesk/internal/crypto"
	"github.com/Kumarsujay03/pairdesk/internal/model"
)

func TestAppendQueryEncrypted(t *testing.T) {
	dir := t.TempDir()
	id, _ := crypto.NewIdentity()
	ix, err := Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	_ = ix.Append(Record{ID: "1", ConvType: model.ConvDM, ConvID: "a:b", Sender: "a", Text: "fix the nginx config tonight", Kind: "text", At: now})
	_ = ix.Append(Record{ID: "2", ConvType: model.ConvDM, ConvID: "a:b", Sender: "b", Text: "nginx is fine", Kind: "text", At: now + 1})
	_ = ix.Append(Record{ID: "3", ConvType: model.ConvGroup, ConvID: "g", Sender: "a", Text: "lunch?", Kind: "text", At: now + 2})
	_ = ix.Append(Record{ID: "3", ConvType: model.ConvGroup, ConvID: "g", Sender: "a", Text: "dup", Kind: "text", At: now + 3})
	hits, _ := ix.Query("nginx", "", 10)
	if len(hits) != 2 {
		t.Fatalf("hits %d", len(hits))
	}
	hits, _ = ix.Query("nginx config", "", 10)
	if len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("hits %+v", hits)
	}
	hits, _ = ix.Query("lunch", "a:b", 10)
	if len(hits) != 0 {
		t.Fatal("conv filter ignored")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "history.enc"))
	if strings.Contains(string(raw), "nginx") {
		t.Fatal("plaintext on disk")
	}
	// Reload with the same identity works; another identity sees nothing.
	ix2, _ := Open(dir, id)
	hits, _ = ix2.Query("lunch", "", 10)
	if len(hits) != 1 {
		t.Fatal("reload failed")
	}
	other, _ := crypto.NewIdentity()
	ix3, _ := Open(dir, other)
	hits, _ = ix3.Query("lunch", "", 10)
	if len(hits) != 0 {
		t.Fatal("other identity decrypted history")
	}
	if err := ix2.Prune(time.Hour); err != nil {
		t.Fatal(err)
	}
}
