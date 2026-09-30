package selection

import (
	"fmt"
	"math"
	"strconv"

	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

// btnLeft is the Linux evdev BTN_LEFT code. The picker models the left button.
const btnLeft = 0x110

type endKind uint8

const (
	endNone     endKind = iota
	endAccepted         // this overlay recorded the accepted pick in the session
	endCanceled         // Escape, or a failure
)

// model is the state of one output's selector overlay. All methods run on that
// overlay's NeferGUI owner loop (OnResize, OnInput and the view), so nothing
// here is locked. The decision leaves through the shared session, never through
// this struct.
type model struct {
	mode    ports.Mode
	outputs []ports.Output
	index   int // the output this overlay covers; every output has its own model

	picker core.Picker
	sized  bool
	w, h   int
	scale  float64 // fractional output scale from OnResize; 0 before the first resize

	// The monitor grid belongs to the overlay, not the picker: Begin resets
	// the picker on a size change but the user's toggle must survive it.
	grid      bool
	gridLines [core.GridLineCount]frect // whole-monitor guides, logical units

	end  endKind
	sess *session // shared first-decision record; the only cross-overlay state

	workspace       ports.Workspace // zero ID: none
	header          string          // static per surface
	footerText      string
	footerKind      core.PickKind
	footerWorkspace bool
	label           label
}

func newModel(mode ports.Mode, outputs []ports.Output, index int, sess *session, grid bool) *model {
	return &model{mode: mode, outputs: outputs, index: index, sess: sess, grid: grid, header: headerText(outputs, index), footerText: footerText(mode, core.PickRegion, false)}
}

// resize is the OnResize callback. A logical size change (re)starts the
// picker; a scale-only change keeps the current pick. Either way the grid is
// laid out again on this output's physical pixels.
func (m *model) resize(w, h int, scale float64) {
	if m.sized && w == m.w && h == m.h {
		m.scale = scale
		m.layoutGrid()
		return
	}
	if err := m.picker.Begin(m.outputs[m.index].ID, w, h); err != nil {
		m.fail(fmt.Errorf("resize to %dx%d: %w", w, h, err))
		return
	}
	m.sized, m.w, m.h, m.scale = true, w, h, scale
	m.label.clear()
	m.picker.SetWorkspace(m.workspace.ID, m.workspace.Region)
	m.refreshFooter()
	m.layoutGrid()
}

// layoutGrid derives the logical guide rectangles from exact physical lines:
// snapping happens in physical pixels, and only then is each edge divided by
// the scale, so the renderer's edge rounding returns the same whole pixels. An
// unusable scale draws no grid instead of ending the selection.
//
// The lines follow the output mode when it explains the surface, because that
// is the monitor the user sees: a fractional scale rounds the logical size, so
// the surface buffer can be a pixel larger than the mode and the compositor
// crops that pixel. Otherwise (unknown, stale or unrelated mode) they follow
// the surface buffer, core.MonitorGrid.
func (m *model) layoutGrid() {
	m.gridLines = [core.GridLineCount]frect{}
	lines, err := m.physicalGrid()
	if err != nil {
		return
	}
	for i, r := range lines {
		m.gridLines[i] = frect{
			float64(r.Min.X) / m.scale, float64(r.Min.Y) / m.scale,
			float64(r.Dx()) / m.scale, float64(r.Dy()) / m.scale,
		}
	}
}

// physicalGrid is the grid in physical pixels; see layoutGrid.
func (m *model) physicalGrid() (core.GridLines, error) {
	buffer, err := core.MonitorGrid(m.w, m.h, m.scale)
	if err != nil {
		return buffer, err
	}
	bw, _ := core.PhysicalExtent(m.w, m.scale)
	bh, _ := core.PhysicalExtent(m.h, m.scale)
	out := m.outputs[m.index]
	// A rotated output reports its mode unrotated, so try both orientations.
	for _, mode := range [2][2]int{{out.Width, out.Height}, {out.Height, out.Width}} {
		if modeFits(mode[0], m.w, bw, m.scale) && modeFits(mode[1], m.h, bh, m.scale) {
			if lines, err := core.MonitorGridPhysical(mode[0], mode[1]); err == nil {
				return lines, nil
			}
		}
	}
	return buffer, nil
}

// modeFits reports whether a physical mode extent is what the compositor shows
// of a surface axis: the logical size is the mode divided by the scale and
// rounded, and the buffer is the same size or one pixel larger, which the
// compositor crops instead of resampling. Anything else (an unknown or stale
// mode, a rounding gap, a buffer smaller than the mode) is not trusted.
func modeFits(mode, logical, buffer int, scale float64) bool {
	return mode > 0 && int(math.Round(float64(mode)/scale)) == logical && buffer-mode >= 0 && buffer-mode <= 1
}

// setWorkspaces keeps the workspace that is currently displayed on this
// output, and only that one. The picker acts instantly on a click or Enter, so
// offering a hidden workspace would highlight and capture something the user
// cannot see. Workspaces without an ID, on other outputs or not Active are
// never offered; with none, W does nothing.
func (m *model) setWorkspaces(ws []ports.Workspace) {
	m.workspace = ports.Workspace{}
	id := m.outputs[m.index].ID
	for _, w := range ws {
		if w.OutputID == id && w.ID != 0 && w.Active {
			m.workspace = w
			return
		}
	}
}

// fail ends this overlay with a failure that names its output.
func (m *model) fail(err error) {
	if m.end == endNone {
		m.end = endCanceled
		m.sess.fail(fmt.Errorf("output %s: %w", m.outputs[m.index].Name, err))
	}
}

// visible is everything the scene draws that input can change. Comparing two
// snapshots tells the framework whether a redraw is needed; it is a plain value
// and never allocates.
type visible struct {
	rect     ports.Region
	dragging bool
	kind     core.PickKind
	grid     bool
	status   core.PickStatus
}

func (m *model) snapshot() visible {
	r, _ := m.picker.Rect()
	return visible{r, m.picker.Dragging(), m.picker.Kind(), m.grid, m.picker.Status()}
}

// input is the OnInput callback. It reports whether the drawn state changed,
// so pointer motion that moves nothing visible (hover, a clamped edge, axis
// events, other buttons, key releases and repeats) costs no redraw. Accepting
// or canceling reports true: the overlay is closing. It allocates only when the
// dimension label text changes.
func (m *model) input(ev nefergui.InputEvent) bool {
	if m.end != endNone || !m.sized {
		return false
	}
	before, endBefore := m.snapshot(), m.end
	switch ev.Kind {
	case nefergui.InputPointerMotion:
		m.picker.MouseMove(ev.X, ev.Y)
	case nefergui.InputPointerPress:
		if ev.Button == btnLeft {
			m.picker.MouseDown(ev.X, ev.Y)
		}
	case nefergui.InputPointerRelease:
		if ev.Button == btnLeft {
			m.picker.MouseUp(ev.X, ev.Y)
		}
	case nefergui.InputReset:
		// Input was dropped: a release may never arrive. Abandon any press or
		// drag, keeping the chosen kind, grid and workspace.
		m.picker.AbandonPress()
	case nefergui.InputKey:
		if ev.Pressed && !ev.Repeat && ev.Modifiers&nefergui.ModCtrl == 0 {
			m.key(ev.KeyName)
		}
	}
	m.settle()
	return m.end != endBefore || m.snapshot() != before
}

func (m *model) key(name string) {
	switch name {
	case "Escape":
		m.picker.Cancel()
	case "m", "M":
		m.picker.SelectMonitor()
	case "w", "W":
		m.picker.SelectWorkspace() // a no-op until a real workspace is supplied
	case "r", "R":
		m.picker.SelectRegion()
	case "g", "G":
		m.grid = !m.grid
	case "Return", "KP_Enter":
		m.picker.Confirm()
	default:
		// A digit picks that whole monitor from any overlay, without reopening.
		if i, ok := outputKey(name, len(m.outputs)); ok && !m.picker.Dragging() {
			m.end = endAccepted
			m.sess.accept(core.PickResult{OutputID: m.outputs[i].ID, Kind: core.PickMonitor})
		}
	}
}

// outputKey maps "1".."9" to an output index below n.
func outputKey(name string, n int) (int, bool) {
	if len(name) != 1 || name[0] < '1' || name[0] > '9' {
		return 0, false
	}
	i := int(name[0] - '1')
	return i, i < n
}

// refreshFooter rebuilds the footer only when the target kind or workspace changes.
func (m *model) refreshFooter() {
	kind := m.picker.Kind()
	_, _, workspace := m.picker.Workspace()
	if kind != m.footerKind || workspace != m.footerWorkspace {
		m.footerKind, m.footerWorkspace = kind, workspace
		m.footerText = footerText(m.mode, kind, workspace)
	}
}

// settle mirrors the picker's terminal state and refreshes the label.
func (m *model) settle() {
	m.refreshFooter()
	switch m.picker.Status() {
	case core.PickAccepted:
		r, _ := m.picker.Result()
		m.end = endAccepted
		m.sess.accept(r)
		return
	case core.PickCanceled:
		m.end = endCanceled
		m.sess.abort()
		return
	}
	if r, ok := m.picker.Rect(); ok {
		m.label.set(r.Width, r.Height)
	} else if m.picker.Kind() == core.PickMonitor {
		m.label.set(m.w, m.h)
	} else if _, r, ok := m.picker.Workspace(); ok && m.picker.Kind() == core.PickWorkspace {
		m.label.set(r.Width, r.Height)
	} else {
		m.label.clear()
	}
}

// label caches the "W × H" text so it is rebuilt only when the size changes.
type label struct {
	w, h int
	text string
	buf  []byte
}

func (l *label) set(w, h int) {
	if l.text != "" && l.w == w && l.h == h {
		return
	}
	l.w, l.h = w, h
	b := strconv.AppendInt(l.buf[:0], int64(w), 10)
	b = append(b, " × "...)
	b = strconv.AppendInt(b, int64(h), 10)
	l.buf = b
	l.text = string(b)
}

func (l *label) clear() { l.text = "" }

// headerText names the output and, with several outputs, the digit keys that
// pick a whole monitor.
func headerText(outputs []ports.Output, index int) string {
	b := append([]byte(nil), outputs[index].Name...)
	if n := len(outputs); n > 1 {
		b = append(b, " · "...)
		b = strconv.AppendInt(b, int64(index+1), 10)
		b = append(b, '/')
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, " · keys 1-"...)
		b = strconv.AppendInt(b, int64(n), 10)
		b = append(b, " pick monitor"...)
	}
	return string(b)
}
