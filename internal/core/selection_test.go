package core_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

func newPicker(t testing.TB, id uint32, w, h int) *core.Picker {
	t.Helper()
	p := &core.Picker{}
	require.NoError(t, p.Begin(id, w, h))
	return p
}

func TestPickerBeginValidation(t *testing.T) {
	for name, c := range map[string]struct {
		id   uint32
		w, h int
	}{
		"zero id":     {0, 100, 100},
		"zero width":  {1, 0, 100},
		"neg height":  {1, 100, -1},
		"too large":   {1, ports.MaxDimension + 1, 100},
		"too large h": {1, 100, ports.MaxDimension + 1},
	} {
		t.Run(name, func(t *testing.T) {
			p := &core.Picker{}
			assert.ErrorIs(t, p.Begin(c.id, c.w, c.h), core.ErrInvalidBounds)
			assert.Equal(t, core.PickIdle, p.Status())
			assert.False(t, p.MouseDown(1, 1))
		})
	}
	p := newPicker(t, 3, 1920, 1080)
	assert.Equal(t, core.PickActive, p.Status())
	assert.Equal(t, core.PickRegion, p.Kind())
}

func TestPickerDragAcceptsOnRelease(t *testing.T) {
	p := newPicker(t, 5, 1000, 800)
	assert.True(t, p.MouseDown(100, 200))
	assert.True(t, p.MouseMove(250, 300))
	r, ok := p.Rect()
	assert.True(t, ok)
	assert.Equal(t, ports.Region{X: 100, Y: 200, Width: 150, Height: 100}, r)
	assert.Equal(t, core.PickActive, p.Status(), "not accepted before release")
	_, ok = p.Result()
	assert.False(t, ok)

	assert.True(t, p.MouseUp(250, 300))
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickResult{OutputID: 5, Region: ports.Region{X: 100, Y: 200, Width: 150, Height: 100}, Kind: core.PickRegion}, res)
	assert.Equal(t, ports.Target{OutputID: 5, Region: res.Region}, res.Target())
}

func TestPickerReverseDragNormalizes(t *testing.T) {
	for name, pts := range map[string][4]float64{
		"up-left":    {300, 300, 100, 120},
		"up-right":   {100, 300, 300, 120},
		"down-left":  {300, 120, 100, 300},
		"down-right": {100, 120, 300, 300},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPicker(t, 1, 1000, 1000)
			p.MouseDown(pts[0], pts[1])
			p.MouseMove(pts[2], pts[3])
			p.MouseUp(pts[2], pts[3])
			res, ok := p.Result()
			require.True(t, ok)
			assert.Equal(t, ports.Region{X: 100, Y: 120, Width: 200, Height: 180}, res.Region)
		})
	}
}

func TestPickerClampsToOutput(t *testing.T) {
	p := newPicker(t, 1, 500, 400)
	p.MouseDown(-50, -20)
	p.MouseMove(9000, 9000)
	r, dragging := p.Rect()
	require.True(t, dragging)
	assert.Equal(t, ports.Region{Width: 500, Height: 400}, r, "live drag is clamped too")
	p.MouseUp(9000, 9000)
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, ports.Region{Width: 500, Height: 400}, res.Region)
	assert.Equal(t, core.PickRegion, res.Kind, "a dragged full-output region stays a region")

	// Never leaves the output, wherever it starts.
	q := newPicker(t, 1, 500, 400)
	q.MouseDown(480, 390)
	q.MouseMove(700, -100)
	q.MouseUp(700, -100)
	res, ok = q.Result()
	require.True(t, ok)
	assert.Equal(t, ports.Region{X: 480, Y: 0, Width: 20, Height: 390}, res.Region)

	// NaN is ignored; infinities clamp to the edges.
	n := newPicker(t, 1, 500, 400)
	assert.False(t, n.MouseMove(math.NaN(), 5))
	assert.False(t, n.MouseMove(5, math.NaN()))
	i := newPicker(t, 1, 500, 400)
	i.MouseDown(math.Inf(-1), math.Inf(-1))
	i.MouseMove(math.Inf(1), math.Inf(1))
	i.MouseUp(math.Inf(1), math.Inf(1))
	res, ok = i.Result()
	require.True(t, ok)
	assert.Equal(t, ports.Region{Width: 500, Height: 400}, res.Region)
}

