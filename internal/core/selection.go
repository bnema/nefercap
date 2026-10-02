package core

import (
	"errors"
	"math"

	"github.com/bnema/nefercap/internal/ports"
)

// Picker selection thresholds, in output-local logical pixels.
const (
	// DragThreshold is the pointer travel from the press point at which a
	// press becomes a drag instead of a click.
	DragThreshold = 4
	// MinRegionSize is the smallest accepted dragged region edge.
	MinRegionSize = 4
)

// ErrInvalidBounds reports a Picker started with an unusable output or size.
var ErrInvalidBounds = errors.New("invalid picker bounds")

// PickKind is what a selection covers.
type PickKind uint8

const (
	// PickRegion is a dragged rectangle. It is also the default picker kind,
	// where a click without a drag picks the whole monitor.
	PickRegion PickKind = iota
	// PickMonitor is one whole output.
	PickMonitor
	// PickWorkspace is the geometry of a workspace whose identity the caller
	// supplied with SetWorkspace.
	PickWorkspace
)

// PickStatus is the picker life cycle.
type PickStatus uint8

const (
	// PickIdle is the state before Begin.
	PickIdle PickStatus = iota
	PickActive
	PickAccepted
	PickCanceled
)

// PickResult is an accepted selection. Region is explicit output-local
// logical geometry for drawing: the dragged rectangle, the full output for a
// monitor, or the workspace rectangle the caller supplied. WorkspaceID is
// nonzero only for PickWorkspace. Use Target for the capture request.
type PickResult struct {
	OutputID    uint32
	Region      ports.Region
	Kind        PickKind
	WorkspaceID uint64
}

// Target converts the result to a capture target; selectors must use it
// rather than map results themselves. A monitor is the zero region of its
// output. A region carries its geometry. A workspace carries only its exact
// Source-local WorkspaceID: the compositor supplies live geometry at capture, so
// the Region snapshot in PickResult is for drawing only and is not cached in
// the target. A workspace is never converted to a monitor. Sizes are logical:
// the caller decides physical resolution and rounding.
func (r PickResult) Target() ports.Target {
	switch r.Kind {
	case PickMonitor:
		return ports.Target{OutputID: r.OutputID}
	case PickWorkspace:
		return ports.Target{OutputID: r.OutputID, WorkspaceID: r.WorkspaceID}
	}
	return ports.Target{OutputID: r.OutputID, Region: r.Region}
}

// Picker is the pure selection state for one output. It has no framework
// types, performs no allocation per event and is not safe for concurrent use.
// Coordinates are output-local logical pixels. Mouse methods model the left
// button only; callers filter other buttons. Every mutating method returns
// whether visible state changed, so callers redraw only then.
//
// Interaction: dragging past DragThreshold and releasing accepts a region;
// a click accepts the whole monitor (or, after SelectWorkspace, the
// workspace); Confirm accepts the kind chosen by SelectMonitor or
// SelectWorkspace; Cancel abandons the pick.
type Picker struct {
	status   PickStatus
	kind     PickKind
	outputID uint32
	width    int
	height   int

	px, py float64 // clamped pointer
	ax, ay float64 // clamped press point
	rect   ports.Region
	pressed,
	dragging bool

	wsID   uint64
	wsRect ports.Region

	result PickResult
}

// Begin (re)starts the picker on an output of the given logical size,
// clearing all state including any workspace.
func (p *Picker) Begin(outputID uint32, width, height int) error {
	if outputID == 0 || width < 1 || height < 1 || width > ports.MaxDimension || height > ports.MaxDimension {
		*p = Picker{}
		return ErrInvalidBounds
	}
	*p = Picker{status: PickActive, outputID: outputID, width: width, height: height}
	return nil
}

// Status returns the life-cycle state.
func (p *Picker) Status() PickStatus { return p.status }

// Kind returns the chosen kind (PickRegion by default).
func (p *Picker) Kind() PickKind { return p.kind }

// Dragging reports whether a press has passed DragThreshold.
func (p *Picker) Dragging() bool { return p.dragging }

// Rect returns the normalized live drag rectangle; ok is false when not
// dragging.
func (p *Picker) Rect() (r ports.Region, ok bool) { return p.rect, p.dragging }

// Bounds returns the output logical size.
func (p *Picker) Bounds() (width, height int) { return p.width, p.height }

// Workspace returns the supplied workspace identity and its rectangle
// clipped to the output; ok is false when none is available.
func (p *Picker) Workspace() (id uint64, r ports.Region, ok bool) {
	return p.wsID, p.wsRect, p.wsID != 0
}

// Result returns the accepted selection; ok is false unless Status is
// PickAccepted.
func (p *Picker) Result() (PickResult, bool) {
	return p.result, p.status == PickAccepted
}

// SetWorkspace supplies the current workspace identity and rectangle in
// output-local logical coordinates. Only a nonzero id with a rectangle that
// overlaps the output is eligible; the rectangle is clipped to the output.
// Anything else clears the workspace (and leaves PickWorkspace for
// PickRegion). It never invents an identity.
func (p *Picker) SetWorkspace(id uint64, r ports.Region) bool {
	if p.status != PickActive {
		return false
	}
	clipped, ok := p.clip(r)
	if id == 0 || !ok {
		id, clipped = 0, ports.Region{}
	}
	if id == p.wsID && clipped == p.wsRect {
		return false
	}
	p.wsID, p.wsRect = id, clipped
	if id == 0 && p.kind == PickWorkspace {
		p.kind = PickRegion
	}
	return true
}

