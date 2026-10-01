// Package indicator is the recording HUD: a small layer surface at the top
// centre of the recorded output (of the workspace frame, when it is smaller)
// showing a REC timer and a Stop button.
//
// It is one thin surface, not a full-output overlay: it takes no keyboard
// focus, and only the Stop button receives pointer input; the rest of the HUD
// and everything around it pass clicks through. The border around the
// recorded area is not drawn here: the compositor marks it.
//
// Capture exclusion is explicit authorization, not a property of how the HUD is
// built. The caller supplies authorize, which NeferGUI calls with the surface
// once it is configured and before its first buffer, to register whatever
// exclusion the compositor contract requires on that same client; Run refuses
// to start without it. The layer namespace is only a role hint and authorizes
// nothing. If authorize fails, Run fails and nothing is shown.
package indicator

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/adapters/uierrors"
	"github.com/bnema/nefercap/internal/adapters/uistyle"
	"github.com/bnema/nefercap/internal/ports"
)

//go:embed style.css
var styleSheet string

// HUD geometry in logical pixels. The Stop button is the only input region.
const (
	baseFont      = 16
	Width, Height = baseFont * 24, baseFont * 5 / 2
	stopWidth     = baseFont * 45 / 8 // 5.625rem in CSS
	topMargin     = baseFont / 2
	namespace     = "nefercap-indicator"
	tick          = time.Second
)

var errOverlayClosed = errors.New("overlay closed by the compositor")

// Setup errors, returned before anything is shown.
var (
	// ErrNoAuthorize means a HUD was built without an exclusion hook.
	ErrNoAuthorize = errors.New("indicator: capture exclusion hook is required")
	// ErrNoStop means a HUD was built without a stop channel.
	ErrNoStop = errors.New("indicator: stop channel is required")
)

// Indicator shows the recording HUD.
type Indicator struct {
	// TargetLabel optionally names what is recorded (a workspace or output
	// name). It is shown after the timer, cut to 12 characters, and only as
	// text: the HUD never draws geometry for it. Set it before Run.
	TargetLabel string
	// Frame optionally is the part of the output that is recorded, in
	// output-local logical pixels (a workspace smaller than its output): the
	// HUD sits at the top centre of it. The zero value is the whole output.
	// Set it before Run.
	Frame ports.Region

	authorize func(context.Context, nefergui.WaylandSurface) error
	stop      chan<- struct{}
}

// New returns an indicator. authorize is required and runs once per Run, on
// the window's owner loop; its context ends when Run's does, so a blocking wait
// for the compositor's acknowledgement is interrupted by cancellation. stop
// should be buffered; a click sends one non-blocking value. The caller reacts
// by ending the recording.
func New(authorize func(context.Context, nefergui.WaylandSurface) error, stop chan<- struct{}) *Indicator {
	return &Indicator{authorize: authorize, stop: stop}
}

// Run shows the HUD on output until ctx ends or the window system fails. It
// returns nil when ctx was canceled by the caller, and an error otherwise; the
// error names the output and its size. The elapsed time starts when Run is
// called, before the window exists. Run blocks; call it from its own goroutine
// and cancel ctx to remove the HUD.
func (i *Indicator) Run(ctx context.Context, output ports.Output) error {
	switch {
	case i == nil || i.authorize == nil:
		return ErrNoAuthorize
	case i.stop == nil:
		return ErrNoStop
	case output.Name == "":
		return fmt.Errorf("indicator: output %d has no name: %w", output.ID, ports.ErrOutputNotFound)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := i.run(ctx, output, time.Now()); err != nil {
		return fmt.Errorf("indicator: output %s (%dx%d): %w", output.Name, output.Width, output.Height, err)
	}
	return nil
}

func (i *Indicator) run(ctx context.Context, output ports.Output, start time.Time) error {
	sheet, remove, err := uistyle.Write(styleSheet, "nefercap-indicator-*.css")
	if err != nil {
		return err
	}
	defer remove()

	child, cancel := context.WithCancel(ctx)
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); ticker(child, wake) }()
	defer func() { cancel(); wg.Wait() }()

	runErr := nefergui.Run(child, newHUD(start, i.stop, i.TargetLabel), view,
		nefergui.Title("NeferCap recording"),
		nefergui.Size(Width, Height),
		nefergui.Styles(sheet),
		nefergui.Transparent(),
		nefergui.Layer(layerConfig(output.Name, i.Frame)),
		nefergui.OnSurface(i.authorize),
		nefergui.Wake(wake),
	)
	// Run is only ever ended by ctx: the HUD never decides by itself. Read the
	// parent once; a bare Canceled is then our own cancellation, and anything
	// else, including an end with a live parent, is a failure that keeps every
	// real error.
	perr := ctx.Err()
	return uierrors.Ended(runErr, perr != nil, perr, errOverlayClosed)
}

// layerConfig is a top-centre HUD that never takes keyboard focus and accepts
// pointer input only inside the Stop button. Centre is that of the output, or
// of frame when it is set: then the HUD is anchored to the top left of the
// output and moved by margins.
func layerConfig(output string, frame ports.Region) nefergui.LayerConfig {
	c := nefergui.LayerConfig{
		Output:        output,
		Namespace:     namespace,
		Level:         nefergui.LayerOverlay,
		Anchors:       nefergui.AnchorTop,
		Keyboard:      nefergui.KeyboardNone,
		ExclusiveZone: -1,
		Margin:        [4]int32{topMargin, 0, 0, 0},
		InputRects:    []nefergui.Rect{stopRect()},
	}
	if frame.Width > 0 && frame.Height > 0 {
		c.Anchors = nefergui.AnchorTop | nefergui.AnchorLeft
		c.Margin = [4]int32{int32(frame.Y) + topMargin, 0, 0, int32(frame.X + max((frame.Width-Width)/2, 0))}
	}
	return c
}

func stopRect() nefergui.Rect {
	return nefergui.Rect{X: Width - stopWidth, Y: 0, Width: stopWidth, Height: Height}
}

// ticker sends a non-blocking wake token every second until ctx ends.
func ticker(ctx context.Context, wake chan<- struct{}) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
}

func view(f *nefergui.Frame, m *hud) {
	root := f.Root(nefergui.Class("hud"))
	row := root.Row(nefergui.Class("bar"))
	row.Text(m.refresh(time.Now()), nefergui.Key("timer"), nefergui.Class("timer"))
	if row.Button("Stop", nefergui.Key("stop"), nefergui.Class("stop")).Activated() {
		m.requestStop()
	}
}
