package selection

import (
	"strings"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

// Node.Rect takes logical pixels, not CSS units. Keep geometry in em
// multiples of the stylesheet's 1rem base (NeferGUI defaults to 16px).
const (
	baseFont         = 16.0
	tagPadY          = baseFont * 0.25
	tagPadX          = baseFont * 0.625
	labelW, labelH   = baseFont * 9, baseFont * 2
	footerW, footerH = baseFont * 40, baseFont * 3.5
	headerH          = labelH
	edge             = 1.0 // thin selection lines
	margin           = baseFont
)

// noIndicatorHint is shown in record mode on a compositor that cannot keep a
// recording indicator out of the video, so none is drawn.
const noIndicatorHint = "No recording indicator on this compositor · stop with the same shortcut or nefercap stop"

// footerText is rebuilt only when the capture mode, picker kind or workspace
// availability changes; pointer motion never formats hints.
func footerText(mode ports.Mode, kind core.PickKind, workspace, toggle, noIndicator bool) string {
	action, name, other := "capture", "shot", "rec"
	if mode == ports.Record {
		action, name, other = "record", "rec", "shot"
	}
	var b strings.Builder
	b.Grow(192 + len(noIndicatorHint)) // one allocation for the whole legend
	b.WriteString(name)
	if toggle {
		b.WriteString(" · Tab ")
		b.WriteString(other)
	}
	b.WriteString(" · R region · M monitor")
	if workspace {
		b.WriteString(" · W workspace")
	}
	b.WriteString(" · G grid · Esc cancel\n")
	switch kind {
	case core.PickMonitor, core.PickWorkspace:
		target := " monitor"
		if kind == core.PickWorkspace {
			target = " workspace"
		}
		b.WriteString("Enter ")
		b.WriteString(action)
		b.WriteString(target)
		b.WriteString(" · click ")
		b.WriteString(action)
		b.WriteString(target)
	default:
		b.WriteString("drag + release ")
		b.WriteString(action)
		b.WriteString(" region · click ")
		b.WriteString(action)
		b.WriteString(" monitor")
	}
	if noIndicator && mode == ports.Record {
		b.WriteByte('\n')
		b.WriteString(noIndicatorHint)
	}
	return b.String()
}

// badgeText is the always-visible mode badge. Both modes share the neutral
// tag style: a red badge would read as an active recording.
func badgeText(mode ports.Mode) string {
	if mode == ports.Record {
		return "REC"
	}
	return "SHOT"
}

// frect is a float rectangle in surface-local logical pixels, the unit
// Node.Rect takes.
type frect struct{ x, y, w, h float64 }

// scene is the complete geometry of one frame, derived from the picker. It is
// a plain value so computing it never allocates.
type scene struct {
	dim    [4]frect                  // top, bottom, left, right of the selection; dim[0] is all when none
	edges  [4]frect                  // top, bottom, left, right lines
	grid   [core.GridLineCount]frect // whole-monitor guides: three vertical then three horizontal
	label  frect
	header frect
	badge  frect // top-left mode badge
	footer frect
}

// highlight returns the rectangle to outline and whether the rest is dimmed.
// A drag highlights the region and dims around it. The monitor kind outlines
// the whole output without dimming. Otherwise the whole output is dimmed.
func highlight(p *core.Picker) (r ports.Region, outline, dim bool) {
	w, h := p.Bounds()
	if r, ok := p.Rect(); ok {
		return r, true, true
	}
	switch p.Kind() {
	case core.PickMonitor:
		return ports.Region{Width: w, Height: h}, true, false
	case core.PickWorkspace:
		if _, r, ok := p.Workspace(); ok {
			return r, true, true
		}
	}
	return ports.Region{Width: w, Height: h}, false, true
}

func (m *model) scene() scene {
	var s scene
	w, h := float64(m.w), float64(m.h)
	r, outline, dim := highlight(&m.picker)
	fr := frect{float64(r.X), float64(r.Y), float64(r.Width), float64(r.Height)}
	switch {
	case outline && dim:
		s.dim = [4]frect{
			{0, 0, w, fr.y},
			{0, fr.y + fr.h, w, h - fr.y - fr.h},
			{0, fr.y, fr.x, fr.h},
			{fr.x + fr.w, fr.y, w - fr.x - fr.w, fr.h},
		}
	case dim:
		s.dim[0] = frect{0, 0, w, h}
	}
	if outline {
		s.edges = [4]frect{
			{fr.x, fr.y, fr.w, edge},
			{fr.x, fr.y + fr.h - edge, fr.w, edge},
			{fr.x, fr.y, edge, fr.h},
			{fr.x + fr.w - edge, fr.y, edge, fr.h},
		}
	}
	if m.grid {
		s.grid = m.gridLines // the whole monitor, whatever is being dragged
	}
	if m.label.text != "" && outline {
		s.label = labelRect(fr, w, h)
	}
	hw, hh := min(headerWidth(m.header), w), min(headerH, h)
	s.header = frect{max(w-hw-margin, 0), min(margin, max(h-hh, 0)), hw, hh}
	s.placeBadge(m.mode, w, h)
	// Keep workspace dimensions clear of the output header; region and
	// monitor labels retain their independent placement.
	if m.picker.Kind() == core.PickWorkspace && s.label.w > 0 &&
		s.label.x < s.header.x+s.header.w && s.label.x+s.label.w > s.header.x &&
		s.label.y < s.header.y+s.header.h && s.label.y+s.label.h > s.header.y {
		y := max(fr.y+tagPadY, s.header.y+s.header.h+tagPadY)
		if y+s.label.h <= h {
			s.label.y = y
		}
	}
	s.clearLabelOfBadge(fr, h) // the header move can bring the label back to it
	fw := min(footerW, w)
	fh := min(footerHeight(m.footerText, fw), h)
	s.footer = frect{(w - fw) / 2, max(h-fh-margin, 0), fw, fh}
	return s
}

// placeBadge puts the mode badge top-left; on an output too narrow to share
// the row with the header it moves below it.
func (s *scene) placeBadge(mode ports.Mode, w, h float64) {
	bw, bh := min(headerWidth(badgeText(mode)), w), min(headerH, h)
	s.badge = frect{min(margin, max(w-bw, 0)), min(margin, max(h-bh, 0)), bw, bh}
	if overlaps(s.badge, s.header) && s.header.y+s.header.h+tagPadY+bh <= h {
		s.badge.y = s.header.y + s.header.h + tagPadY
	}
	s.clearLabelOfBadge(frect{}, h)
}

// clearLabelOfBadge moves a size label that meets the badge below it, inside
// the highlighted rectangle fr when known.
func (s *scene) clearLabelOfBadge(fr frect, h float64) {
	if s.label.w > 0 && overlaps(s.label, s.badge) {
		if y := max(fr.y+tagPadY, s.badge.y+s.badge.h+tagPadY); y+s.label.h <= h {
			s.label.y = y
		}
	}
}

func overlaps(a, b frect) bool {
	return a.x < b.x+b.w && a.x+a.w > b.x && a.y < b.y+b.h && a.y+a.h > b.y
}

// footerHeight counts every explicit row separately, reserving conservative
// monospace cells and metric lines for wrapping. Range counts runes (including
// the middle-dot separators) without allocating per scene.
func footerHeight(text string, width float64) float64 {
	cells := max(int((width-2*tagPadX)/(baseFont*0.625)), 1)
	lines, count := 0, 0
	for _, r := range text {
		if r == '\n' {
			lines += max((count+cells-1)/cells, 1)
			count = 0
		} else {
			count++
		}
	}
	lines += max((count+cells-1)/cells, 1)
	return max(footerH, float64(lines)*baseFont*1.5+2*tagPadY)
}

// headerWidth is a monospace estimate of the header text plus padding.
func headerWidth(text string) float64 {
	return float64(len([]rune(text)))*baseFont*0.625 + 2*tagPadX
}

// labelRect places the size label above the region, or just inside it when
// there is no room above, and keeps it on the surface.
func labelRect(r frect, w, h float64) frect {
	x, y := r.x, r.y-labelH-4
	if y < 0 {
		x, y = r.x+4, r.y+4
	}
	x = min(max(x, 0), max(w-labelW, 0))
	y = min(max(y, 0), max(h-labelH, 0))
	return frect{x, y, labelW, labelH}
}
