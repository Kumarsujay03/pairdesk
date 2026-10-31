package term

import (
	"bytes"
	"io"
	"sync"
)

// redactor copies a byte stream to w, dropping everything that follows a
// sudo password marker up to and including the next newline. The marker
// is what the sudo shim passes to real sudo as its prompt, so whatever the
// visitor types (or a misconfigured tty echoes) after it never reaches the
// transcript. The marker may arrive split across writes.
type redactor struct {
	mu        sync.Mutex
	w         io.Writer
	marker    []byte
	pending   []byte // tail that could be a marker prefix
	redacting bool
}

func newRedactor(w io.Writer, marker string) *redactor {
	return &redactor{w: w, marker: []byte(marker)}
}

func (r *redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := append(r.pending, p...)
	r.pending = nil
	var out []byte
	for len(data) > 0 {
		if r.redacting {
			i := bytes.IndexAny(data, "\r\n")
			if i < 0 {
				data = nil
				break
			}
			out = append(out, data[i])
			data = data[i+1:]
			r.redacting = false
			continue
		}
		i := bytes.Index(data, r.marker)
		if i >= 0 {
			out = append(out, data[:i+len(r.marker)]...)
			data = data[i+len(r.marker):]
			r.redacting = true
			continue
		}
		// Keep any tail that is a proper prefix of the marker.
		keep := 0
		for k := min(len(r.marker)-1, len(data)); k > 0; k-- {
			if bytes.HasPrefix(r.marker, data[len(data)-k:]) {
				keep = k
				break
			}
		}
		out = append(out, data[:len(data)-keep]...)
		r.pending = append([]byte{}, data[len(data)-keep:]...)
		data = nil
	}
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes any held-back prefix bytes.
func (r *redactor) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 || r.redacting {
		r.pending = nil
		return nil
	}
	_, err := r.w.Write(r.pending)
	r.pending = nil
	return err
}
