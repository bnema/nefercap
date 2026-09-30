// Package ports defines framework-independent capture contracts.
package ports

import (
	"context"
	"time"
)

// Output IDs are stable only within a Source connection. Name is wl_output.name.
type Output struct {
	ID            uint32
	Name          string
	Width, Height int // physical mode dimensions, informational
	Scale         int
}

// Region uses output-local logical coordinates. Zero value means the full output.
type Region struct{ X, Y, Width, Height int }

type Target struct {
	OutputID uint32
	Region   Region
}

// PixelFormat names the native little-endian wl_shm 32-bit pixel layout.
type PixelFormat uint32

const (
	ARGB8888 PixelFormat = 0
	XRGB8888 PixelFormat = 1
)

// Frame borrows storage from Source until the next Capture or Close call.
// Consumers must finish reading synchronously and must not retain Pixels.
// YInvert means the first stored row is the bottom row. Output is opaque SDR.
type Frame struct {
	Pixels                []byte
	Width, Height, Stride int
	Format                PixelFormat
	YInvert               bool
}

// Source has one sequential owner; cancellation must interrupt blocked I/O.
// A failed Capture invalidates its borrowed frame. Close is idempotent.
type Source interface {
	Outputs(context.Context) ([]Output, error)
	Capture(context.Context, Target) (Frame, error)
	Close() error
}

// ScreenshotWriter writes an opaque PNG without overwriting an existing path.
// Save completes all reads from borrowed frame storage before returning.
type ScreenshotWriter interface {
	Save(context.Context, Frame, string) error
}

// VideoSettings describes fixed-rate silent SDR video. Zero output dimensions
// retain source resolution. Both dimensions must be specified for scaling.
type VideoSettings struct {
	FPS           int
	Width, Height int
}

// VideoWriter has one owner. Start fixes input geometry and reserves a new path.
// Start does not encode its frame; pass the first frame explicitly to Write.
// Start rejects input/output geometry that its encoder cannot use; it must not
// silently crop when zero output dimensions request source resolution.
// Write consumes borrowed pixels before returning; it must not queue frames.
// Close drains and finalizes the encoder with a bounded shutdown deadline.
// Abort interrupts pending writes and is safe concurrently with Write.
// Write rejects width, height or format changes with ErrGeometryChanged.
// Stride changes are allowed. Close and Abort are idempotent terminal operations;
// Write after termination returns ErrClosed. Other methods have one owner.
type VideoWriter interface {
	Start(context.Context, Frame, string, VideoSettings) error
	Write(context.Context, Frame) error
	Close() error
	Abort() error
}

type Mode string

const (
	Screenshot Mode = "screenshot"
	Record     Mode = "record"
)

// Selection is returned by a control panel before capture begins.
type Selection struct {
	Mode     Mode
	Target   Target
	Path     string
	Video    VideoSettings
	Duration time.Duration // zero records until context cancellation
}

// Selector must close its surface before returning an accepted selection.
type Selector interface {
	Select(context.Context, []Output) (Selection, bool, error)
}
