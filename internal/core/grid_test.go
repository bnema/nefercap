package core_test

import (
	"image"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

// gridWant builds the expected lines for a physical size and the pixel
// coordinates of the three vertical and three horizontal lines.
func gridWant(w, h int, xs, ys [3]int) core.GridLines {
	var g core.GridLines
	for i := range 3 {
		g[i] = image.Rect(xs[i], 0, xs[i]+1, h)
		g[3+i] = image.Rect(0, ys[i], w, ys[i]+1)
	}
	return g
}

func TestMonitorGridExactPhysicalLines(t *testing.T) {
	for name, c := range map[string]struct {
		w, h  int
		scale float64
		want  core.GridLines
	}{
		"1920x1080 at 1":    {1920, 1080, 1, gridWant(1920, 1080, [3]int{640, 960, 1280}, [3]int{360, 540, 720})},
		"1600x900 at 1.2":   {1600, 900, 1.2, gridWant(1920, 1080, [3]int{640, 960, 1280}, [3]int{360, 540, 720})},
		"1536x864 at 1.25":  {1536, 864, 1.25, gridWant(1920, 1080, [3]int{640, 960, 1280}, [3]int{360, 540, 720})},
		"1280x720 at 1.5":   {1280, 720, 1.5, gridWant(1920, 1080, [3]int{640, 960, 1280}, [3]int{360, 540, 720})},
		"960x540 at 2":      {960, 540, 2, gridWant(1920, 1080, [3]int{640, 960, 1280}, [3]int{360, 540, 720})},
		"odd 1366x768 at 1": {1366, 768, 1, gridWant(1366, 768, [3]int{455, 683, 910}, [3]int{256, 384, 512})},
		// 1367*1.25 = 1708.75 and 769*1.25 = 961.25 round up to whole buffer pixels.
		"odd fractional 1367x769 at 1.25": {1367, 769, 1.25, gridWant(1709, 962, [3]int{569, 854, 1139}, [3]int{320, 481, 641})},
		// 1001*1.2 = 1201.2 and 601*1.2 = 721.2.
		"odd fractional 1001x601 at 1.2": {1001, 601, 1.2, gridWant(1202, 722, [3]int{400, 601, 801}, [3]int{240, 361, 481})},
		"1x1 at 1":                       {1, 1, 1, gridWant(1, 1, [3]int{}, [3]int{})},
		"2x2 at 1":                       {2, 2, 1, gridWant(2, 2, [3]int{0, 1, 1}, [3]int{0, 1, 1})},
		"3x3 at 1":                       {3, 3, 1, gridWant(3, 3, [3]int{1, 1, 2}, [3]int{1, 1, 2})},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := core.MonitorGrid(c.w, c.h, c.scale)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
			for i, r := range got {
				if i < 3 {
					assert.Equal(t, 1, r.Dx(), "vertical %d is one physical pixel wide", i)
				} else {
					assert.Equal(t, 1, r.Dy(), "horizontal %d is one physical pixel tall", i)
				}
			}
		})
	}
}

func TestMonitorGridLineOrderAndBounds(t *testing.T) {
	g, err := core.MonitorGrid(1920, 1080, 1)
	require.NoError(t, err)
	assert.Equal(t, core.GridLineCount, len(g))
	assert.Less(t, g[0].Min.X, g[1].Min.X)
	assert.Less(t, g[1].Min.X, g[2].Min.X)
	assert.Less(t, g[3].Min.Y, g[4].Min.Y)
	assert.Less(t, g[4].Min.Y, g[5].Min.Y)
	whole := image.Rect(0, 0, 1920, 1080)
	for _, r := range g {
		assert.True(t, r.In(whole), "%v stays inside the physical monitor", r)
	}
}

