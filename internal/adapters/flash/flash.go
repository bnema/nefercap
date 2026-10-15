// Package flash confirms a saved screenshot: a green frame covers the captured
// area and fades out in about 300 ms. It is drawn only after the capture, so it
// never appears in the image. The surface takes no keyboard focus and lets
// every click through.
package flash

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"

	"github.com/bnema/nefercap/internal/adapters/layerui"
	"github.com/bnema/nefercap/internal/adapters/uierrors"
	"github.com/bnema/nefercap/internal/adapters/uistyle"
	"github.com/bnema/nefercap/internal/ports"
)

//go:embed style.css
var styleSheet string

const (
	namespace = "nefercap-flash"
	step      = 30 * time.Millisecond
	// limit bounds the whole effect, setup included, so a slow compositor or
	// GPU never keeps the process alive.
	limit = 2 * time.Second
)

// stepClasses are the opacity steps of style.css, from opaque to faint.
var stepClasses = [...]string{"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9"}

var errClosed = errors.New("flash closed by the compositor")

// Show flashes area on output and returns when the fade ends. The zero area is
// the whole output. It returns nil when the fade completed or ctx ended.
func Show(ctx context.Context, output ports.Output, area ports.Region) error {
	if output.Name == "" {
		return fmt.Errorf("flash: output %d has no name: %w", output.ID, ports.ErrOutputNotFound)
	}
	if err := show(ctx, output, area); err != nil {
		return fmt.Errorf("flash: output %s: %w", output.Name, err)
	}
	return nil
}

func show(ctx context.Context, output ports.Output, area ports.Region) error {
	sheet, remove, err := uistyle.Write(styleSheet, "nefercap-flash-*.css")
	if err != nil {
		return err
	}
	defer remove()

	child, cancel := context.WithTimeout(ctx, limit)
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Go(func() { ticker(child, wake) })
	defer func() { cancel(); wg.Wait() }()

	m := &fade{done: cancel}
	runErr := layerui.Run(child, layerui.Config{
		Layer:  layerConfig(output.Name, area),
		Styles: sheet,
		Wake:   wake,
	}, m, view)
	// The fade, the time limit or the parent ended the run on purpose; only
	// an end with neither is the compositor closing the surface.
	explained := m.ended || child.Err() != nil
	return uierrors.Ended(runErr, explained, ctx.Err(), errClosed)
}

// layerConfig covers area, or the whole output for the zero area, above every
// window, without focus or pointer input.
func layerConfig(output string, area ports.Region) neferclient.LayerConfig {
	c := neferclient.LayerConfig{
		Output:        output,
		Namespace:     namespace,
		Level:         neferclient.LayerOverlay,
		Anchors:       neferclient.AnchorTop | neferclient.AnchorBottom | neferclient.AnchorLeft | neferclient.AnchorRight,
		Keyboard:      neferclient.KeyboardNone,
		ExclusiveZone: -1,
		InputRects:    []neferclient.Rect{},
	}
	if area.Width > 0 && area.Height > 0 {
		c.Anchors = neferclient.AnchorTop | neferclient.AnchorLeft
		c.Margin = [4]int32{int32(area.Y), 0, 0, int32(area.X)}
		c.Width, c.Height = int32(area.Width), int32(area.Height)
	}
	return c
}

// ticker sends a non-blocking wake token every step until ctx ends.
func ticker(ctx context.Context, wake chan<- struct{}) {
	t := time.NewTicker(step)
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

// fade is the view model. It is used only on the layerui owner goroutine.
type fade struct {
	start time.Time // first frame; zero until then
	ended bool
	done  func()
}

// class returns the opacity class for now. The fade starts with the first
// frame, so a slow setup does not eat into it. Past the last step it ends the
// run and keeps the faintest class.
func (f *fade) class(now time.Time) string {
	if f.start.IsZero() {
		f.start = now
	}
	i := int(now.Sub(f.start) / step)
	if i >= len(stepClasses) {
		if !f.ended {
			f.ended = true
			f.done()
		}
		i = len(stepClasses) - 1
	}
	return stepClasses[max(i, 0)]
}

func view(f *nefergui.Frame, m *fade) {
	f.Root().Box(nefergui.Key("flash"), nefergui.Class("flash"), nefergui.Class(m.class(time.Now())))
}
