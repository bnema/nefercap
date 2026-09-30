// Package core implements the capture workflow over ports only.
package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bnema/nefercap/internal/ports"
)

// ErrInvalidSelection reports a selection rejected before any port is used.
var ErrInvalidSelection = errors.New("invalid capture selection")

// Service runs one capture selection. It does not own the Source lifetime:
// the caller closes the Source after Run returns.
type Service struct {
	source ports.Source
	png    ports.ScreenshotWriter
	video  ports.VideoWriter
}

// New wires the capture workflow to its ports.
func New(source ports.Source, png ports.ScreenshotWriter, video ports.VideoWriter) *Service {
	return &Service{source: source, png: png, video: video}
}

// Run validates selection and then takes one screenshot or records silent
// fixed-rate video. Recording ends at the selection duration or when ctx is
// cancelled; both finalize the file through VideoWriter.Close. Captures and
// writes are sequential, so at most one is in flight.
func (s *Service) Run(ctx context.Context, selection ports.Selection) error {
	if err := validateSelection(selection); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if selection.Mode == ports.Screenshot {
		return s.screenshot(ctx, selection)
	}
	return s.record(ctx, selection)
}

func (s *Service) screenshot(ctx context.Context, sel ports.Selection) error {
	frame, err := s.source.Capture(ctx, sel.Target)
	if err != nil {
		return fmt.Errorf("capture screenshot: %w", err)
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("captured frame: %w", err)
	}
	if err := s.png.Save(ctx, frame, sel.Path); err != nil {
		return fmt.Errorf("save screenshot: %w", err)
	}
	return nil
}

func (s *Service) record(ctx context.Context, sel ports.Selection) error {
	frame, err := s.source.Capture(ctx, sel.Target)
	if err != nil {
		return fmt.Errorf("capture first frame: %w", err)
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("captured frame: %w", err)
	}
	if err := s.video.Start(ctx, frame, sel.Path, sel.Video); err != nil {
		return s.abort(fmt.Errorf("start video: %w", err))
	}
	if err := s.video.Write(ctx, frame); err != nil {
		// Nothing was recorded, even when the cause is cancellation: discard
		// the file instead of finalizing an empty recording.
		return s.abort(fmt.Errorf("write first frame: %w", err))
	}

	loopErr := s.recordLoop(ctx, frame, sel)
	// Graceful finalize on every exit after the first frame; the writer bounds it.
	if closeErr := s.video.Close(); closeErr != nil {
		return errors.Join(loopErr, fmt.Errorf("finalize video: %w", closeErr))
	}
	return loopErr
}

func (s *Service) abort(err error) error {
	if abortErr := s.video.Abort(); abortErr != nil {
		return errors.Join(err, fmt.Errorf("abort video: %w", abortErr))
	}
	return err
}

// stopWriteTimeout bounds the trailing duplicate writes made after a stop
// request, which must not be interrupted by the cancellation itself.
const stopWriteTimeout = 2 * time.Second

// schedule maps elapsed time since the first frame to fixed-rate frame slots.
// Slot n is due at n/fps. total is the slot count of a timed recording, or 0
// when recording until cancellation.
type schedule struct {
	start time.Time
	fps   int64
	total int64
}

func newSchedule(start time.Time, sel ports.Selection) schedule {
	sc := schedule{start: start, fps: int64(sel.Video.FPS)}
	if sel.Duration > 0 {
		// ceil(duration*fps) split into whole seconds and remainder so the
		// arithmetic cannot overflow for any positive time.Duration.
		secs, rem := int64(sel.Duration/time.Second), int64(sel.Duration%time.Second)
		sc.total = secs*sc.fps + (rem*sc.fps+int64(time.Second)-1)/int64(time.Second)
	}
	return sc
}

// due returns the offset at which slot n starts.
func (sc schedule) due(n int64) time.Duration {
	q, r := n/sc.fps, n%sc.fps
	return time.Duration(q)*time.Second + time.Duration(r)*time.Second/time.Duration(sc.fps)
}

// current returns the slot containing now, limited to the last slot of a
// timed recording. past reports that a timed recording's window has ended.
func (sc schedule) current() (slot int64, past bool) {
	elapsed := time.Since(sc.start)
	secs, rem := int64(elapsed/time.Second), int64(elapsed%time.Second)
	slot = secs*sc.fps + rem*sc.fps/int64(time.Second)
	if sc.total > 0 && slot >= sc.total {
		return sc.total - 1, true
	}
	return slot, false
}

