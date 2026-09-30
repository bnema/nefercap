package selection

import (
	"fmt"
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

	end  endKind
	sess *session // shared first-decision record; the only cross-overlay state

	workspace  ports.Workspace // zero ID: none
	header     string          // static per surface
	footerText string
	label      label
}

func newModel(mode ports.Mode, outputs []ports.Output, index int, sess *session) *model {
	return &model{mode: mode, outputs: outputs, index: index, sess: sess, header: headerText(outputs, index), footerText: footerText(mode)}
}

// resize (re)starts the picker when the logical output size changes. It is
// the OnResize callback; scale-only changes keep the current pick.
func (m *model) resize(w, h int, _ float64) {
	if m.sized && w == m.w && h == m.h {
		return
	}
	if err := m.picker.Begin(m.outputs[m.index].ID, w, h); err != nil {
		m.fail(fmt.Errorf("resize to %dx%d: %w", w, h, err))
		return
	}
	m.sized, m.w, m.h = true, w, h
	m.label.clear()
	m.picker.SetWorkspace(m.workspace.ID, m.workspace.Region)
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
	return visible{r, m.picker.Dragging(), m.picker.Kind(), m.picker.Grid(), m.picker.Status()}
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
		m.picker.ToggleGrid()
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

// settle mirrors the picker's terminal state and refreshes the label.
func (m *model) settle() {
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
