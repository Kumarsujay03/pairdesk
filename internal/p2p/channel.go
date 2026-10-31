package p2p

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

// Channel is a stream over one WebRTC data channel. Reads are stream-like
// (a message may be consumed across several Reads); Writes are chunked to
// stay under pion's message size limit and block while the send buffer is
// above the high-water mark.
type Channel interface {
	io.ReadWriteCloser
	Label() string
}

const (
	chunkSize    = 16 * 1024   // bytes per data channel message
	bufHighWater = 1024 * 1024 // pause writes above this much unsent data
	bufLowWater  = 256 * 1024  // resume writes once below this
)

type channel struct {
	dc  *webrtc.DataChannel
	raw datachannel.ReadWriteCloser

	rmu     sync.Mutex
	pending []byte // unread tail of the last message

	wmu     sync.Mutex
	lowCh   chan struct{}
	closed  chan struct{}
	closeMu sync.Once
	onClose func()
}

func newChannel(dc *webrtc.DataChannel, raw datachannel.ReadWriteCloser) *channel {
	c := &channel{dc: dc, raw: raw, lowCh: make(chan struct{}, 1), closed: make(chan struct{})}
	dc.SetBufferedAmountLowThreshold(bufLowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case c.lowCh <- struct{}{}:
		default:
		}
	})
	dc.OnClose(func() { c.markClosed() })
	return c
}

func (c *channel) Label() string { return c.dc.Label() }

func (c *channel) markClosed() {
	c.closeMu.Do(func() {
		close(c.closed)
		if c.onClose != nil {
			c.onClose()
		}
	})
}

func (c *channel) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		buf := make([]byte, 64*1024)
		n, err := c.raw.Read(buf)
		if n > 0 {
			c.pending = buf[:n]
			break
		}
		if err != nil {
			c.markClosed()
			if errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			return 0, err
		}
		// zero-length message: keep waiting
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *channel) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	for len(p) > 0 {
		if err := c.waitForRoom(); err != nil {
			return written, err
		}
		n := len(p)
		if n > chunkSize {
			n = chunkSize
		}
		if _, err := c.raw.Write(p[:n]); err != nil {
			c.markClosed()
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *channel) waitForRoom() error {
	for c.dc.BufferedAmount() > bufHighWater {
		select {
		case <-c.closed:
			return ErrClosed
		case <-c.lowCh:
		}
	}
	select {
	case <-c.closed:
		return ErrClosed
	default:
		return nil
	}
}

func (c *channel) Close() error {
	err := c.raw.Close()
	c.markClosed()
	return err
}

// openDetached waits for dc to open and returns its detached stream.
func openDetached(ctx context.Context, dc *webrtc.DataChannel, done <-chan struct{}) (*channel, error) {
	ready := make(chan *channel, 1)
	errc := make(chan error, 1)
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			errc <- err
			return
		}
		ready <- newChannel(dc, raw)
	})
	select {
	case ch := <-ready:
		return ch, nil
	case err := <-errc:
		return nil, err
	case <-ctx.Done():
		_ = dc.Close()
		return nil, ctx.Err()
	case <-done:
		return nil, ErrClosed
	}
}
