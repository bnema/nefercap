// Package selection is the layer-shell capture selector, drawn with NeferGUI.
//
// Select opens one transparent overlay layer surface on every output. Each
// dims everything except the dragged region, draws thin accent lines and a
// size label, and the first decision from any of them is returned. It needs no
// screenshot: the live compositor shows through the transparent surface.
//
// One overlay per output: every monitor is selectable at once. A Wayland
// client cannot hold exclusive keyboard focus on several surfaces, so the first
// output's overlay is keyboard-exclusive and the others take the keyboard on
// demand (on click). Each overlay is a separate layer surface on its own
// Wayland connection, with its own model on its own owner loop; they share only
// a first-decision-wins session.
package selection

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"sync"

	"github.com/bnema/neferclient"

	"github.com/bnema/nefercap/internal/adapters/layerui"
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
	mode        ports.Mode
	video       ports.VideoSettings
	workspaces  []ports.Workspace
	grid        bool
	toggle      bool
	noIndicator bool
	backdrops   map[uint32]image.Image // frozen screens by output ID
	frozen      bool                   // the accepted selection showed the frozen screen
}

var _ ports.Selector = (*Selector)(nil)

// New returns a selector for mode. Record selections carry video; a zero FPS
// becomes DefaultFPS. workspaces is ext-workspace metadata: the W key is
// offered only for the Active workspace with a valid (nonzero) ID on the output
// an overlay covers. Other workspaces are never offered and nothing is
// invented when no workspace qualifies; a NeferWL recording of that workspace
// keeps following it when the user switches away. grid is the initial state of the
// whole-monitor guides; the G key toggles them per overlay. The caller assigns
// Selection.Path. The first output passed to Select opens first, so the caller
// orders outputs to put the preferred one first.
func New(mode ports.Mode, video ports.VideoSettings, workspaces []ports.Workspace, grid bool) *Selector {
	if video.FPS == 0 {
		video.FPS = DefaultFPS
	}
	return &Selector{mode: mode, video: video, workspaces: append([]ports.Workspace(nil), workspaces...), grid: grid}
}

// WithCapabilities adapts the selector to the compositor: W is offered only
// when it has workspaces, and record mode says so when it cannot show a
// recording indicator. Without it the selector assumes nothing is missing.
func (s *Selector) WithCapabilities(c ports.Capabilities) *Selector {
	s.noIndicator = !c.Exclusion
	if !c.Workspaces {
		s.workspaces = nil
	}
	return s
}

// These bounds mirror NeferGUI's image dimension and cache byte limits.
const (
	maxBackdrop      = 8192
	maxBackdropBytes = 64 << 20
)

// CanBackdrop reports whether an output's physical mode fits NeferGUI's image
// limits. Outputs exceeding either the side or byte limit stay live.
func CanBackdrop(o ports.Output) bool {
	return o.Width > 0 && o.Height > 0 && max(o.Width, o.Height) <= maxBackdrop &&
		o.Width <= maxBackdropBytes/ports.BytesPerPixel/o.Height
}

// WithBackdrops draws each output's frozen frame under its overlay instead of
// the live screen; outputs without a frame stay live. A frame larger than
// CanBackdrop allows is an error. The frames are copied, so the caller may
// release them once this returns.
func (s *Selector) WithBackdrops(frames map[uint32]ports.Frame) (*Selector, error) {
	s.backdrops = make(map[uint32]image.Image, len(frames))
	for id, f := range frames {
		if !CanBackdrop(ports.Output{Width: f.Width, Height: f.Height}) {
			return s, fmt.Errorf("selection: frozen frame of output %d is %dx%d, at most %d per side and %d bytes", id, f.Width, f.Height, maxBackdrop, maxBackdropBytes)
		}
		s.backdrops[id] = backdropImage(f)
	}
	return s, nil
}

