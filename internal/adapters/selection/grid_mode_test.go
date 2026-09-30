package selection

import (
	"image"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

// modeModel is a one-output overlay whose output mode is mw x mh and whose
// surface is w x h logical at scale.
func modeModel(t *testing.T, mw, mh, w, h int, scale float64) *model {
	t.Helper()
	sess, _ := testSession()
	outs := []ports.Output{{ID: 7, Name: "DP-1", Width: mw, Height: mh, Scale: 1}}
	m := newModel(ports.Screenshot, outs, 0, sess, true)
	m.resize(w, h, scale)
	require.Equal(t, core.PickActive, m.picker.Status())
	return m
}

// renderedGrid returns the scene's lines as the renderer places them.
func renderedGrid(m *model) (g core.GridLines) {
	for i, r := range m.scene().grid {
		g[i] = physicalEdges(r, m.scale)
	}
	return g
}

func TestGridFollowsTheOutputModeNotTheBuffer(t *testing.T) {
	// The logical surface is round(mode/scale); its buffer is ceil(logical*scale)
	// and can be one pixel larger than the mode, which the compositor crops.
	// The guides must sit on the visible mode's thirds.
	want, err := core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	for _, scale := range []float64{1.2, 1.25, 1.5, 2} {
		w, h := int(math.Round(901/scale)), int(math.Round(601/scale))
		m := modeModel(t, 901, 601, w, h, scale)
		got := renderedGrid(m)
		assert.Equal(t, want, got, "scale %g (%dx%d logical)", scale, w, h)
		for i, x := range []int{300, 450, 600} {
			assert.Equal(t, image.Rect(x, 0, x+1, 601), got[i], "scale %g vertical %d", scale, i)
		}
		for i, y := range []int{200, 300, 400} {
			assert.Equal(t, image.Rect(0, y, 901, y+1), got[3+i], "scale %g horizontal %d", scale, i)
		}
		bw, _ := core.PhysicalExtent(w, scale)
		bh, _ := core.PhysicalExtent(h, scale)
		for i, r := range got {
			assert.True(t, r.In(image.Rect(0, 0, bw, bh)), "scale %g line %d stays inside the %dx%d buffer", scale, i, bw, bh)
		}
	}
}

func TestGridFallsBackToTheBufferWithoutAMatchingMode(t *testing.T) {
	buffer, err := core.MonitorGrid(751, 501, 1.2)
	require.NoError(t, err)
	for name, c := range map[string]struct{ mw, mh int }{
		"unknown mode":      {0, 0},
		"negative mode":     {-901, -601},
		"width mismatches":  {1920, 601},
		"height mismatches": {901, 1080},
		"stale config":      {1920, 1080},
		"one pixel off":     {903, 601}, // 903/1.2 rounds to 753
	} {
		m := modeModel(t, c.mw, c.mh, 751, 501, 1.2)
		assert.Equal(t, buffer, renderedGrid(m), name)
	}
}

func TestGridModeIsCheckedAgainstEveryResize(t *testing.T) {
	m := modeModel(t, 901, 601, 751, 501, 1.2)
	mode, err := core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	assert.Equal(t, mode, renderedGrid(m))

	m.resize(751, 501, 1.25) // scale only: 901/1.25 is no longer 751
	buffer, err := core.MonitorGrid(751, 501, 1.25)
	require.NoError(t, err)
	assert.Equal(t, buffer, renderedGrid(m), "the mode no longer explains the surface")

	m.resize(721, 481, 1.25) // round(901/1.25) x round(601/1.25)
	mode, err = core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	assert.Equal(t, mode, renderedGrid(m), "and matches again when it does")
}

func TestGridUsesRotatedModeWhenItMatchesSwapped(t *testing.T) {
	// A mode reported as 601x901 with a surface of 751x501 logical.
	m := modeModel(t, 601, 901, 751, 501, 1.2)
	want, err := core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	assert.Equal(t, want, renderedGrid(m))

	// An unrelated swapped mode is ignored.
	m = modeModel(t, 1080, 1920, 751, 501, 1.2)
	buffer, err := core.MonitorGrid(751, 501, 1.2)
	require.NoError(t, err)
	assert.Equal(t, buffer, renderedGrid(m))
}

func TestGridBufferMoreThanOnePixelPastTheModeFallsBack(t *testing.T) {
	// At scale 3 the surface is round(901/3) = 300 wide, a 900 buffer: the
	// mode is larger than the buffer, so the compositor resamples and the
	// mode is not a safe position source.
	m := modeModel(t, 901, 601, 300, 200, 3)
	buffer, err := core.MonitorGrid(300, 200, 3)
	require.NoError(t, err)
	assert.Equal(t, buffer, renderedGrid(m))

	// A surface size the mode does not round to: 1000/3 is 333, not 334.
	m = modeModel(t, 1000, 1000, 334, 334, 3)
	buffer, err = core.MonitorGrid(334, 334, 3)
	require.NoError(t, err)
	assert.Equal(t, buffer, renderedGrid(m))
}

func TestGridModePerOutputIsIndependent(t *testing.T) {
	sess, _ := testSession()
	outs := []ports.Output{
		{ID: 7, Name: "DP-1", Width: 901, Height: 601, Scale: 1},
		{ID: 9, Name: "HDMI-A-1", Width: 1280, Height: 720, Scale: 1},
	}
	a := newModel(ports.Screenshot, outs, 0, sess, true)
	b := newModel(ports.Screenshot, outs, 1, sess, true)
	a.resize(751, 501, 1.2)
	b.resize(1280, 720, 1)
	wantA, err := core.MonitorGridPhysical(901, 601)
	require.NoError(t, err)
	wantB, err := core.MonitorGridPhysical(1280, 720)
	require.NoError(t, err)
	assert.Equal(t, wantA, renderedGrid(a))
	assert.Equal(t, wantB, renderedGrid(b))
}

func TestGridModeAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	m := modeModel(t, 901, 601, 751, 501, 1.2)
	n := testing.AllocsPerRun(200, func() {
		m.resize(751, 501, 1.2)
		m.resize(751, 501, 1.25)
		_ = m.scene()
	})
	assert.Zero(t, n)
}
