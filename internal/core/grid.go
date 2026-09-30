package core

import (
	"errors"
	"image"
	"math"

	"github.com/bnema/nefercap/internal/ports"
)

// GridLineCount is the number of guide lines of a monitor grid.
const GridLineCount = 6

// MaxGridScale is the largest output scale a grid accepts. It bounds the
// physical size to MaxDimension * MaxGridScale, far inside int32.
const MaxGridScale = 8

// scaleEpsilon absorbs binary floating-point noise at exact integer
// boundaries (1600 * 1.2 is 1920.0000000000002), like the surface buffer
// size the window system derives from the same logical size and scale.
const scaleEpsilon = 1e-9

// ErrInvalidGrid reports a grid requested for an unusable size or scale.
var ErrInvalidGrid = errors.New("invalid grid size or scale")

// GridLines are the guide lines of a monitor in physical pixels, origin at the
// monitor's top-left: three vertical lines (1/3, 1/2, 2/3 of the width) then
// three horizontal lines (1/3, 1/2, 2/3 of the height). Each line is one
// physical pixel thick and spans the whole monitor. It is a plain value.
type GridLines [GridLineCount]image.Rectangle

// PhysicalExtent returns the physical pixel count of a logical extent at a
// possibly fractional scale: ceil(logical * scale), computed like the surface
// buffer size with a small epsilon so float noise never adds a pixel. It
// fails with ErrInvalidGrid for a logical extent outside 1..MaxDimension, a
// scale that is not finite or above MaxGridScale, or a result below one pixel.
func PhysicalExtent(logical int, scale float64) (int, error) {
	if logical < 1 || logical > ports.MaxDimension ||
		math.IsNaN(scale) || scale <= 0 || scale > MaxGridScale {
		return 0, ErrInvalidGrid
	}
	n := int(math.Ceil(float64(logical)*scale - scaleEpsilon))
	if n < 1 {
		return 0, ErrInvalidGrid
	}
	return n, nil
}

// MonitorGrid returns the fixed guide lines of a whole monitor of the given
// logical size at the given scale, on the surface buffer PhysicalExtent gives
// for each axis. Every line coordinate is snapped to a whole physical pixel
// first (floor of the exact 1/3, 1/2 and 2/3 position) and is exactly one
// pixel thick, so callers convert to logical units only afterwards. It does
// not allocate and returns the zero value with ErrInvalidGrid on bad input.
//
// The buffer can be a pixel larger than the output mode when the logical size
// was rounded; use MonitorGridPhysical with the mode when it is known.
func MonitorGrid(width, height int, scale float64) (GridLines, error) {
	pw, err := PhysicalExtent(width, scale)
	if err != nil {
		return GridLines{}, err
	}
	ph, err := PhysicalExtent(height, scale)
	if err != nil {
		return GridLines{}, err
	}
	return physicalGrid(pw, ph), nil
}

// MonitorGridPhysical returns the guide lines of a monitor whose physical
// size is exactly width x height pixels, for example the output mode. The
// lines are snapped and one pixel thick as in MonitorGrid and span the given
// size. Each size must be 1..MaxDimension*MaxGridScale, since a physical mode
// can exceed MaxDimension when the scale is above 1. It does not allocate and
// returns the zero value with ErrInvalidGrid on bad input.
func MonitorGridPhysical(width, height int) (GridLines, error) {
	const limit = ports.MaxDimension * MaxGridScale
	if width < 1 || width > limit || height < 1 || height > limit {
		return GridLines{}, ErrInvalidGrid
	}
	return physicalGrid(width, height), nil
}

// physicalGrid lays the lines out on an exact physical size of at least 1x1.
func physicalGrid(pw, ph int) GridLines {
	var g GridLines
	// Integer floor division keeps the snap exact for every size.
	xs := [3]int{pw / 3, pw / 2, pw * 2 / 3}
	ys := [3]int{ph / 3, ph / 2, ph * 2 / 3}
	for i := range xs {
		g[i] = image.Rect(xs[i], 0, xs[i]+1, ph)
		g[3+i] = image.Rect(0, ys[i], pw, ys[i]+1)
	}
	return g
}
