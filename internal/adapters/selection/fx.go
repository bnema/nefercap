package selection

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

// Short effects confirm a key press: a toast names the new mode or target and
// the selection border pulses. They only exist while the selector is open, so
// they never reach a capture.
const (
	fxStep     = 30 * time.Millisecond
	toastFade  = 9 * fxStep // o1..o9 after the hold
	modeHold   = 600 * time.Millisecond
	targetHold = 400 * time.Millisecond
	warnHold   = 1500 * time.Millisecond
	pulseSpan  = 210 * time.Millisecond
	pulseExtra = 3.0 // logical pixels added to the border at the pulse start
)

// opacityClasses are the toast steps of style.css, from opaque to faint.
var opacityClasses = [...]string{"o0", "o1", "o2", "o3", "o4", "o5", "o6", "o7", "o8", "o9"}

// Toast texts are constants so triggering one never allocates.
const (
	toastShot      = "Screenshot mode"
	toastRec       = "Record mode"
	toastRegion    = "Region · drag to select"
	toastMonitor   = "Monitor"
	toastWorkspace = "Workspace"
	toastNoWS      = "No workspace found · using monitor"
)

// toast is one centered message. start is zero until the first frame draws
// it, so a slow frame does not eat the hold.
type toast struct {
	text  string
	warn  bool
	hold  time.Duration
	start time.Time
	step  int // index in opacityClasses; valid while on
	on    bool
}

// pulse thickens the selection border and lets it shrink back.
type pulse struct {
	start time.Time
	on    bool
}

// animator wakes its overlay every fxStep while an effect runs and sleeps
// otherwise, so an idle selector does not redraw. until is written by the
// overlay's owner loop and read by the animator goroutine.
type animator struct {
	kick  chan struct{}
	until atomic.Int64 // unix nanoseconds
}

func newAnimator() *animator { return &animator{kick: make(chan struct{}, 1)} }

// extend keeps the overlay redrawing until t. A nil animator does nothing.
func (a *animator) extend(t time.Time) {
	if a == nil {
		return
	}
	if n := t.UnixNano(); n > a.until.Load() {
		a.until.Store(n)
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// run sends a non-blocking wake every fxStep until the effects end, then waits
// for the next kick. It returns when ctx ends.
func (a *animator) run(ctx context.Context, wake chan<- struct{}) {
	t := time.NewTicker(fxStep)
	t.Stop()
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.kick:
		}
		t.Reset(fxStep)
		// One step past the end draws the frame that removes the effect.
		for time.Now().UnixNano() < a.until.Load()+int64(fxStep) {
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
		t.Stop()
	}
}

// showToast replaces the current toast. It never allocates.
func (m *model) showToast(text string, hold time.Duration, warn bool) {
	m.toast = toast{text: text, warn: warn, hold: hold, on: true}
	m.fx++
}

// pulseBorder restarts the border pulse.
func (m *model) pulseBorder() {
	m.pulse = pulse{on: true}
	m.fx++
}

// targetToast is the toast that confirms a target key.
func targetToast(kind core.PickKind) string {
	switch kind {
	case core.PickMonitor:
		return toastMonitor
	case core.PickWorkspace:
		return toastWorkspace
	}
	return toastRegion
}

func modeToast(mode ports.Mode) string {
	if mode == ports.Record {
		return toastRec
	}
	return toastShot
}

// animate advances the effects to now. The view calls it before every scene;
// it never allocates.
func (m *model) animate(now time.Time) {
	m.edgeW = edge
	if t := &m.toast; t.on {
		if t.start.IsZero() {
			t.start = now
			m.anim.extend(now.Add(t.hold + toastFade))
		}
		el := now.Sub(t.start)
		switch {
		case el >= t.hold+toastFade:
			t.on = false
		case el <= t.hold:
			t.step = 0
		default:
			t.step = min(1+int((el-t.hold)/fxStep), len(opacityClasses)-1)
		}
	}
	if p := &m.pulse; p.on {
		if p.start.IsZero() {
			p.start = now
			m.anim.extend(now.Add(pulseSpan))
		}
		el := now.Sub(p.start)
		if el >= pulseSpan {
			p.on = false
		} else {
			m.edgeW = edge + math.Round(pulseExtra*float64(pulseSpan-el)/float64(pulseSpan))
		}
	}
}
