package gui

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/ports"
)

const (
	modeScreenshot = string(ports.Screenshot)
	modeRecord     = string(ports.Record)

	statusReady  = "ready: press Capture (or Enter in a field)"
	noticeExists = "! file exists: choose another path"

	// The panel is a regular toplevel: the compositor places it, and capture
	// support is limited to what the capture adapter offers.
	warning = "note: regular window, not a layer-shell overlay; it opens on the monitor " +
		"of the current workspace. Off-screen sources and the cursor cannot be captured."
	recordNote  = "duration 0 records until Ctrl+C in the terminal."
	stampLayout = "20060102-150405"
)

// outputChoice is one precomputed radio entry.
type outputChoice struct {
	id         uint32
	key, label string
	opts       []nefergui.ButtonOption
}

// model is owned by the single Run view; nothing else touches it while the
// window is open.
type model struct {
	outputs []outputChoice
	output  string // key of the selected outputChoice
	mode    string
	// lastMode detects mode changes to swap an untouched default path.
	lastMode string

	path, region, fps, size, duration string
	defaultShot, defaultRec           string

	// notice is a submit-time problem shown until the path or mode changes.
	notice string

	sel      ports.Selection
	accepted bool
	closed   bool
	cancel   context.CancelFunc
}

// newModel precomputes output labels, keys and default names once.
func newModel(outputs []ports.Output, now time.Time) (*model, error) {
	if len(outputs) == 0 {
		return nil, errNoOutputs
	}
	stamp := now.Format(stampLayout)
	m := &model{
		outputs:     make([]outputChoice, len(outputs)),
		mode:        modeScreenshot,
		lastMode:    modeScreenshot,
		defaultShot: "nefercap-" + stamp + ".png",
		defaultRec:  "nefercap-" + stamp + ".mp4",
		fps:         "30",
		duration:    "0",
	}
	m.path = m.defaultShot
	var b strings.Builder
	for i, o := range outputs {
		b.Reset()
		if o.Name != "" {
			b.WriteString(o.Name)
		} else {
			b.WriteString("output ")
			b.WriteString(strconv.FormatUint(uint64(o.ID), 10))
		}
		if o.Width > 0 && o.Height > 0 {
			b.WriteString("  ")
			b.WriteString(strconv.Itoa(o.Width))
			b.WriteByte('x')
			b.WriteString(strconv.Itoa(o.Height))
		}
		if o.Scale > 1 {
			b.WriteString("  scale ")
			b.WriteString(strconv.Itoa(o.Scale))
		}
		key := strconv.Itoa(i)
		m.outputs[i] = outputChoice{
			id:    o.ID,
			key:   key,
			label: b.String(),
			opts:  []nefergui.ButtonOption{nefergui.Key("out-" + key)},
		}
	}
	m.output = m.outputs[0].key
	return m, nil
}

// outputIndex returns the selected output's index, or -1.
func (m *model) outputIndex() int {
	for i := range m.outputs {
		if m.outputs[i].key == m.output {
			return i
		}
	}
	return -1
}

// windowHeight sizes the initial window for the output list.
func (m *model) windowHeight() int {
	return min(600+26*len(m.outputs), 1000)
}

// status reports the live validation result. submittable is false while the
// form is invalid; ok is false whenever the message is a problem, including a
// submit-time notice that still lets the user retry.
func (m *model) status() (text string, submittable, ok bool) {
	if err := m.parse(); err != nil {
		return err.Error(), false, false
	}
	if m.notice != "" {
		return m.notice, true, false
	}
	return statusReady, true, true
}

// modeChanged swaps the default file name when the user has not edited it.
func (m *model) modeChanged() {
	if m.mode == m.lastMode {
		return
	}
	if m.path == m.defaultFor(m.lastMode) {
		m.path = m.defaultFor(m.mode)
	}
	m.lastMode = m.mode
	m.notice = ""
}

func (m *model) defaultFor(mode string) string {
	if mode == modeRecord {
		return m.defaultRec
	}
	return m.defaultShot
}

// pathChanged clears stale submit notices.
func (m *model) pathChanged() { m.notice = "" }

// submit validates the form; on success it records the selection and closes
// the window by cancelling Run's child context. Repeated calls are ignored.
func (m *model) submit() {
	if m.accepted || m.closed {
		return
	}
	if err := m.parse(); err != nil {
		return
	}
	if _, err := os.Lstat(m.sel.Path); err == nil {
		m.notice = noticeExists
		return
	}
	m.notice = ""
	m.accepted = true
	m.stop()
}

// dismiss closes the window without a selection.
func (m *model) dismiss() {
	if m.accepted || m.closed {
		return
	}
	m.closed = true
	m.stop()
}

func (m *model) stop() {
	if m.cancel != nil {
		m.cancel()
	}
}
