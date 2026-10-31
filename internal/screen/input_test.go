package screen

import (
	"errors"
	"testing"

	"github.com/bendahl/uinput"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []InputEvent{
		{Kind: KindKey, Code: uinput.KeyA, Press: true},
		{Kind: KindKey, Code: uinput.KeyLeftctrl, Press: false},
		{Kind: KindMove, X: 1919, Y: 1079},
		{Kind: KindMove, X: -5, Y: 0},
		{Kind: KindClick, Button: 3, Press: true},
		{Kind: KindScroll, Delta: -3},
		{Kind: KindScroll, Delta: 1 << 20},
	}
	for _, ev := range cases {
		b := EncodeInput(ev)
		if len(b) != InputEventSize {
			t.Fatalf("encoded size %d, want %d", len(b), InputEventSize)
		}
		got, err := DecodeInput(append(b, 0xff, 0xff)) // trailing padding ignored
		if err != nil {
			t.Fatalf("decode %+v: %v", ev, err)
		}
		if got != ev {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, ev)
		}
	}
}

func TestDecodeInputRejectsBadInput(t *testing.T) {
	if _, err := DecodeInput(make([]byte, InputEventSize-1)); !errors.Is(err, ErrBadInput) {
		t.Errorf("short buffer: got %v", err)
	}
	b := EncodeInput(InputEvent{Kind: "bogus"})
	if _, err := DecodeInput(b); !errors.Is(err, ErrBadInput) {
		t.Errorf("unknown kind: got %v", err)
	}
	b = EncodeInput(InputEvent{Kind: KindKey})
	b[1] = 7
	if _, err := DecodeInput(b); !errors.Is(err, ErrBadInput) {
		t.Errorf("bad press flag: got %v", err)
	}
}
