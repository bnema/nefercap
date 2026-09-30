// Package selection is the NeferGUI layer-shell capture selector.
//
// Select opens one transparent overlay layer surface on every output. Each
// dims everything except the dragged region, draws thin accent lines and a
// size label, and the first decision from any of them is returned. It needs no
// screenshot: the live compositor shows through the transparent surface.
//
// One overlay per output: every monitor is selectable at once. A Wayland
// client cannot hold exclusive keyboard focus on several surfaces, so the first
// output's overlay is keyboard-exclusive and the others take the keyboard on
// demand (on click). Each overlay is a separate NeferGUI window with its own
// model on its own owner loop; they share only a first-decision-wins session.
package selection

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/adapters/uistyle"
	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

//go:embed style.css
var styleSheet string

// DefaultFPS is the recording frame rate assigned to record selections.
const DefaultFPS = 30

// namespace is an opaque role hint for the compositor, not an authorization.
const namespace = "nefercap-selector"

// MaxOutputs is the most outputs one selection covers: one per digit key.
const MaxOutputs = 9

// ErrTooManyOutputs reports more outputs than MaxOutputs. Nothing is opened.
var ErrTooManyOutputs = errors.New("selection: too many outputs")

var errNoOutputs = fmt.Errorf("selection: no outputs to select: %w", ports.ErrOutputNotFound)

// Selector shows the layer-shell selection overlay.
type Selector struct {
	mode       ports.Mode
	video      ports.VideoSettings
	workspaces []ports.Workspace
	grid       bool
}

var _ ports.Selector = (*Selector)(nil)

// New returns a selector for mode. Record selections carry video; a zero FPS
// becomes DefaultFPS. workspaces is native compositor metadata: the W key is
// offered only for the Active workspace with a valid (nonzero) ID on the output
// an overlay covers. Hidden workspaces are never offered and nothing is
// invented when no workspace qualifies. grid is the initial state of the
// whole-monitor guides; the G key toggles them per overlay. The caller assigns
// Selection.Path. The first output passed to Select opens first, so the caller
// orders outputs to put the preferred one first.
func New(mode ports.Mode, video ports.VideoSettings, workspaces []ports.Workspace, grid bool) *Selector {
	if mode == ports.Record && video.FPS == 0 {
		video.FPS = DefaultFPS
	}
	if mode != ports.Record {
		video = ports.VideoSettings{}
	}
	return &Selector{mode: mode, video: video, workspaces: append([]ports.Workspace(nil), workspaces...), grid: grid}
}

// Select blocks until the user accepts a selection (true), aborts with Escape
// (false, nil) or ctx ends or the window system fails (error). One overlay
// opens on every output at once, so the whole desktop is selectable without
// reopening. The first decision from any overlay wins and ends all of them:
// Select returns only after every overlay's Wayland session has closed, so an
// accepted selection never races a window that is still mapped. The compositor
// may still be processing that teardown, so a caller must confirm through a
// fresh capture connection before capturing. Select creates no capture path or
// directory and fails with ErrTooManyOutputs rather than dropping outputs. One
// goroutine at a time.
//
// Keyboard: outputs[0] takes exclusive keyboard focus; the other overlays take
// it on demand, which compositors grant when the pointer clicks them. Digits
// 1..9 pick that whole monitor from whichever overlay has the keyboard.
func (s *Selector) Select(ctx context.Context, outputs []ports.Output) (ports.Selection, bool, error) {
	if err := ctx.Err(); err != nil {
		return ports.Selection{}, false, err
	}
	if len(outputs) == 0 {
		return ports.Selection{}, false, errNoOutputs
	}
	if len(outputs) > MaxOutputs {
		return ports.Selection{}, false, fmt.Errorf("%w: %d outputs, at most %d", ErrTooManyOutputs, len(outputs), MaxOutputs)
	}
	sheet, remove, err := uistyle.Write(styleSheet, "nefercap-selector-*.css")
	if err != nil {
		return ports.Selection{}, false, fmt.Errorf("selection: %w", err)
	}
	defer remove()

	child, cancel := context.WithCancel(ctx)
	defer cancel()
	sess := newSession(cancel)
	var wg sync.WaitGroup
	for i := range outputs {
		m := newModel(s.mode, outputs, i, sess, s.grid)
		m.setWorkspaces(s.workspaces)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.runEnded(ctx, outputs[i].Name, run(child, sheet, m))
		}()
	}
	wg.Wait()
	result, ok, err := sess.outcome(ctx)
	if err != nil || !ok {
		return ports.Selection{}, false, err
	}
	return s.selection(result), true, nil
}

// run shows one overlay on its own output until the context ends.
func run(ctx context.Context, sheet string, m *model) error {
	kb := nefergui.KeyboardOnDemand
	if m.index == 0 {
		kb = nefergui.KeyboardExclusive
	}
	return nefergui.Run(ctx, m, view,
		nefergui.Title("NeferCap selector"),
		nefergui.Size(1, 1),
		nefergui.Styles(sheet),
		nefergui.Transparent(),
		nefergui.Layer(layerConfig(m.outputs[m.index].Name, kb)),
		nefergui.OnResize(m.resize),
		nefergui.OnInput(m.input),
	)
}

// layerConfig is one overlay: every edge, above windows, the given keyboard
// mode, and an exclusive zone of -1 so no bar shifts it.
func layerConfig(output string, keyboard nefergui.KeyboardMode) nefergui.LayerConfig {
	return nefergui.LayerConfig{
		Output:        output,
		Namespace:     namespace,
		Level:         nefergui.LayerOverlay,
		Anchors:       nefergui.AnchorTop | nefergui.AnchorBottom | nefergui.AnchorLeft | nefergui.AnchorRight,
		Keyboard:      keyboard,
		ExclusiveZone: -1,
	}
}

// selection converts an accepted pick to a port selection through the core
// rules: a monitor and a workspace carry a zero Region (a workspace's geometry
// comes live from the compositor, identified by WorkspaceID), a dragged region
// keeps its explicit output-local geometry.
func (s *Selector) selection(r core.PickResult) ports.Selection {
	return ports.Selection{Mode: s.mode, Target: r.Target(), Video: s.video}
}
