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

func TestUndoubleByteaWithPrefixInside(t *testing.T) {
	// hex of the text "\x0a0b" including its prefix: 5c 78 30 61 30 62
	in := json.RawMessage(`{"ciphertext":"\\x5c7830613062"}`)
	var row struct {
		Ciphertext bytea `json:"ciphertext"`
	}
	if err := json.Unmarshal(undoubleBytea(in), &row); err != nil {
		t.Fatal(err)
	}
	if string(row.Ciphertext) != "\x0a\x0b" {
		t.Fatalf("got %x", row.Ciphertext)
	}
}

func TestDecodeByteaBareHexFromRealtime(t *testing.T) {
	// Realtime change records: bare hex, no prefix. Length 8 is also valid
	// base64, which used to win and produce garbage.
	v, err := decodeBytea("92a6edd7")
	if err != nil || string(v) != "\x92\xa6\xed\xd7" {
		t.Fatalf("bare hex: %x %v", v, err)
	}
	// Odd-length-in-hex-terms strings are still base64.
	v, err = decodeBytea("aGk=")
	if err != nil || string(v) != "hi" {
		t.Fatalf("base64: %q %v", v, err)
	}
	if _, err := decodeBytea("\\x92a6"); err != nil {
		t.Fatal(err)
	}
}