func TestPickerThresholdAndClick(t *testing.T) {
	p := newPicker(t, 9, 1920, 1080)
	p.MouseDown(500, 500)
	assert.True(t, p.MouseMove(502, 502), "pointer moved")
	assert.False(t, p.Dragging(), "under threshold")
	assert.False(t, p.MouseMove(502, 502), "no change no redraw")
	assert.True(t, p.MouseMove(504, 500), "exactly the threshold starts a drag")
	assert.True(t, p.Dragging())

	// A click, even with jitter below the threshold, picks the monitor.
	c := newPicker(t, 9, 1920, 1080)
	c.MouseDown(500, 500)
	c.MouseMove(503, 500)
	assert.True(t, c.MouseUp(503, 500))
	res, ok := c.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickResult{OutputID: 9, Region: ports.Region{Width: 1920, Height: 1080}, Kind: core.PickMonitor}, res)
	assert.Equal(t, ports.Target{OutputID: 9}, res.Target(), "monitor is the full-output zero region")
}

func TestPickerMinimumSize(t *testing.T) {
	p := newPicker(t, 1, 1000, 1000)
	p.MouseDown(100, 100)
	p.MouseMove(300, 102) // past threshold horizontally, height 2
	assert.True(t, p.MouseUp(300, 102))
	assert.Equal(t, core.PickActive, p.Status(), "tiny drag is discarded")
	_, dragging := p.Rect()
	assert.False(t, dragging)

	// picking continues
	p.MouseDown(10, 10)
	p.MouseMove(60, 60)
	p.MouseUp(60, 60)
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, ports.Region{X: 10, Y: 10, Width: 50, Height: 50}, res.Region)
}

func TestPickerEscape(t *testing.T) {
	p := newPicker(t, 1, 100, 100)
	p.MouseDown(10, 10)
	p.MouseMove(50, 50)
	assert.True(t, p.Cancel())
	assert.Equal(t, core.PickCanceled, p.Status())
	_, ok := p.Result()
	assert.False(t, ok)
	assert.False(t, p.MouseUp(50, 50))
	assert.False(t, p.Confirm())
	assert.False(t, p.Cancel())
	assert.False(t, p.MouseDown(1, 1))
}

func TestPickerMonitorKeyAndConfirm(t *testing.T) {
	p := newPicker(t, 2, 2560, 1440)
	assert.False(t, p.Confirm(), "region kind needs a drag")
	assert.True(t, p.SelectMonitor())
	assert.False(t, p.SelectMonitor())
	assert.Equal(t, core.PickMonitor, p.Kind())
	assert.True(t, p.Confirm())
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickMonitor, res.Kind)
	assert.Equal(t, ports.Region{Width: 2560, Height: 1440}, res.Region)
	assert.Zero(t, res.WorkspaceID)

	// A click also picks the monitor when m was chosen, even under the pointer.
	q := newPicker(t, 2, 2560, 1440)
	q.MouseMove(1000, 1000)
	q.SelectMonitor()
	q.MouseDown(1000, 1000)
	q.MouseUp(1000, 1000)
	res, ok = q.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickMonitor, res.Kind)
}

func TestPickerWorkspaceNeedsRealIdentity(t *testing.T) {
	rect := ports.Region{X: 0, Y: 0, Width: 1900, Height: 1000}

	p := newPicker(t, 4, 1920, 1080)
	assert.False(t, p.SelectWorkspace(), "no workspace supplied")
	assert.False(t, p.SetWorkspace(0, rect), "zero id is not an identity")
	assert.False(t, p.SelectWorkspace())
	assert.False(t, p.SetWorkspace(7, ports.Region{}), "empty rect is not geometry")
	assert.False(t, p.SetWorkspace(7, ports.Region{X: 5000, Y: 0, Width: 10, Height: 10}), "outside output")
	assert.False(t, p.SetWorkspace(7, ports.Region{Width: -5, Height: 10}))
	_, _, ok := p.Workspace()
	assert.False(t, ok)
	assert.False(t, p.SelectWorkspace())

	assert.True(t, p.SetWorkspace(7, rect))
	assert.False(t, p.SetWorkspace(7, rect), "unchanged")
	assert.True(t, p.SelectWorkspace())
	assert.True(t, p.Confirm())
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickResult{OutputID: 4, Region: rect, Kind: core.PickWorkspace, WorkspaceID: 7}, res)
	assert.Equal(t, rect, res.Region, "geometry stays available for drawing")
	assert.Equal(t, ports.Target{OutputID: 4, WorkspaceID: 7}, res.Target())
}

