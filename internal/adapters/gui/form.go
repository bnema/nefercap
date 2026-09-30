package gui

import (
	"errors"
	"strings"
	"time"

	"github.com/bnema/nefercap/internal/ports"
)

// Form limits. Bounds are generous but keep every value far from int overflow.
const (
	maxDim      = 16384
	maxFPS      = 240
	maxDuration = 24 * time.Hour
)

// Validation messages are static so status rendering never allocates.
var (
	errOutput   = errors.New("! select an output")
	errMode     = errors.New("! select screenshot or record")
	errPath     = errors.New("! file must name a path (not empty, not a directory)")
	errRegion   = errors.New("! region must be X,Y,WxH with X,Y >= 0 and W,H > 0, or empty")
	errFPS      = errors.New("! fps must be a whole number from 1 to 240")
	errSize     = errors.New("! size must be WxH with both > 0, or empty for source size")
	errDuration = errors.New("! duration must be seconds or e.g. 1m30s (max 24h); 0 = until Ctrl+C")
)

// parse validates the form and, on success, stores the result in m.sel. It
// reuses m's storage and does not allocate for valid or invalid input.
func (m *model) parse() error {
	oi := m.outputIndex()
	if oi < 0 {
		return errOutput
	}
	path := strings.TrimSpace(m.path)
	if path == "" || strings.HasSuffix(path, "/") {
		return errPath
	}
	region, ok := parseRegion(m.region)
	if !ok {
		return errRegion
	}
	sel := ports.Selection{
		Target: ports.Target{OutputID: m.outputs[oi].id, Region: region},
		Path:   path,
	}
	switch m.mode {
	case modeScreenshot:
		sel.Mode = ports.Screenshot
	case modeRecord:
		sel.Mode = ports.Record
		fps, ok := parseUint(m.fps, maxFPS)
		if !ok || fps < 1 {
			return errFPS
		}
		w, h, ok := parseSize(m.size)
		if !ok {
			return errSize
		}
		dur, ok := parseDuration(m.duration)
		if !ok {
			return errDuration
		}
		sel.Video = ports.VideoSettings{FPS: fps, Width: w, Height: h}
		sel.Duration = dur
	default:
		return errMode
	}
	m.sel = sel
	return nil
}

// parseUint parses plain decimal digits no greater than limit.
func parseUint(s string, limit int) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > limit {
			return 0, false
		}
	}
	return n, true
}

// parseRegion parses "X,Y,WxH". Empty means the full output (zero Region).
func parseRegion(s string) (ports.Region, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ports.Region{}, true
	}
	xs, rest, ok := strings.Cut(s, ",")
	if !ok {
		return ports.Region{}, false
	}
	ys, size, ok := strings.Cut(rest, ",")
	if !ok {
		return ports.Region{}, false
	}
	x, ok1 := parseUint(xs, maxDim)
	y, ok2 := parseUint(ys, maxDim)
	w, h, ok3 := parseSize(size)
	if !ok1 || !ok2 || !ok3 || w == 0 {
		return ports.Region{}, false
	}
	return ports.Region{X: x, Y: y, Width: w, Height: h}, true
}

// parseSize parses "WxH" with both positive, or empty as 0x0.
func parseSize(s string) (w, h int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, true
	}
	i := strings.IndexAny(s, "xX")
	if i < 0 {
		return 0, 0, false
	}
	w, ok1 := parseUint(s[:i], maxDim)
	h, ok2 := parseUint(s[i+1:], maxDim)
	if !ok1 || !ok2 || w < 1 || h < 1 {
		return 0, 0, false
	}
	return w, h, true
}

// parseDuration accepts whole seconds or a Go duration; empty means zero.
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	if n, ok := parseUint(s, int(maxDuration/time.Second)); ok {
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || d > maxDuration {
		return 0, false
	}
	return d, true
}
