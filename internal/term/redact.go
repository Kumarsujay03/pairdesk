package term

import (
	"bytes"
	"io"
	"sync"
)

// Password prompt markers. Each sudo invocation the host allows gets a
// fresh marker, passed to real sudo as its prompt (-p). The marker is an
// OSC sequence terminals ignore, so the visitor sees a normal prompt
// while the redactor can spot it reliably and drop whatever follows on
// that line. Only markers the host issued are honoured: the visitor can
// print the prefix with any hex it likes and nothing is hidden.
const (
	markerPrefix = "\x1b]pairdesk-sudo-"
	markerHexLen = 12
	markerSuffix = "\x07[sudo] password for host: "
	markerLen    = len(markerPrefix) + markerHexLen + len(markerSuffix)
)

// redactor copies a byte stream to w, dropping everything that follows a
// live marker up to and including the next newline. The marker may
// arrive split across writes.
type redactor struct {
	mu        sync.Mutex
	w         io.Writer
	live      map[string]bool
	pending   []byte // tail that could be a marker prefix
	redacting bool
}

func newRedactor(w io.Writer) *redactor {
	return &redactor{w: w, live: map[string]bool{}}
}

// issue makes m a live marker; retire forgets it. Markers that are not
// live are passed through as ordinary output.
func (r *redactor) issue(m string) {
	r.mu.Lock()
	r.live[m] = true
	r.mu.Unlock()
}

func (r *redactor) retire(m string) {
	r.mu.Lock()
	delete(r.live, m)
	r.mu.Unlock()
}

func (r *redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := append(r.pending, p...)
	r.pending = nil
	prefix := []byte(markerPrefix)
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
		i := bytes.Index(data, prefix)
		if i >= 0 {
			if len(data)-i < markerLen {
				// Possibly a marker cut short by the write boundary.
				out = append(out, data[:i]...)
				r.pending = append([]byte{}, data[i:]...)
				data = nil
				break
			}
			cand := string(data[i : i+markerLen])
			if r.live[cand] {
				out = append(out, data[:i+markerLen]...)
				data = data[i+markerLen:]
				r.redacting = true
			} else {
				out = append(out, data[:i+len(prefix)]...)
				data = data[i+len(prefix):]
			}
			continue
		}
		// Keep any tail that is a proper prefix of the marker prefix.
		keep := 0
		for k := min(len(prefix)-1, len(data)); k > 0; k-- {
			if bytes.HasPrefix(prefix, data[len(data)-k:]) {
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

// Flush writes any held-back bytes that did not turn into a live marker.
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
