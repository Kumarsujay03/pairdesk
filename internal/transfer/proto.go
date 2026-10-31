// Package transfer implements PairDesk's torrent-style file drop (PAIRDESK.md
// section 4.6). A file is described by a Manifest (whole-file hash plus one
// hash per fixed-size chunk). A seed serves chunks on request over a peer
// channel; a receiver pulls every missing chunk, verifies each against the
// manifest, writes them into a sparse ".part" file with a ".part.json"
// bitmap sidecar so an interrupted transfer resumes, and renames the file
// into place once the whole-file hash checks out. A receiver that has
// finished (or even one still in progress) can Serve the same manifest, so
// every group member becomes a seed and late joiners pull rarest-first from
// whichever seeds are online.
//
// # Wire protocol
//
// The channel is a byte stream with no message boundaries, so every message
// is a length-prefixed frame:
//
//	+----------------+-----------+---------------------+
//	| length (u32 BE)| type (u8) | payload (length-1 B)|
//	+----------------+-----------+---------------------+
//
// length counts the type byte plus the payload and is never larger than
// MaxFrame (1 + 4 + ChunkSize); a frame claiming more is a protocol error
// and the connection is dropped, so no allocation is ever sized from an
// untrusted number beyond that bound.
//
// Frame types (receiver R, seed S):
//
//	HELLO  R->S  payload: manifest ID (UTF-8, at most MaxIDLen bytes).
//	              Always the first frame. The seed refuses (ERR, close) if
//	              the ID is not the manifest it is serving.
//	HAVE   S->R  payload: bitmap, ceil(NumChunks/8) bytes, chunk i at
//	              byte i/8 bit (i%8) LSB-first. Which chunks the seed holds.
//	REQ    R->S  payload: chunk index (u32 BE).
//	DATA   S->R  payload: chunk index (u32 BE) followed by the chunk bytes.
//	              The receiver verifies the length and SHA-256 against the
//	              manifest and discards anything that does not match.
//	DONE   R->S  no payload. The receiver has everything; the seed returns.
//	ERR    both  payload: UTF-8 text (at most MaxErrLen bytes). Fatal for
//	              this connection.
//
// A session is: R sends HELLO, S answers HAVE (or ERR), R sends REQs with up
// to Parallel outstanding per seed, S answers each with DATA, R sends DONE.
package transfer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Frame types.
const (
	typeHello byte = 1
	typeHave  byte = 2
	typeReq   byte = 3
	typeData  byte = 4
	typeDone  byte = 5
	typeErr   byte = 6
)

const (
	// MaxFrame bounds the length field: type byte + index + one chunk.
	MaxFrame = 1 + 4 + ChunkSize
	// MaxIDLen bounds a HELLO payload.
	MaxIDLen = 64
	// MaxErrLen bounds an ERR payload.
	MaxErrLen = 1024
)

// ErrProtocol is returned when the peer violates the wire protocol.
var ErrProtocol = errors.New("transfer: protocol error")

type frame struct {
	typ     byte
	payload []byte
}

// frameReader decodes frames from a stream. One per channel; it buffers.
type frameReader struct {
	r *bufio.Reader
}

func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(r, 64*1024)}
}

func (fr *frameReader) read() (frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 1 || n > MaxFrame {
		return frame{}, fmt.Errorf("%w: frame length %d", ErrProtocol, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(fr.r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return frame{}, err
	}
	f := frame{typ: buf[0], payload: buf[1:]}
	switch f.typ {
	case typeHello:
		if len(f.payload) > MaxIDLen {
			return frame{}, fmt.Errorf("%w: HELLO too long", ErrProtocol)
		}
	case typeReq:
		if len(f.payload) != 4 {
			return frame{}, fmt.Errorf("%w: bad REQ", ErrProtocol)
		}
	case typeData:
		if len(f.payload) < 4 {
			return frame{}, fmt.Errorf("%w: bad DATA", ErrProtocol)
		}
	case typeDone:
		if len(f.payload) != 0 {
			return frame{}, fmt.Errorf("%w: bad DONE", ErrProtocol)
		}
	case typeErr:
		if len(f.payload) > MaxErrLen {
			return frame{}, fmt.Errorf("%w: ERR too long", ErrProtocol)
		}
	case typeHave:
	default:
		return frame{}, fmt.Errorf("%w: unknown frame type %d", ErrProtocol, f.typ)
	}
	return f, nil
}

// writeFrame encodes one frame and writes it with a single Write call so
// that transports with message semantics see one message per frame.
func writeFrame(w io.Writer, typ byte, parts ...[]byte) error {
	n := 1
	for _, p := range parts {
		n += len(p)
	}
	if n > MaxFrame {
		return fmt.Errorf("%w: frame too large", ErrProtocol)
	}
	buf := make([]byte, 4+n)
	binary.BigEndian.PutUint32(buf, uint32(n))
	buf[4] = typ
	off := 5
	for _, p := range parts {
		off += copy(buf[off:], p)
	}
	_, err := w.Write(buf)
	return err
}

func writeErr(w io.Writer, text string) error {
	if len(text) > MaxErrLen {
		text = text[:MaxErrLen]
	}
	return writeFrame(w, typeErr, []byte(text))
}

func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// packBits encodes a bool slice LSB-first.
func packBits(bits []bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		if b {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

// unpackBits decodes exactly n bits; the byte length must match.
func unpackBits(b []byte, n int) ([]bool, bool) {
	if len(b) != (n+7)/8 {
		return nil, false
	}
	out := make([]bool, n)
	for i := range out {
		out[i] = b[i/8]&(1<<(i%8)) != 0
	}
	return out, true
}