func TestPickResultTargetWorkspaceKeepsIdentity(t *testing.T) {
	// A workspace that fills the whole output is still a workspace, not the
	// monitor: the exact ID survives and no geometry snapshot is cached.
	full := ports.Region{Width: 1920, Height: 1080}
	p := newPicker(t, 4, 1920, 1080)
	require.True(t, p.SetWorkspace(1<<40+5, full))
	require.True(t, p.SelectWorkspace())
	require.True(t, p.Confirm())
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, full, res.Region)
	assert.Equal(t, uint64(1<<40+5), res.WorkspaceID)
	got := res.Target()
	assert.Equal(t, ports.Target{OutputID: 4, WorkspaceID: 1<<40 + 5}, got)
	assert.Zero(t, got.Region)
	assert.NotEqual(t, ports.Target{OutputID: 4}, got)

	// Monitor and region results never carry a workspace ID.
	assert.Equal(t, ports.Target{OutputID: 4}, core.PickResult{OutputID: 4, Kind: core.PickMonitor, Region: full, WorkspaceID: 9}.Target())
	assert.Equal(t, ports.Target{OutputID: 4, Region: full}, core.PickResult{OutputID: 4, Kind: core.PickRegion, Region: full, WorkspaceID: 9}.Target())
}

func TestPickerWorkspaceClickAndClip(t *testing.T) {
	p := newPicker(t, 4, 1000, 500)
	p.SetWorkspace(11, ports.Region{X: -20, Y: 30, Width: 2000, Height: 2000})
	id, r, ok := p.Workspace()
	require.True(t, ok)
	assert.Equal(t, uint64(11), id)
	assert.Equal(t, ports.Region{X: 0, Y: 30, Width: 1000, Height: 470}, r, "clipped to the output")
	p.SelectWorkspace()
	p.MouseDown(5, 5)
	p.MouseUp(5, 5)
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickWorkspace, res.Kind)
	assert.Equal(t, uint64(11), res.WorkspaceID)

	// Drag still wins as a region even with workspace chosen.
	q := newPicker(t, 4, 1000, 500)
	q.SetWorkspace(11, ports.Region{Width: 1000, Height: 500})
	q.SelectWorkspace()
	q.MouseDown(10, 10)
	q.MouseMove(90, 90)
	q.MouseUp(90, 90)
	res, ok = q.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickRegion, res.Kind)
	assert.Zero(t, res.WorkspaceID)
}

func TestPickerWorkspaceLostFallsBack(t *testing.T) {
	p := newPicker(t, 4, 1000, 500)
	p.SetWorkspace(3, ports.Region{Width: 100, Height: 100})
	p.SelectWorkspace()
	assert.True(t, p.SetWorkspace(0, ports.Region{}))
	assert.Equal(t, core.PickRegion, p.Kind())
	assert.False(t, p.Confirm())
}

func TestPickerFullscreenMaxMonitor(t *testing.T) {
	const max = ports.MaxDimension
	p := newPicker(t, 1, max, max)
	p.MouseDown(0, 0)
	p.MouseMove(1e12, 1e12)
	p.MouseUp(1e12, 1e12)
	res, ok := p.Result()
	require.True(t, ok)
	assert.Equal(t, ports.Region{Width: max, Height: max}, res.Region)

	q := newPicker(t, 1, max, max)
	q.MouseDown(0, 0)
	q.MouseUp(0, 0)
	res, ok = q.Result()
	require.True(t, ok)
	assert.Equal(t, core.PickMonitor, res.Kind)
	assert.Equal(t, ports.Region{Width: max, Height: max}, res.Region)
}

func TestPickerChangedFlags(t *testing.T) {
	p := newPicker(t, 1, 100, 100)
	assert.True(t, p.MouseMove(10, 10))
	assert.False(t, p.MouseMove(10, 10), "same position")
	assert.False(t, p.MouseUp(10, 10), "no press")
	assert.True(t, p.MouseDown(10, 10))
	assert.False(t, p.MouseDown(20, 20), "already pressed")
	assert.False(t, p.SelectMonitor(), "mode cannot change mid-press")
	assert.False(t, p.Confirm(), "no confirm mid-press")
}

func TestPickerAllocations(t *testing.T) {
	p := newPicker(t, 1, 1920, 1080)
	p.SetWorkspace(1, ports.Region{Width: 100, Height: 100})
	n := testing.AllocsPerRun(10, func() {
		p.MouseDown(10, 10)
		for i := 0; i < 1000; i++ {
			p.MouseMove(float64(10+i%900), float64(10+(i*7)%900))
			_, _ = p.Rect()
			_, _ = p.Result()
		}
		p.SelectMonitor()
		p.SelectRegion()
		p.Cancel()
		_ = p.Begin(1, 1920, 1080)
		p.SetWorkspace(1, ports.Region{Width: 100, Height: 100})
	})
	assert.Zero(t, n)
}

func BenchmarkPickerDrag(b *testing.B) {
	p := &core.Picker{}
	_ = p.Begin(1, 3840, 2160)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.MouseDown(100, 100)
		for j := 0; j < 100; j++ {
			p.MouseMove(float64(100+j*30), float64(100+j*15))
		}
		p.MouseUp(3000, 1500)
		_ = p.Begin(1, 3840, 2160)
	}
}
