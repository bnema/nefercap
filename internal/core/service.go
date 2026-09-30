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
	runCtx := ctx
	if sel.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, sel.Duration)
		defer cancel()
	}

	frame, err := s.source.Capture(runCtx, sel.Target)
	if err != nil {
		return fmt.Errorf("capture first frame: %w", err)
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("captured frame: %w", err)
	}
	if err := s.video.Start(runCtx, frame, sel.Path, sel.Video); err != nil {
		// Nothing was recorded: discard instead of finalizing.
		err = fmt.Errorf("start video: %w", err)
		if abortErr := s.video.Abort(); abortErr != nil {
			err = errors.Join(err, fmt.Errorf("abort video: %w", abortErr))
		}
		return err
	}

	loopErr := s.recordLoop(runCtx, frame, sel)
	// Graceful finalize on every exit after Start; the writer bounds it.
	if closeErr := s.video.Close(); closeErr != nil {
		return errors.Join(loopErr, fmt.Errorf("finalize video: %w", closeErr))
	}
	return loopErr
}

// recordLoop writes the first frame, then one frame per tick. It returns nil
// when the run context ends (duration elapsed or cancellation).
func (s *Service) recordLoop(ctx context.Context, first ports.Frame, sel ports.Selection) error {
	if err := s.video.Write(ctx, first); err != nil {
		return writeResult(ctx, err)
	}
	ticker := time.NewTicker(time.Second / time.Duration(sel.Video.FPS))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return nil
		}
		frame, err := s.source.Capture(ctx, sel.Target)
		if err != nil {
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
	}
}

func writeResult(ctx context.Context, err error) error {
	if stopped(ctx, err) {
		return nil
	}
	return fmt.Errorf("write frame: %w", err)
}

// stopped reports whether err is only the consequence of the run ending.
func stopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
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
