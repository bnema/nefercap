package selection

import (
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

// footerText is rebuilt only when the capture mode, picker kind or workspace
// availability changes; pointer motion never formats hints.
func footerText(mode ports.Mode, kind core.PickKind, workspace bool) string {
	text, action := "shot · R region · M monitor", "capture"
	if mode == ports.Record {
		text, action = "rec · R region · M monitor", "record"
	}
	if workspace {
		text += " · W workspace"
	}
	text += " · G grid · Esc cancel\n"
	switch kind {
	case core.PickMonitor:
		return text + "Enter " + action + " monitor · click " + action + " monitor"
	case core.PickWorkspace:
		return text + "Enter " + action + " workspace · click " + action + " workspace"
	default:
		return text + "drag + release " + action + " region · click " + action + " monitor"
	}
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
	fw := min(footerW, w)
	fh := min(footerHeight(m.footerText, fw), h)
	s.footer = frect{(w - fw) / 2, max(h-fh-margin, 0), fw, fh}
	return s
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