// SelectWorkspace makes Confirm and click pick the supplied workspace. It
// does nothing when no eligible workspace was supplied.
func (p *Picker) SelectWorkspace() bool { return p.setKind(PickWorkspace) }

// SelectMonitor makes Confirm pick the whole monitor.
func (p *Picker) SelectMonitor() bool { return p.setKind(PickMonitor) }

// SelectRegion returns to the default kind.
func (p *Picker) SelectRegion() bool { return p.setKind(PickRegion) }

func (p *Picker) setKind(k PickKind) bool {
	if p.status != PickActive || p.pressed || p.kind == k || (k == PickWorkspace && p.wsID == 0) {
		return false
	}
	p.kind = k
	return true
}

// MouseDown starts a press at (x, y).
func (p *Picker) MouseDown(x, y float64) bool {
	if p.status != PickActive || p.pressed {
		return false
	}
	p.setPointer(x, y)
	p.pressed, p.dragging = true, false
	p.ax, p.ay = p.px, p.py
	p.rect = ports.Region{}
	return true
}

// MouseMove moves the pointer, growing the drag once past DragThreshold.
func (p *Picker) MouseMove(x, y float64) bool {
	if p.status != PickActive {
		return false
	}
	changed := p.setPointer(x, y)
	if !p.pressed {
		return changed
	}
	if !p.dragging {
		dx, dy := p.px-p.ax, p.py-p.ay
		if dx*dx+dy*dy < DragThreshold*DragThreshold {
			return changed
		}
		p.dragging = true
		changed = true
	}
	if r := p.dragRect(); r != p.rect {
		p.rect = r
		changed = true
	}
	return changed
}

// MouseUp ends the press. A drag accepts its region when both edges reach
// MinRegionSize, otherwise the drag is discarded and picking continues. A
// click accepts the workspace when PickWorkspace is chosen, else the monitor.
func (p *Picker) MouseUp(x, y float64) bool {
	if p.status != PickActive || !p.pressed {
		return false
	}
	p.setPointer(x, y)
	p.pressed = false
	if !p.dragging {
		if p.kind == PickWorkspace && p.wsID != 0 {
			p.accept(PickWorkspace)
		} else {
			p.accept(PickMonitor)
		}
		return true
	}
	p.dragging = false
	r := p.dragRect()
	p.rect = ports.Region{}
	if r.Width < MinRegionSize || r.Height < MinRegionSize {
		return true
	}
	p.status = PickAccepted
	p.result = PickResult{OutputID: p.outputID, Region: r, Kind: PickRegion}
	return true
}

// Confirm accepts the chosen monitor or workspace. It does nothing for the
// default region kind, whose selection is made by the mouse.
func (p *Picker) Confirm() bool {
	if p.status != PickActive || p.pressed {
		return false
	}
	switch {
	case p.kind == PickMonitor:
		p.accept(PickMonitor)
	case p.kind == PickWorkspace && p.wsID != 0:
		p.accept(PickWorkspace)
	default:
		return false
	}
	return true
}

// Cancel abandons the pick (Escape).
func (p *Picker) Cancel() bool {
	if p.status != PickActive {
		return false
	}
	p.status = PickCanceled
	p.pressed, p.dragging = false, false
	p.rect = ports.Region{}
	return true
}

func (p *Picker) accept(k PickKind) {
	p.status = PickAccepted
	switch k {
	case PickWorkspace:
		p.result = PickResult{OutputID: p.outputID, Region: p.wsRect, Kind: PickWorkspace, WorkspaceID: p.wsID}
	default:
		p.result = PickResult{OutputID: p.outputID, Region: ports.Region{Width: p.width, Height: p.height}, Kind: PickMonitor}
	}
}

// setPointer clamps into the output and reports movement. NaN is ignored;
// +Inf and -Inf clamp to the output edges like any out-of-range value.
func (p *Picker) setPointer(x, y float64) bool {
	if math.IsNaN(x) || math.IsNaN(y) {
		return false
	}
	x = math.Min(math.Max(x, 0), float64(p.width))
	y = math.Min(math.Max(y, 0), float64(p.height))
	moved := x != p.px || y != p.py
	p.px, p.py = x, y
	return moved
}

// dragRect is the normalized rectangle between press and pointer, rounded to
// whole logical pixels.
func (p *Picker) dragRect() ports.Region {
	x0, x1 := math.Round(math.Min(p.ax, p.px)), math.Round(math.Max(p.ax, p.px))
	y0, y1 := math.Round(math.Min(p.ay, p.py)), math.Round(math.Max(p.ay, p.py))
	return ports.Region{X: int(x0), Y: int(y0), Width: int(x1 - x0), Height: int(y1 - y0)}
}

// clip intersects r with the output; ok is false when empty or invalid.
func (p *Picker) clip(r ports.Region) (ports.Region, bool) {
	x0, y0 := max(int64(r.X), 0), max(int64(r.Y), 0)
	x1 := min(int64(r.X)+int64(r.Width), int64(p.width))
	y1 := min(int64(r.Y)+int64(r.Height), int64(p.height))
	if r.Width < 1 || r.Height < 1 || x1 <= x0 || y1 <= y0 {
		return ports.Region{}, false
	}
	return ports.Region{X: int(x0), Y: int(y0), Width: int(x1 - x0), Height: int(y1 - y0)}, true
}
