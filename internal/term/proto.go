// Package term hosts a PTY for a remote visitor over a p2p Channel and, on
// the visitor side, takes over the local terminal like ssh does.
package term

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Frame types on the wire. Each frame is: uint32 big-endian length of
// (type byte + payload), then the type byte, then the payload.
const (
	FrameData    byte = 0 // raw bytes, either direction
	FrameResize  byte = 1 // {cols,rows} as two uint16 big-endian, visitor→host
	FrameCommand byte = 2 // CommandEvent JSON, host→visitor
	FrameSudo    byte = 3 // {"cmd":...} JSON, host→visitor (informational)
	FrameShare   byte = 4 // one byte: 1=on 0=off; visitor→host requests, host→visitor announces
	FrameBye     byte = 5 // no payload; session over
)

const maxFrame = 1 << 20

var errFrameTooBig = errors.New("term: frame too large")

// CommandEvent is one command the visitor ran, reported by the shell hooks.
type CommandEvent struct {
	Time   time.Time `json:"time"`
	Cmd    string    `json:"cmd"`
	Cwd    string    `json:"cwd"`
	Exit   int       `json:"exit"`
	Sudo   bool      `json:"sudo"`
	Typist string    `json:"typist"` // "visitor" or "host"
}

// WriteFrame writes one frame. Safe for one writer at a time.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload)+1 > maxFrame {
		return errFrameTooBig
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)+1))
	buf[4] = typ
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return 0, nil, errors.New("term: empty frame")
	}
	if n > maxFrame {
		return 0, nil, errFrameTooBig
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return body[0], body[1:], nil
}

func encodeResize(cols, rows uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b
}

func decodeResize(p []byte) (cols, rows uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("term: bad resize payload")
	}
	return binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:]), nil
}

func encodeCommand(ev CommandEvent) []byte {
	b, _ := json.Marshal(ev)
	return b
}

func decodeCommand(p []byte) (CommandEvent, error) {
	var ev CommandEvent
	err := json.Unmarshal(p, &ev)
	return ev, err
}

func jsonUnmarshal(p []byte, v any) error { return json.Unmarshal(p, v) }
