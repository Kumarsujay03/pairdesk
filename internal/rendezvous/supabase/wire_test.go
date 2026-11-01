package supabase

import (
	"encoding/json"
	"testing"
)

func TestUndoubleBytea(t *testing.T) {
	// Realtime sends the hex of the hex text: "\x0a0b" becomes "\x30613062".
	in := json.RawMessage(`{"id":"m1","ciphertext":"\\x30613062","nonce":"\\x","sender":"u1","key_epoch":1}`)
	var row struct {
		Ciphertext bytea `json:"ciphertext"`
		Nonce      bytea `json:"nonce"`
		Sender     string
	}
	if err := json.Unmarshal(undoubleBytea(in), &row); err != nil {
		t.Fatal(err)
	}
	if string(row.Ciphertext) != "\x0a\x0b" || len(row.Nonce) != 0 || row.Sender != "u1" {
		t.Fatalf("got %x / %x / %q", row.Ciphertext, row.Nonce, row.Sender)
	}
	// Real binary that is not ASCII hex is left alone.
	plain := json.RawMessage(`{"ciphertext":"\\x0a0bff"}`)
	if string(undoubleBytea(plain)) != string(plain) {
		t.Fatalf("plain bytea changed: %s", undoubleBytea(plain))
	}
}
