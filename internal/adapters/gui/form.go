package gui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/nefercap/internal/ports"
)

// Form limits. Dimension, frame-size and fps bounds come from ports so the
// panel cannot accept what the capture pipeline rejects.
const maxDuration = 24 * time.Hour

// Validation messages are built once, so status rendering never allocates.
var (
	errOutput   = errors.New("! select an output")
	errMode     = errors.New("! select screenshot or record")
	errPath     = errors.New("! file must name a path (not empty, not a directory)")
	errRegion   = fmt.Errorf("! region must be X,Y,WxH with X,Y >= 0, W,H > 0 and X+W, Y+H <= %d, or empty", ports.MaxDimension)
	errFPS      = fmt.Errorf("! fps must be a whole number from 1 to %d", ports.MaxFPS)
	errSize     = fmt.Errorf("! resolution must be WxH with both from 1 to %d, or empty for source size", ports.MaxDimension)
	errSizeOdd  = errors.New("! resolution width and height must be even (video encoding)")
	errSizeSize = fmt.Errorf("! resolution is too large: at most %d MiB per frame", ports.MaxFrameBytes>>20)
	errDuration = errors.New("! duration must be seconds or e.g. 1m30s (max 24h); 0 = until Ctrl+C")

	fpsPlaceholder = "1-" + strconv.Itoa(ports.MaxFPS)
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
		fps, ok := parseUint(m.fps, ports.MaxFPS)
		if !ok || fps < 1 {
			return errFPS
		}
		w, h, ok := parseSize(m.size)
		if !ok {
			return errSize
		}
		if err := checkVideoSize(w, h); err != nil {
			return err
		}
		dur, ok := m.parsedDuration()
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

// checkVideoSize applies encoder rules to an explicit video resolution (zero
// means source size). Region sizes are not checked: captured physical
// dimensions can differ from the logical region by the output scale, so an odd
// region does not imply odd video. If the source itself is odd, the user must
// set an even resolution; the record note says so.
func checkVideoSize(w, h int) error {
	if w == 0 {
		return nil
	}
	if w%2 != 0 || h%2 != 0 {
		return errSizeOdd
	}
	if w > ports.MaxFrameBytes/ports.BytesPerPixel/h {
		return errSizeSize
	}
	return nil
}

// parsedDuration parses m.duration, remembering the last input and result so
// an unchanged (even invalid) field costs no allocation per frame.
func (m *model) parsedDuration() (time.Duration, bool) {
	if !m.durCached || m.durSrc != m.duration {
		m.durSrc = m.duration
		m.durVal, m.durOK = parseDuration(m.duration)
		m.durCached = true
	}
	return m.durVal, m.durOK
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
// It is deliberately stricter than the coordinate bound alone: the far edges
// X+W and Y+H must also stay within ports.MaxDimension. Whether the region fits
// the chosen output is only known to the capture adapter.
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
	x, ok1 := parseUint(xs, ports.MaxDimension)
	y, ok2 := parseUint(ys, ports.MaxDimension)
	w, h, ok3 := parseSize(size)
	if !ok1 || !ok2 || !ok3 || w == 0 || x+w > ports.MaxDimension || y+h > ports.MaxDimension {
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
	w, ok1 := parseUint(s[:i], ports.MaxDimension)
	h, ok2 := parseUint(s[i+1:], ports.MaxDimension)
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
