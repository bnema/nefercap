package core_test

import (
	"image"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

func TestMonitorGridPhysicalUsesTheGivenPixels(t *testing.T) {
	// An odd 901x601 output mode: the lines are its own thirds, one pixel each.
	g, err := core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	assert.Equal(t, gridWant(901, 601, [3]int{300, 450, 600}, [3]int{200, 300, 400}), g)

	same, err := core.MonitorGridPhysical(1920, 1080)
	require.NoError(t, err)
	fromScale, err := core.MonitorGrid(1600, 900, 1.2)
	require.NoError(t, err)
	assert.Equal(t, fromScale, same, "an exact-fit buffer and mode agree")
}

func TestMonitorGridPhysicalSpansItsOwnSize(t *testing.T) {
	g, err := core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	for i, r := range g {
		assert.True(t, r.In(image.Rect(0, 0, 901, 601)), "line %d %v", i, r)
		if i < 3 {
			assert.Equal(t, image.Rect(r.Min.X, 0, r.Min.X+1, 601), r)
		} else {
			assert.Equal(t, image.Rect(0, r.Min.Y, 901, r.Min.Y+1), r)
		}
	}
}

func TestMonitorGridPhysicalValidation(t *testing.T) {
	const limit = ports.MaxDimension * core.MaxGridScale
	for name, c := range map[string]struct{ w, h int }{
		"zero width":      {0, 600},
		"zero height":     {900, 0},
		"negative width":  {-1, 600},
		"negative height": {900, -5},
		"width too large": {limit + 1, 600},
		"height too high": {900, limit + 1},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := core.MonitorGridPhysical(c.w, c.h)
			assert.ErrorIs(t, err, core.ErrInvalidGrid)
			assert.Equal(t, core.GridLines{}, g)
		})
	}
	// A physical mode may exceed MaxDimension when the scale is above 1.
	g, err := core.MonitorGridPhysical(limit, limit)
	require.NoError(t, err)
	assert.Equal(t, limit, g[3].Dx())
	_, err = core.MonitorGridPhysical(1, 1)
	assert.NoError(t, err)
}

func TestMonitorGridPhysicalAllocations(t *testing.T) {
	var g core.GridLines
	n := testing.AllocsPerRun(200, func() {
		g, _ = core.MonitorGridPhysical(901, 601)
		_, _ = core.MonitorGridPhysical(0, 601)
	})
	assert.Zero(t, n)
	_ = g
}

func BenchmarkMonitorGridPhysical(b *testing.B) {
	b.ReportAllocs()
	var g core.GridLines
	for i := 0; b.Loop(); i++ {
		g, _ = core.MonitorGridPhysical(901+i%3, 601)
	}
	_ = g
}
