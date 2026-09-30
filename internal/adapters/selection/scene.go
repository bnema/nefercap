package selection

import (
	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

// Fixed overlay element sizes in logical pixels.
const (
	// 4px tag padding leaves 18px for the default 13px monospace line.
	labelW, labelH   = 120.0, 26.0
	footerW, footerH = 400.0, 28.0
	headerH          = 28.0
	edge             = 1.0 // thin selection lines
	margin           = 16.0
)

// Footer hints; the first word names the mode.
const (
	footerShot = "shot · drag region · click monitor · Esc"
	footerRec  = "rec · drag region · click monitor · Esc"
)

func footerText(mode ports.Mode) string {
	if mode == ports.Record {
		return footerRec
	}
	return footerShot
}

// frect is a float rectangle in surface-local logical pixels, the unit
// Node.Rect takes.
type frect struct{ x, y, w, h float64 }

// scene is the complete geometry of one frame, derived from the picker. It is
// a plain value so computing it never allocates.
type scene struct {
	dim    [4]frect // top, bottom, left, right of the selection; dim[0] is all when none
	edges  [4]frect // top, bottom, left, right lines
	grid   [4]frect // two vertical then two horizontal thirds lines
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
	if m.picker.Grid() {
		g := frect{0, 0, w, h}
		if outline {
			g = fr
		}
		s.grid = [4]frect{
			{g.x + g.w/3, g.y, edge, g.h},
			{g.x + 2*g.w/3, g.y, edge, g.h},
			{g.x, g.y + g.h/3, g.w, edge},
			{g.x, g.y + 2*g.h/3, g.w, edge},
		}
	}
	if m.label.text != "" && outline {
		s.label = labelRect(fr, w, h)
	}
	s.header = frect{margin, margin, headerWidth(m.header), headerH}
	s.footer = frect{(w - footerW) / 2, h - footerH - margin, footerW, footerH}
	if w < footerW+2*margin { // narrow surface: keep the hints on screen
		s.footer.x, s.footer.w = 0, w
	}
	return s
}

// headerWidth is a monospace estimate of the header text plus padding.
func headerWidth(text string) float64 {
	return float64(len([]rune(text)))*8 + 20
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
