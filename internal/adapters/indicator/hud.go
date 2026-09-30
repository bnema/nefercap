package indicator

import (
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// prefix starts the recording label; the elapsed time follows it.
const prefix = "● REC "

// maxLabelRunes bounds the optional target label so the line fits beside the
// Stop button: 228 px of 13 px monospace is about 29 cells, and "● REC h:mm:ss · "
// takes 16.
const maxLabelRunes = 12

// suffixFor returns " · label" with the label cut to maxLabelRunes (ending in
// an ellipsis when cut), or "" for an empty label. Anything that is not a
// printable character is dropped, so a workspace name cannot break the single
// line: controls, unassigned and private-use code points, the line and
// paragraph separators U+2028 and U+2029, and the bidirectional controls
// (Unicode Bidi_Control) that could reorder the timer beside it.
func suffixFor(label string) string {
	var r [maxLabelRunes + 1]rune
	n := 0
	for _, c := range label {
		if keepRune(c) {
			r[n] = c
			n++
			if n > maxLabelRunes {
				r[maxLabelRunes-1] = '…'
				n = maxLabelRunes
				break
			}
		}
	}
	if n == 0 {
		return ""
	}
	// Encode prefix and label together: a long rune-to-string conversion
	// followed by concatenation would allocate an intermediate string too.
	var buf [len(" · ") + maxLabelRunes*utf8.UTFMax]byte
	b := buf[:copy(buf[:], " · ")]
	for _, c := range r[:n] {
		b = utf8.AppendRune(b, c)
	}
	return string(b)
}

// keepRune reports whether c may appear in the label.
func keepRune(c rune) bool {
	if c == 0x2028 || c == 0x2029 || unicode.Is(unicode.Bidi_Control, c) {
		return false
	}
	// IsPrint admits only letters, marks, numbers, punctuation, symbols and the
	// ASCII space; other spaces, controls and format characters are dropped.
	return unicode.IsPrint(c)
}

// hud is the indicator model. The view and every write run on the NeferGUI
// owner loop, so nothing is locked. The elapsed time is computed from the
// monotonic clock reading the view passes to refresh, never from a counter
// another goroutine advances. start carries a monotonic reading (time.Now), so
// wall-clock steps do not move the timer.
type hud struct {
	start time.Time
	stop  chan<- struct{}

	suffix string // fixed for the session, built once
	secs   int64  // seconds shown in text; -1 before the first view
	text   string
	buf    []byte
}

func newHUD(start time.Time, stop chan<- struct{}, label string) *hud {
	return &hud{start: start, stop: stop, suffix: suffixFor(label), secs: -1}
}

// refresh returns the label for the reading now, rebuilding it only when the
// displayed second changed. A reading before start shows zero.
func (h *hud) refresh(now time.Time) string {
	s := int64(now.Sub(h.start) / time.Second)
	if s < 0 {
		s = 0
	}
	if s == h.secs {
		return h.text
	}
	h.secs = s
	h.buf = append(appendElapsed(append(h.buf[:0], prefix...), s), h.suffix...)
	h.text = string(h.buf)
	return h.text
}

// requestStop notifies the owner without blocking. A full channel already
// holds a pending stop request.
func (h *hud) requestStop() {
	select {
	case h.stop <- struct{}{}:
	default:
	}
}

// appendElapsed appends mm:ss, or h:mm:ss from one hour.
func appendElapsed(b []byte, secs int64) []byte {
	h, m, s := secs/3600, secs/60%60, secs%60
	if h > 0 {
		b = strconv.AppendInt(b, h, 10)
		b = append(b, ':')
	}
	b = appendTwo(b, m)
	b = append(b, ':')
	return appendTwo(b, s)
}

func appendTwo(b []byte, n int64) []byte {
	return append(b, byte('0'+n/10), byte('0'+n%10))
}
