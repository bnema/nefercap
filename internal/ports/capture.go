// Package ports defines framework-independent capture contracts.
package ports

import (
	"context"
	"io"
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
	// WorkspaceID is a Source-local identity from Source.Workspaces, never an
	// index or monitor alias. Nonzero requires a zero Region. A workspace
	// that is not displayed can be captured only if the compositor supports
	// it; otherwise it is captured as its output while Active.
	WorkspaceID uint64
}

// Capabilities says what the compositor behind a Source offers beyond
// capturing whole outputs. It is fixed once the Source is created.
type Capabilities struct {
	// Workspaces: Source.Workspaces lists the compositor's workspaces.
	Workspaces bool
	// Exclusion: a client's own layer surfaces can be left out of its frames,
	// so a recording indicator can be shown without appearing in the video.
	Exclusion bool
}

// Workspace is compositor workspace metadata. Region is output-local, logical
// geometry (the whole output). Active means this workspace is displayed.
type Workspace struct {
	ID       uint64
	OutputID uint32
	Name     string
	Region   Region
	Active   bool
}

// PixelFormat names the native little-endian wl_shm 32-bit pixel layout.
type PixelFormat uint32

const (
	ARGB8888 PixelFormat = 0
	XRGB8888 PixelFormat = 1
)

// Frame borrows storage from Source until the next Capture or Close call.
// Consumers must finish reading synchronously and must not retain Pixels.
// Output is opaque SDR.
type Frame struct {
	Pixels                []byte
	Width, Height, Stride int
	Format                PixelFormat
}

// Source has one sequential owner; cancellation must interrupt blocked I/O.
// Cancellation errors wrap ctx.Err(), not a custom cancellation cause.
// A failed Capture invalidates its borrowed frame. Close is idempotent.
type Source interface {
	Capabilities() Capabilities
	Outputs(context.Context) ([]Output, error)
	// Workspaces returns nil without error when the compositor has none.
	Workspaces(context.Context) ([]Workspace, error)
	Capture(context.Context, Target) (Frame, error)
	Close() error
}

// ScreenshotWriter writes an opaque PNG without overwriting an existing path.
// Save completes all reads from borrowed frame storage before returning.
type ScreenshotWriter interface {
	Save(context.Context, Frame, string) error
	// Encode consumes borrowed pixels synchronously without creating a file.
	Encode(context.Context, io.Writer, Frame) error
}

// Clipboard accepts PNG bytes synchronously. Its owner may outlive this process.
type Clipboard interface {
	Copy(context.Context, io.Reader) error
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
// Abort as the first terminal operation interrupts writes and discards output.
// Abort is safe concurrently with Write or a draining Close. During Close it
// escalates shutdown unless Close already observed encoder exit; Close's result
// is authoritative. Abort after completed Close is a no-op. Close after Abort
// returns Abort's cleanup result. Close preserves frames on a clean exit.
// Cancellation errors wrap ctx.Err(); unrelated failures must be preserved.
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
	Mode      Mode
	Target    Target
	Path      string
	Clipboard bool // screenshot only; permits an empty Path
	Video     VideoSettings
	Duration  time.Duration // zero records until context cancellation
}

// Selector must close its surface before returning an accepted selection.
type Selector interface {
	Select(context.Context, []Output) (Selection, bool, error)
}
