package supervisor

import (
	"strings"
	"sync"
)

// ring keeps the last N lines a worker wrote, for `cyprid logs` and error reports.
type ring struct {
	mu      sync.Mutex
	lines   []string
	max     int
	partial string
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := strings.Split(r.partial+string(b), "\n")
	r.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		// Progress bars redraw with \r; keep only the final state of the line.
		if i := strings.LastIndex(strings.TrimRight(line, "\r"), "\r"); i >= 0 {
			line = line[i+1:]
		}
		r.lines = append(r.lines, strings.TrimRight(line, "\r"))
	}
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = append([]string(nil), r.lines[over:]...)
	}
	return len(b), nil
}

func (r *ring) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := r.lines
	if r.partial != "" {
		lines = append(append([]string(nil), lines...), r.partial)
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]string(nil), lines...)
}

func (r *ring) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines, r.partial = nil, ""
}