func TestPhysicalExtentMatchesExactFractionalScales(t *testing.T) {
	// Compositor fractional scales are k/120. The buffer edge is
	// ceil(logical*k/120) computed exactly in integers; float noise at exact
	// boundaries (for example 1600*1.2) must not add a pixel.
	for k := 120; k <= 480; k++ {
		scale := float64(k) / 120
		for n := 1; n <= ports.MaxDimension; n += 1 + n/97 {
			want := (n*k + 119) / 120
			got, err := core.PhysicalExtent(n, scale)
			require.NoError(t, err)
			if got != want {
				t.Fatalf("PhysicalExtent(%d, %d/120) = %d, want %d", n, k, got, want)
			}
		}
		for n := 1; n <= 4096; n++ { // every small size
			want := (n*k + 119) / 120
			got, err := core.PhysicalExtent(n, scale)
			require.NoError(t, err)
			if got != want {
				t.Fatalf("PhysicalExtent(%d, %d/120) = %d, want %d", n, k, got, want)
			}
		}
	}
}

func TestMonitorGridSweepInvariants(t *testing.T) {
	for k := 120; k <= 360; k += 7 {
		scale := float64(k) / 120
		for w := 1; w <= 3000; w += 13 {
			h := w/2 + 1
			g, err := core.MonitorGrid(w, h, scale)
			require.NoError(t, err)
			pw, _ := core.PhysicalExtent(w, scale)
			ph, _ := core.PhysicalExtent(h, scale)
			for i, r := range g {
				if !r.In(image.Rect(0, 0, pw, ph)) || r.Empty() {
					t.Fatalf("%dx%d@%d/120 line %d = %v outside %dx%d", w, h, k, i, r, pw, ph)
				}
				if i < 3 && (r.Dx() != 1 || r.Dy() != ph) || i >= 3 && (r.Dy() != 1 || r.Dx() != pw) {
					t.Fatalf("%dx%d@%d/120 line %d = %v is not one pixel wide over the full monitor", w, h, k, i, r)
				}
			}
			if g[0].Min.X > g[1].Min.X || g[1].Min.X > g[2].Min.X || g[3].Min.Y > g[4].Min.Y || g[4].Min.Y > g[5].Min.Y {
				t.Fatalf("%dx%d@%d/120 lines out of order: %v", w, h, k, g)
			}
		}
	}
}

func TestMonitorGridRejectsInvalidInput(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for name, c := range map[string]struct {
		w, h  int
		scale float64
	}{
		"zero width":        {0, 1080, 1},
		"zero height":       {1920, 0, 1},
		"negative width":    {-1, 1080, 1},
		"negative height":   {1920, -1, 1},
		"width too large":   {ports.MaxDimension + 1, 1080, 1},
		"height too large":  {1920, ports.MaxDimension + 1, 1},
		"zero scale":        {1920, 1080, 0},
		"negative scale":    {1920, 1080, -1},
		"NaN scale":         {1920, 1080, nan},
		"infinite scale":    {1920, 1080, inf},
		"negative infinity": {1920, 1080, -inf},
		"scale too large":   {1920, 1080, core.MaxGridScale * 2},
		"vanishing scale":   {1, 1, 1e-12},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := core.MonitorGrid(c.w, c.h, c.scale)
			assert.ErrorIs(t, err, core.ErrInvalidGrid)
			assert.Equal(t, core.GridLines{}, g, "no partial result")
		})
	}
	_, err := core.PhysicalExtent(0, 1)
	assert.ErrorIs(t, err, core.ErrInvalidGrid)
	_, err = core.PhysicalExtent(10, nan)
	assert.ErrorIs(t, err, core.ErrInvalidGrid)
}

func TestMonitorGridAcceptsLimits(t *testing.T) {
	g, err := core.MonitorGrid(ports.MaxDimension, ports.MaxDimension, core.MaxGridScale)
	require.NoError(t, err)
	pw := ports.MaxDimension * core.MaxGridScale
	assert.Equal(t, pw, g[3].Dx())
}

func TestMonitorGridAllocations(t *testing.T) {
	var g core.GridLines
	n := testing.AllocsPerRun(200, func() {
		g, _ = core.MonitorGrid(1600, 900, 1.2)
		_, _ = core.MonitorGrid(0, 900, 1.2) // the error path too
	})
	assert.Zero(t, n)
	_ = g
}

func BenchmarkMonitorGrid(b *testing.B) {
	b.ReportAllocs()
	var g core.GridLines
	for i := 0; b.Loop(); i++ {
		g, _ = core.MonitorGrid(1600+i%3, 900, 1.2)
	}
	_ = g
}
