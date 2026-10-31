package screen

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bendahl/uinput"
)

func TestLookupKey(t *testing.T) {
	cases := []struct {
		in   string
		want Key
	}{
		{"a", Key{Code: uinput.KeyA}},
		{"Z", Key{Code: uinput.KeyZ, Shift: true}},
		{"0", Key{Code: uinput.Key0}},
		{"!", Key{Code: uinput.Key1, Shift: true}},
		{" ", Key{Code: uinput.KeySpace}},
		{"/", Key{Code: uinput.KeySlash}},
		{"?", Key{Code: uinput.KeySlash, Shift: true}},
		{"\x01", Key{Code: uinput.KeyA, Ctrl: true}},
		{"\x03", Key{Code: uinput.KeyC, Ctrl: true}},
		{"\x1a", Key{Code: uinput.KeyZ, Ctrl: true}},
		{"\t", Key{Code: uinput.KeyTab}},
		{"\r", Key{Code: uinput.KeyEnter}},
		{"\x7f", Key{Code: uinput.KeyBackspace}},
		{"\x1b", Key{Code: uinput.KeyEsc}},
		{"\x1b[A", Key{Code: uinput.KeyUp}},
		{"\x1bOB", Key{Code: uinput.KeyDown}},
		{"\x1b[C", Key{Code: uinput.KeyRight}},
		{"\x1b[D", Key{Code: uinput.KeyLeft}},
		{"\x1b[H", Key{Code: uinput.KeyHome}},
		{"\x1b[F", Key{Code: uinput.KeyEnd}},
		{"\x1b[3~", Key{Code: uinput.KeyDelete}},
		{"\x1b[5~", Key{Code: uinput.KeyPageup}},
		{"\x1b[6~", Key{Code: uinput.KeyPagedown}},
		{"\x1b[Z", Key{Code: uinput.KeyTab, Shift: true}},
		{"\x1bOP", Key{Code: uinput.KeyF1}},
		{"\x1b[15~", Key{Code: uinput.KeyF5}},
		{"\x1b[24~", Key{Code: uinput.KeyF12}},
		{"\x1b[1;5A", Key{Code: uinput.KeyUp, Ctrl: true}},
		{"\x1b[1;2C", Key{Code: uinput.KeyRight, Shift: true}},
		{"\x1b[1;3D", Key{Code: uinput.KeyLeft, Alt: true}},
		{"\x1b[3;5~", Key{Code: uinput.KeyDelete, Ctrl: true}},
		{"\x1b[15;2~", Key{Code: uinput.KeyF5, Shift: true}},
		{"\x1bx", Key{Code: uinput.KeyX, Alt: true}},
		{"\x1b\x06", Key{Code: uinput.KeyF, Alt: true, Ctrl: true}},
	}
	for _, c := range cases {
		got, ok := LookupKey([]byte(c.in))
		if !ok {
			t.Errorf("%q: not recognised", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "\x1b[99;9X", "\x80"} {
		if _, ok := LookupKey([]byte(bad)); ok {
			t.Errorf("%q: unexpectedly recognised", bad)
		}
	}
}

func TestKeyEvents(t *testing.T) {
	got := Key{Code: uinput.KeyA, Ctrl: true, Shift: true}.Events()
	want := []InputEvent{
		{Kind: KindKey, Code: uinput.KeyLeftctrl, Press: true},
		{Kind: KindKey, Code: uinput.KeyLeftshift, Press: true},
		{Kind: KindKey, Code: uinput.KeyA, Press: true},
		{Kind: KindKey, Code: uinput.KeyA, Press: false},
		{Kind: KindKey, Code: uinput.KeyLeftshift, Press: false},
		{Kind: KindKey, Code: uinput.KeyLeftctrl, Press: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestSplitSequences(t *testing.T) {
	in := "ab\x1b[A\x1b[1;5C\x1bOP\x1b[15~\x1bq\x1b"
	got := SplitSequences([]byte(in))
	want := []string{"a", "b", "\x1b[A", "\x1b[1;5C", "\x1bOP", "\x1b[15~", "\x1bq", "\x1b"}
	if len(got) != len(want) {
		t.Fatalf("got %d sequences %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("seq %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestReadTerminalInputPointerModeAndExit(t *testing.T) {
	// ctrl+] p  -> pointer mode; right, down -> moves; enter -> click;
	// ctrl+] p  -> back to keys; "h" -> key; ctrl+] q -> exit.
	in := "\x1dp\x1b[C\x1b[B\r\x1dph\x1dq"
	var got []InputEvent
	err := ReadTerminalInput(context.Background(), strings.NewReader(in), func(ev InputEvent) {
		got = append(got, ev)
	})
	if !errors.Is(err, ErrExitRequested) {
		t.Fatalf("err = %v, want ErrExitRequested", err)
	}
	want := []InputEvent{
		{Kind: KindMove, X: PointerStep, Y: 0},
		{Kind: KindMove, X: PointerStep, Y: PointerStep},
		{Kind: KindClick, Button: 1, Press: true},
		{Kind: KindClick, Button: 1, Press: false},
		{Kind: KindKey, Code: uinput.KeyH, Press: true},
		{Kind: KindKey, Code: uinput.KeyH, Press: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestReadTerminalInputEOF(t *testing.T) {
	var n int
	err := ReadTerminalInput(context.Background(), strings.NewReader("\x1b[A"), func(InputEvent) { n++ })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if n != 2 {
		t.Errorf("got %d events, want 2 (press+release)", n)
	}
}
