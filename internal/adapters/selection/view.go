package selection

import (
	"time"

	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/ports"
)

// view draws the scene. Geometry goes through Node.Rect, so no CSS strings
// are built per frame; classes come from the embedded stylesheet. Boxes with no
// area are not emitted at all, so an idle surface is one dim box plus the
// header and footer, and no empty chip is painted.
func view(f *nefergui.Frame, m *model) {
	m.syncMode() // a Tab on another overlay redraws this one
	m.refreshFooter()
	m.animate(time.Now())
	root := f.Root(nefergui.Class("root"))
	st := root.Stack(nefergui.Class("scene"))
	if drawsBackdrop(m) {
		// The frozen screen, scaled to fill the logical surface.
		box := st.Box(nefergui.Key("backdrop"), nefergui.Class("backdrop")).Rect(0, 0, float64(m.w), float64(m.h))
		box.Image(m.backdrop)
	}
	s := m.scene()
	for i, r := range s.dim {
		box(st, dimKeys[i], "dim", r)
	}
	for i, r := range s.grid {
		box(st, gridKeys[i], "grid", r)
	}
	mc := modeClass(m.mode)
	for i, r := range s.edges {
		if r.w > 0 && r.h > 0 {
			st.Box(nefergui.Key(edgeKeys[i]), nefergui.Class("edge"), nefergui.Class(mc)).Rect(r.x, r.y, r.w, r.h)
		}
	}
	hc := m.hudClass()
	if n := tag(st, "label", "o0", s.label); n.ok {
		n.node.Text(m.label.text)
	}
	for i, r := range s.modes {
		chip(st, modeKeys[i], r, s.modeState[i], hc, badgeText(modeChips[i]))
	}
	for i, r := range s.kinds {
		chip(st, kindKeys[i], r, s.kindState[i], hc, targetTexts[i])
	}
	if n := tag(st, "header", hc, s.header); n.ok {
		n.node.Text(m.header)
	}
	if n := tag(st, "footer", hc, s.footer); n.ok {
		n.node.Text(m.footerText)
	}
	if r := s.toast; r.w > 0 && r.h > 0 {
		kind := "info"
		if m.toast.warn {
			kind = "warn"
		}
		st.Box(nefergui.Key("toast"), nefergui.Class("toast"), nefergui.Class(kind),
			nefergui.Class(m.toastClass())).Rect(r.x, r.y, r.w, r.h).Text(m.toast.text)
	}
}

// chip draws one selector chip; state is shot or rec (active, in the mode
// color), idle, or off (unavailable).
func chip(st nefergui.Node, key string, r frect, state, fade, text string) {
	if r.w > 0 && r.h > 0 {
		st.Box(nefergui.Key(key), nefergui.Class("tag"), nefergui.Class("chip"), nefergui.Class(state), nefergui.Class(fade)).Rect(r.x, r.y, r.w, r.h).Text(text)
	}
}

// drawsBackdrop: a screenshot selection shows the frozen screen; a recording
// selects on the live screen.
func drawsBackdrop(m *model) bool {
	return m.backdrop != nil && m.mode == ports.Screenshot && m.w > 0 && m.h > 0
}

type tagged struct {
	node nefergui.Node
	ok   bool
}

func box(st nefergui.Node, key, class string, r frect) {
	if r.w > 0 && r.h > 0 {
		st.Box(nefergui.Key(key), nefergui.Class(class)).Rect(r.x, r.y, r.w, r.h)
	}
}

// tag draws a text tag; fade is its opacity class (o0 is opaque).
func tag(st nefergui.Node, key, fade string, r frect) tagged {
	if r.w <= 0 || r.h <= 0 {
		return tagged{}
	}
	return tagged{st.Box(nefergui.Key(key), nefergui.Class("tag"), nefergui.Class(fade)).Rect(r.x, r.y, r.w, r.h), true}
}

// Static keys avoid building identity strings per frame.
var (
	dimKeys  = [4]string{"dim0", "dim1", "dim2", "dim3"}
	gridKeys = [6]string{"grid0", "grid1", "grid2", "grid3", "grid4", "grid5"}
	edgeKeys = [4]string{"edge0", "edge1", "edge2", "edge3"}
	modeKeys = [2]string{"mode-shot", "mode-rec"}
	kindKeys = [3]string{"kind-region", "kind-monitor", "kind-workspace"}
)