// backdropImage converts a validated frame to an opaque image. NRGBA is the
// format NeferGUI uploads without another conversion.
func backdropImage(f ports.Frame) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	for y := range f.Height {
		src, dst := f.Row(y), img.Pix[y*img.Stride:]
		for x := 0; x+3 < len(src); x += 4 {
			dst[x], dst[x+1], dst[x+2], dst[x+3] = src[x+2], src[x+1], src[x], 0xff
		}
	}
	return img
}

// Frozen reports whether the last accepted selection still showed the frozen
// screen, so its screenshot must come from the frozen copy. It is false after
// F switched to the live screen, and when there were no backdrops.
func (s *Selector) Frozen() bool { return s.frozen }

// AllowToggle lets Tab switch between screenshot and record selection. The
// starting mode is New's; the target selection is kept across a switch.
func (s *Selector) AllowToggle() *Selector {
	s.toggle = true
	return s
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
	sess.canFreeze = len(s.backdrops) > 0
	s.frozen = false
	// Build every model and the complete wakes slice before any overlay
	// starts: a running overlay reads sess.wakes on Tab or F.
	models := make([]*model, len(outputs))
	for i := range outputs {
		m := newModel(s.mode, outputs, i, sess, s.grid)
		m.setWorkspaces(s.workspaces)
		m.noIndicator = s.noIndicator
		m.backdrop = s.backdrops[outputs[i].ID]
		// Every overlay has a wake: Tab on another overlay and the key
		// effects both redraw through it.
		m.wake, m.anim = make(chan struct{}, 1), newAnimator()
		if s.toggle {
			m.toggle = true
		}
		if s.toggle || sess.canFreeze {
			sess.wakes = append(sess.wakes, m.wake)
		}
		m.footerText = footerText(m.mode, m.footerKind, m.footerWorkspace, m.toggle, m.noIndicator, m.freezeHint())
		models[i] = m
	}
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fx, stop := context.WithCancel(child)
			var anim sync.WaitGroup
			anim.Go(func() { m.anim.run(fx, m.wake) })
			err := run(child, sheet, m)
			stop()
			anim.Wait()
			sess.runEnded(ctx, outputs[i].Name, err)
		}()
	}
	wg.Wait()
	s.backdrops = nil // every overlay ended: nothing draws them any more
	result, ok, err := sess.outcome(ctx)
	if err != nil || !ok {
		return ports.Selection{}, false, err
	}
	s.frozen = sess.acceptedFrozen()
	return s.selectionFor(sess.acceptedMode(), result), true, nil
}

// run shows one overlay on its own output until the context ends.
func run(ctx context.Context, sheet string, m *model) error {
	kb := neferclient.KeyboardOnDemand
	if m.index == 0 {
		kb = neferclient.KeyboardExclusive
	}
	return layerui.Run(ctx, layerui.Config{
		Layer:    layerConfig(m.outputs[m.index].Name, kb),
		Styles:   sheet,
		Wake:     m.wake,
		OnResize: m.resize,
		OnInput:  m.input,
	}, m, view)
}

// layerConfig is one overlay: every edge, above windows, the given keyboard
// mode, and an exclusive zone of -1 so no bar shifts it.
func layerConfig(output string, keyboard neferclient.KeyboardMode) neferclient.LayerConfig {
	return neferclient.LayerConfig{
		Output:        output,
		Namespace:     namespace,
		Level:         neferclient.LayerOverlay,
		Anchors:       neferclient.AnchorTop | neferclient.AnchorBottom | neferclient.AnchorLeft | neferclient.AnchorRight,
		Keyboard:      keyboard,
		ExclusiveZone: -1,
	}
}

// selection converts an accepted pick to a port selection through the core
// rules: a monitor and a workspace carry a zero Region (a workspace's geometry
// comes live from the compositor, identified by WorkspaceID), a dragged region
// keeps its explicit output-local geometry.
func (s *Selector) selection(r core.PickResult) ports.Selection {
	return s.selectionFor(s.mode, r)
}

func (s *Selector) selectionFor(mode ports.Mode, r core.PickResult) ports.Selection {
	sel := ports.Selection{Mode: mode, Target: r.Target()}
	if mode == ports.Record {
		sel.Video = s.video
	}
	return sel
}