// recordLoop fills slots 1.. after the already written first frame. A slot
// missed because Capture or Write ran long reuses the current frame, which is
// still valid until the next Capture, so the encoded duration stays exact
// even when wall time overruns. A timed recording ends after ceil(duration*fps)
// frames; otherwise it ends when ctx is done.
func (s *Service) recordLoop(ctx context.Context, frame ports.Frame, sel ports.Selection) error {
	sc := newSchedule(time.Now(), sel)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	timer.Stop()

	next := int64(1)
	for sc.total == 0 || next < sc.total {
		if wait := sc.due(next) - time.Since(sc.start); wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				return s.fillElapsed(ctx, frame, next, sc)
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			return s.fillElapsed(ctx, frame, next, sc)
		}

		cur, past := sc.current()
		cur = max(cur, next)
		// Slots already missed repeat the frame before it is invalidated.
		last := cur - 1
		if past {
			last = cur // window over: no capture for the final slot
		}
		for ; next <= last; next++ {
			if err := s.video.Write(ctx, frame); err != nil {
				return writeResult(ctx, err)
			}
		}
		if past {
			return nil
		}

		var err error
		if frame, err = s.source.Capture(ctx, sel.Target); err != nil {
			if stopped(ctx, err) {
				return nil
			}
			return fmt.Errorf("capture frame: %w", err)
		}
		if err := frame.Validate(); err != nil {
			return fmt.Errorf("captured frame: %w", err)
		}
		if err := s.video.Write(ctx, frame); err != nil {
			return writeResult(ctx, err)
		}
		next++
	}
	return nil
}

// fillElapsed repeats the current frame for slots that began before the stop
// request so the file covers the time actually recorded.
func (s *Service) fillElapsed(ctx context.Context, frame ports.Frame, next int64, sc schedule) error {
	cur, _ := sc.current()
	if next > cur {
		return nil
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopWriteTimeout)
	defer cancel()
	for ; next <= cur; next++ {
		if err := s.video.Write(wctx, frame); err != nil {
			return fmt.Errorf("write trailing frame: %w", err)
		}
	}
	return nil
}

func writeResult(ctx context.Context, err error) error {
	if stopped(ctx, err) {
		return nil
	}
	return fmt.Errorf("write frame: %w", err)
}

// stopped reports whether the run is ending and err consists only of context
// cancellation or deadline leaves. A failure joined with cancellation is real.
func stopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil && onlyContext(err)
}

func onlyContext(err error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		for _, c := range children {
			if !onlyContext(c) {
				return false
			}
		}
		return len(children) > 0
	case interface{ Unwrap() error }:
		return err == context.Canceled || err == context.DeadlineExceeded || onlyContext(e.Unwrap())
	default:
		return err == context.Canceled || err == context.DeadlineExceeded
	}
}

func validateSelection(sel ports.Selection) error {
	switch sel.Mode {
	case ports.Screenshot, ports.Record:
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidSelection, sel.Mode)
	}
	if sel.Target.OutputID == 0 {
		return fmt.Errorf("%w: output id is zero", ErrInvalidSelection)
	}
	if err := validateRegion(sel.Target.Region); err != nil {
		return err
	}
	if sel.Path == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidSelection)
	}
	if sel.Duration < 0 {
		return fmt.Errorf("%w: negative duration %s", ErrInvalidSelection, sel.Duration)
	}
	if sel.Mode == ports.Record {
		return validateVideo(sel.Video)
	}
	return nil
}

// validateRegion accepts the zero region (full output) or a region with
// positive size and nonnegative origin that fits in int32.
func validateRegion(r ports.Region) error {
	if r == (ports.Region{}) {
		return nil
	}
	x, y, w, h := int64(r.X), int64(r.Y), int64(r.Width), int64(r.Height)
	if x < 0 || y < 0 || w < 1 || h < 1 ||
		x > math.MaxInt32 || y > math.MaxInt32 || w > math.MaxInt32 || h > math.MaxInt32 ||
		x+w > math.MaxInt32 || y+h > math.MaxInt32 {
		return fmt.Errorf("%w: %w: %+v", ErrInvalidSelection, ports.ErrInvalidRegion, r)
	}
	return nil
}

func validateVideo(v ports.VideoSettings) error {
	if v.FPS < 1 || v.FPS > ports.MaxFPS {
		return fmt.Errorf("%w: fps %d outside 1..%d", ErrInvalidSelection, v.FPS, ports.MaxFPS)
	}
	if v.Width == 0 && v.Height == 0 {
		return nil
	}
	w, h := v.Width, v.Height
	if w < 1 || h < 1 || w > ports.MaxDimension || h > ports.MaxDimension || w%2 != 0 || h%2 != 0 ||
		w*ports.BytesPerPixel > ports.MaxFrameBytes/h {
		return fmt.Errorf("%w: invalid output size %dx%d", ErrInvalidSelection, w, h)
	}
	return nil
}
