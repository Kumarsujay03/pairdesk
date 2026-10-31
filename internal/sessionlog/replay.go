package sessionlog

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Replay writes a transcript to w with its original timing (divided by
// speed) until ctx is done or stop is closed.
func Replay(ctx context.Context, dir string, w io.Writer, speed float64, stop <-chan struct{}) error {
	f, err := os.Open(filepath.Join(dir, "transcript.cast"))
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if _, err := r.ReadBytes('\n'); err != nil { // header
		return err
	}
	last := 0.0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var ev []any
			if json.Unmarshal(line, &ev) == nil && len(ev) == 3 {
				t, _ := ev[0].(float64)
				kind, _ := ev[1].(string)
				data, _ := ev[2].(string)
				delay := time.Duration((t - last) / speed * float64(time.Second))
				if delay > 2*time.Second {
					delay = 2 * time.Second
				}
				last = t
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-stop:
					return nil
				case <-time.After(delay):
				}
				if kind == "o" {
					if _, err := io.WriteString(w, data); err != nil {
						return err
					}
				}
			}
		}
		if err != nil {
			return nil
		}
	}
}
