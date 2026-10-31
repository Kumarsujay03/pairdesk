// Package screen captures the host's screen as a VP8 RTP stream (host side)
// and renders a remote VP8 track in a window (viewer side). Both ends shell
// out to gst-launch-1.0 over UDP loopback so the binary stays cgo-free.
//
// Control-mode input events (viewer -> host) are defined here too so that the
// viewer, the data channel and internal/input share one wire format.
package screen

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// InputEvent kinds.
const (
	KindKey    = "key"    // Code = Linux KEY_* code, Press = down/up
	KindMove   = "move"   // X, Y = absolute pointer position in host pixels
	KindClick  = "click"  // Button = 1 left, 2 right, 3 middle; Press = down/up
	KindScroll = "scroll" // Delta = wheel ticks, positive = up/away
)

// InputEvent is one keyboard or pointer action sent from the viewer to the
// host while control mode is active.
type InputEvent struct {
	Kind   string // key|move|click|scroll
	Code   int    // Linux input event code for Kind == "key"
	Press  bool   // true = press/down, false = release/up (key, click)
	X, Y   int32  // absolute pointer position for Kind == "move"
	Button int    // 1 left, 2 right, 3 middle for Kind == "click"
	Delta  int    // wheel ticks for Kind == "scroll"
}

// InputEventSize is the fixed encoded size of an InputEvent in bytes.
const InputEventSize = 20

const (
	kindCodeKey uint8 = iota + 1
	kindCodeMove
	kindCodeClick
	kindCodeScroll
)

var kindToCode = map[string]uint8{
	KindKey:    kindCodeKey,
	KindMove:   kindCodeMove,
	KindClick:  kindCodeClick,
	KindScroll: kindCodeScroll,
}

var codeToKind = map[uint8]string{
	kindCodeKey:    KindKey,
	kindCodeMove:   KindMove,
	kindCodeClick:  KindClick,
	kindCodeScroll: KindScroll,
}

// ErrBadInput is returned by DecodeInput for malformed bytes.
var ErrBadInput = errors.New("screen: malformed input event")

// EncodeInput serialises ev into a compact fixed-size (InputEventSize) buffer:
//
//	byte 0     kind (1 key, 2 move, 3 click, 4 scroll)
//	byte 1     press flag (0/1)
//	byte 2     button
//	byte 3     reserved (0)
//	bytes 4-7  code   (int32 little endian)
//	bytes 8-11 x      (int32 little endian)
//	bytes 12-15 y     (int32 little endian)
//	bytes 16-19 delta (int32 little endian)
//
// Unknown kinds encode as kind 0, which DecodeInput rejects.
func EncodeInput(ev InputEvent) []byte {
	b := make([]byte, InputEventSize)
	b[0] = kindToCode[ev.Kind]
	if ev.Press {
		b[1] = 1
	}
	b[2] = uint8(ev.Button)
	binary.LittleEndian.PutUint32(b[4:], uint32(int32(ev.Code)))
	binary.LittleEndian.PutUint32(b[8:], uint32(ev.X))
	binary.LittleEndian.PutUint32(b[12:], uint32(ev.Y))
	binary.LittleEndian.PutUint32(b[16:], uint32(int32(ev.Delta)))
	return b
}

// DecodeInput parses a buffer produced by EncodeInput. Trailing bytes beyond
// InputEventSize are ignored so a datagram can carry padding.
func DecodeInput(b []byte) (InputEvent, error) {
	if len(b) < InputEventSize {
		return InputEvent{}, fmt.Errorf("%w: %d bytes, want %d", ErrBadInput, len(b), InputEventSize)
	}
	kind, ok := codeToKind[b[0]]
	if !ok {
		return InputEvent{}, fmt.Errorf("%w: unknown kind %d", ErrBadInput, b[0])
	}
	if b[1] > 1 {
		return InputEvent{}, fmt.Errorf("%w: bad press flag %d", ErrBadInput, b[1])
	}
	return InputEvent{
		Kind:   kind,
		Press:  b[1] == 1,
		Button: int(b[2]),
		Code:   int(int32(binary.LittleEndian.Uint32(b[4:]))),
		X:      int32(binary.LittleEndian.Uint32(b[8:])),
		Y:      int32(binary.LittleEndian.Uint32(b[12:])),
		Delta:  int(int32(binary.LittleEndian.Uint32(b[16:]))),
	}, nil
}
