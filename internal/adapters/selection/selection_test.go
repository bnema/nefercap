package selection

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"strings"
	"testing"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

var testOutputs = []ports.Output{
	{ID: 7, Name: "DP-1", Width: 1920, Height: 1080, Scale: 1},
	{ID: 9, Name: "HDMI-A-1", Width: 1280, Height: 720, Scale: 1},
}

// testSession is a real session whose cancel is observable.
func testSession() (*session, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	return newSession(cancel), ctx
}

func newTestModel(t *testing.T) *model {
	t.Helper()
	sess, _ := testSession()
	m := newModel(ports.Screenshot, testOutputs, 0, sess, false)
	m.resize(1920, 1080, 1)
	require.Equal(t, core.PickActive, m.picker.Status())
	return m
}

// newBareModel has no size yet, like an overlay before its first configure.
func newBareModel() (*model, context.Context) {
	sess, ctx := testSession()
	return newModel(ports.Screenshot, testOutputs, 0, sess, false), ctx
}

func motion(x, y float64) nefergui.Input {
	return nefergui.Input{Kind: nefergui.InputPointerMotion, X: x, Y: y}
}

func button(x, y float64, pressed bool) nefergui.Input {
	k := nefergui.InputPointerRelease
	if pressed {
		k = nefergui.InputPointerPress
	}
	return nefergui.Input{Kind: k, X: x, Y: y, Button: btnLeft, Pressed: pressed}
}

func key(name string) nefergui.Input {
	return nefergui.Input{Kind: nefergui.InputKey, Keysym: keysym(name), Pressed: true}
}

// keysym is the xkb keysym of a key name used by the tests; an unknown name
// is a keysym the selector ignores.
func keysym(name string) uint32 {
	switch name {
	case "Escape":
		return 0xff1b
	case "Tab":
		return 0xff09
	case "Return":
		return 0xff0d
	case "KP_Enter":
		return 0xff8d
	}
	if len(name) == 1 {
		return uint32(name[0])
	}
	return 0xffffff
}

func TestKeyNameRoundTrip(t *testing.T) {
	for _, name := range []string{"Escape", "Tab", "Return", "KP_Enter", "1", "9", "a", "z", "A", "Z", "m", "W"} {
		assert.Equal(t, name, keyName(keysym(name)), name)
	}
	for _, sym := range []uint32{0, '0', ' ', ':', '@', '[', '`', '{', 0xffffff, 0xff08} {
		assert.Empty(t, keyName(sym), "%#x", sym)
	}
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = keyName('m') }))
}

func TestDragReleaseAcceptsRegion(t *testing.T) {
	m := newTestModel(t)
	m.input(button(100, 50, true))
	m.input(motion(300, 250))
	assert.Equal(t, "200 × 200", m.label.text)
	assert.Equal(t, endNone, m.end)
	m.input(button(300, 250, false))
	require.Equal(t, endAccepted, m.end)
	res, ok, err := m.sess.outcome(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, core.PickRegion, res.Kind)
	assert.Equal(t, ports.Region{X: 100, Y: 50, Width: 200, Height: 200}, res.Region)
	assert.Equal(t, uint32(7), res.OutputID)
}

func TestClickAcceptsMonitor(t *testing.T) {
	m := newTestModel(t)
	m.input(button(500, 500, true))
	m.input(button(500, 500, false))
	require.Equal(t, endAccepted, m.end)
	res, _, _ := m.sess.outcome(context.Background())
	assert.Equal(t, core.PickMonitor, res.Kind)
	sel := New(ports.Screenshot, ports.VideoSettings{}, nil, false).selection(res)
	assert.Equal(t, ports.Target{OutputID: 7}, sel.Target)
}

func TestMonitorKeyThenEnter(t *testing.T) {
	m := newTestModel(t)
	m.input(key("m"))
	assert.Equal(t, "1920 × 1080", m.label.text)
	assert.Equal(t, endNone, m.end)
	m.input(key("Return"))
	require.Equal(t, endAccepted, m.end)
	res, _, _ := m.sess.outcome(context.Background())
	assert.Equal(t, core.PickMonitor, res.Kind)
}

func TestEscapeAbortsEveryOverlay(t *testing.T) {
	sess, ctx := testSession()
	m := newModel(ports.Screenshot, testOutputs, 1, sess, false)
	m.resize(1280, 720, 1)
	m.input(key("Escape"))
	assert.Equal(t, endCanceled, m.end)
	assert.Error(t, ctx.Err(), "the shared context ends every overlay")
	_, ok, err := sess.outcome(context.Background())
	assert.False(t, ok)
	assert.NoError(t, err)
}

func TestDigitPicksMonitorFromAnyOverlay(t *testing.T) {
	m := newTestModel(t) // overlay on output 1 (ID 7)
	m.input(key("3"))    // no such output
	assert.Equal(t, endNone, m.end)
	m.input(key("2"))
	require.Equal(t, endAccepted, m.end)
	res, ok, err := m.sess.outcome(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, core.PickResult{OutputID: 9, Kind: core.PickMonitor}, res)
	sel := New(ports.Screenshot, ports.VideoSettings{}, nil, false).selection(res)
	assert.Equal(t, ports.Target{OutputID: 9}, sel.Target)

	own := newTestModel(t)
	own.input(key("1"))
	res, _, _ = own.sess.outcome(context.Background())
	assert.Equal(t, uint32(7), res.OutputID, "a digit may also pick the overlay's own monitor")
}

func TestOutputKeyIgnoredWhileDragging(t *testing.T) {
	m := newTestModel(t)
	m.input(button(10, 10, true))
	m.input(motion(100, 100))
	m.input(key("2"))
	assert.Equal(t, endNone, m.end)
}

func TestOtherButtonsAndReleasedKeysIgnored(t *testing.T) {
	m := newTestModel(t)
	ev := button(10, 10, true)
	ev.Button = 0x111
	m.input(ev)
	m.input(motion(200, 200))
	assert.False(t, m.picker.Dragging())
	up := key("Escape")
	up.Pressed = false
	m.input(up)
	rep := key("Escape")
	rep.Repeat = true
	m.input(rep)
	assert.Equal(t, endNone, m.end)
}

func TestWorkspaceSelectionTarget(t *testing.T) {
	ws := []ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Y: 30, Width: 1920, Height: 1050}}}
	m, _ := newBareModel()
	m.setWorkspaces(ws)
	m.resize(1920, 1080, 1)
	m.input(key("w"))
	m.input(key("g"))
	require.Equal(t, core.PickWorkspace, m.picker.Kind())

	m.input(key("Return"))
	require.Equal(t, endAccepted, m.end)
	res, ok, err := m.sess.outcome(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	sel := New(ports.Screenshot, ports.VideoSettings{}, ws, false).selection(res)
	assert.Equal(t, ports.Target{OutputID: 7, WorkspaceID: 11}, sel.Target, "a workspace has a stable ID and a zero Region")
}

func TestResizeRestartsOnSizeChangeOnly(t *testing.T) {
	m := newTestModel(t)
	m.input(button(10, 10, true))
	m.input(motion(200, 200))
	m.resize(1920, 1080, 2) // scale only
	assert.True(t, m.picker.Dragging())
	m.resize(1280, 720, 2)
	assert.False(t, m.picker.Dragging())
	w, h := m.picker.Bounds()
	assert.Equal(t, [2]int{1280, 720}, [2]int{w, h})
}

func TestResizeInvalidSizeFails(t *testing.T) {
	m, ctx := newBareModel()
	m.resize(0, 0, 1)
	assert.Equal(t, endCanceled, m.end)
	assert.Error(t, ctx.Err())
	_, ok, err := m.sess.outcome(context.Background())
	assert.False(t, ok)
	assert.ErrorIs(t, err, core.ErrInvalidBounds)
	assert.ErrorContains(t, err, "output DP-1", "the failing output is named")
	assert.ErrorContains(t, err, "resize to 0x0", "the rejected size is reported")
}

func TestWorkspaceOnlyWhenRealAndOnThisOutput(t *testing.T) {
	ws := []ports.Workspace{
		{ID: 0, OutputID: 7, Active: true, Region: ports.Region{Width: 100, Height: 100}}, // no identity
		{ID: 5, OutputID: 9, Active: true, Region: ports.Region{Width: 100, Height: 100}}, // other output
		{ID: 6, OutputID: 7, Region: ports.Region{Width: 100, Height: 100}},               // not displayed
		{ID: 11, OutputID: 7, Active: true, Region: ports.Region{X: 0, Y: 30, Width: 1920, Height: 1050}},
	}
	none, _ := newBareModel()
	none.resize(1920, 1080, 1)
	none.input(key("w"))
	none.input(key("Return"))
	assert.Equal(t, endNone, none.end, "W without workspace metadata must do nothing")

	m, _ := newBareModel()
	m.setWorkspaces(ws)
	m.resize(1920, 1080, 1)
	m.input(key("w"))
	m.input(key("Return"))
	require.Equal(t, endAccepted, m.end)
	res, _, _ := m.sess.outcome(context.Background())
	assert.Equal(t, core.PickWorkspace, res.Kind)
	assert.Equal(t, uint64(11), res.WorkspaceID)
	sel := New(ports.Screenshot, ports.VideoSettings{}, ws, false).selection(res)
	assert.Equal(t, ports.Target{OutputID: 7, WorkspaceID: 11}, sel.Target, "workspace geometry is live from the compositor, not a cached crop")
}

func TestOnlyTheActiveWorkspaceIsOffered(t *testing.T) {
	m, _ := newBareModel()
	m.setWorkspaces([]ports.Workspace{
		{ID: 1, OutputID: 7, Region: ports.Region{Width: 960, Height: 1080}},
		{ID: 2, OutputID: 7, Region: ports.Region{X: 960, Width: 960, Height: 1080}, Active: true},
		{ID: 3, OutputID: 7, Region: ports.Region{Width: 960, Height: 1080}},
	})
	m.resize(1920, 1080, 1)
	id, _, ok := m.picker.Workspace()
	require.True(t, ok)
	assert.Equal(t, uint64(2), id)

	hidden, _ := newBareModel()
	hidden.setWorkspaces([]ports.Workspace{{ID: 1, OutputID: 7, Region: ports.Region{Width: 960, Height: 1080}}})
	hidden.resize(1920, 1080, 1)
	_, _, ok = hidden.picker.Workspace()
	assert.False(t, ok, "a workspace that is not displayed is never offered")
	hidden.input(key("w"))
	hidden.input(key("Return"))
	assert.Equal(t, endNone, hidden.end)
}

func TestSelectionConversionUsesCoreTarget(t *testing.T) {
	s := New(ports.Screenshot, ports.VideoSettings{}, nil, false)
	region := ports.Region{X: 10, Y: 20, Width: 300, Height: 200}
	assert.Equal(t, ports.Target{OutputID: 7, Region: region},
		s.selection(core.PickResult{OutputID: 7, Kind: core.PickRegion, Region: region}).Target, "a dragged region keeps its geometry")
	assert.Equal(t, ports.Target{OutputID: 7},
		s.selection(core.PickResult{OutputID: 7, Kind: core.PickMonitor, Region: ports.Region{Width: 1920, Height: 1080}}).Target)
	assert.Equal(t, ports.Target{OutputID: 7, WorkspaceID: 11},
		s.selection(core.PickResult{OutputID: 7, Kind: core.PickWorkspace, Region: region, WorkspaceID: 11}).Target)
}

func TestNewSelectionDefaults(t *testing.T) {
	sel := New(ports.Record, ports.VideoSettings{}, nil, false).selection(core.PickResult{OutputID: 7, Kind: core.PickMonitor})
	assert.Equal(t, ports.Record, sel.Mode)
	assert.Equal(t, DefaultFPS, sel.Video.FPS)
	assert.Empty(t, sel.Path)

	custom := ports.VideoSettings{FPS: 60, Width: 640, Height: 360}
	assert.Equal(t, custom, New(ports.Record, custom, nil, false).selection(core.PickResult{}).Video)
	assert.Equal(t, ports.VideoSettings{}, New(ports.Screenshot, custom, nil, false).selection(core.PickResult{}).Video)
}

func TestSelectRejectsTooManyOutputsWithoutOpeningAnything(t *testing.T) {
	outs := make([]ports.Output, MaxOutputs+1)
	for i := range outs {
		outs[i] = ports.Output{ID: uint32(i + 1), Name: "O"}
	}
	_, ok, err := New(ports.Screenshot, ports.VideoSettings{}, nil, false).Select(context.Background(), outs)
	assert.False(t, ok)
	assert.ErrorIs(t, err, ErrTooManyOutputs)
}

func TestSelectRejectsBadInput(t *testing.T) {
	s := New(ports.Screenshot, ports.VideoSettings{}, nil, false)
	_, ok, err := s.Select(context.Background(), nil)
	assert.False(t, ok)
	assert.ErrorIs(t, err, ports.ErrOutputNotFound)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, err = s.Select(ctx, testOutputs)
	assert.False(t, ok)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSessionFirstDecisionWins(t *testing.T) {
	sess, ctx := testSession()
	first := core.PickResult{OutputID: 7, Kind: core.PickMonitor}
	sess.accept(first, ports.Screenshot)
	sess.accept(core.PickResult{OutputID: 9, Kind: core.PickMonitor}, ports.Record)
	sess.abort()
	assert.Error(t, ctx.Err())
	res, ok, err := sess.outcome(context.Background())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, first, res)
	assert.Equal(t, ports.Screenshot, sess.acceptedMode())
}

// The recorded mode is the one the confirming overlay showed: a Tab on
// another overlay after the decision changes nothing.
func TestLateTabKeepsConfirmedMode(t *testing.T) {
	sess, _ := testSession()
	a := newModel(ports.Screenshot, testOutputs, 0, sess, false)
	b := newModel(ports.Screenshot, testOutputs, 1, sess, false)
	a.toggle, b.toggle = true, true
	a.wake, b.wake = make(chan struct{}, 1), make(chan struct{}, 1)
	sess.wakes = []chan struct{}{a.wake, b.wake}
	a.resize(1920, 1080, 1)
	b.resize(1280, 720, 1)
	b.input(key("Tab"))
	<-a.wake
	<-b.wake
	require.Equal(t, ports.Record, b.mode)
	b.input(key("m"))
	b.input(key("Return"))
	res, ok, err := sess.outcome(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	a.input(key("Tab")) // arrives before a sees the cancel
	assert.Empty(t, a.wake, "no redraw after the decision")
	assert.Equal(t, ports.Record, sess.acceptedMode())
	sel := New(ports.Screenshot, ports.VideoSettings{}, nil, false).AllowToggle().selectionFor(sess.acceptedMode(), res)
	assert.Equal(t, ports.Record, sel.Mode)
}

func TestSessionRunEnded(t *testing.T) {
	boom := errors.New("boom")

	sess, _ := testSession()
	sess.accept(core.PickResult{OutputID: 7}, ports.Screenshot)
	sess.runEnded(context.Background(), "DP-1", context.Canceled) // our own cancel
	_, ok, err := sess.outcome(context.Background())
	assert.True(t, ok)
	assert.NoError(t, err)

	sess, _ = testSession()
	sess.runEnded(context.Background(), "DP-1", errors.Join(context.Canceled, boom))
	_, ok, err = sess.outcome(context.Background())
	assert.False(t, ok)
	assert.ErrorIs(t, err, boom)

	// GO012: a real failure joined with Canceled survives whole, including the
	// output name, after our own cancel.
	sess, _ = testSession()
	sess.accept(core.PickResult{OutputID: 7}, ports.Screenshot)
	real := fmt.Errorf("surface close: %w", context.Canceled)
	sess.runEnded(context.Background(), "DP-2", errors.Join(context.Canceled, real, boom))
	_, ok, err = sess.outcome(context.Background())
	assert.False(t, ok, "a real teardown failure rejects even an accepted pick")
	require.ErrorIs(t, err, boom)
	assert.ErrorContains(t, err, "output DP-2")
	assert.ErrorContains(t, err, "surface close", "a wrapped Canceled is a real failure, not hidden")

	sess, _ = testSession()
	sess.runEnded(context.Background(), "DP-1", nil) // closed without any decision
	_, _, err = sess.outcome(context.Background())
	assert.ErrorIs(t, err, errOverlayClosed)
	assert.ErrorContains(t, err, "output DP-1")

	sess, _ = testSession()
	sess.accept(core.PickResult{OutputID: 7}, ports.Screenshot)
	sess.runEnded(context.Background(), "DP-1", boom) // a later overlay failing rejects the pick
	_, ok, err = sess.outcome(context.Background())
	assert.False(t, ok || err == nil)

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	sess, _ = testSession()
	sess.runEnded(parent, "DP-1", context.Canceled)
	_, ok, err = sess.outcome(parent)
	assert.False(t, ok)
	assert.ErrorIs(t, err, context.Canceled)
	sess, _ = testSession()
	sess.runEnded(parent, "DP-1", boom)
	_, _, err = sess.outcome(parent)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, boom)
}

func TestLayerConfig(t *testing.T) {
	c := layerConfig("DP-1", neferclient.KeyboardExclusive)
	assert.Equal(t, "DP-1", c.Output)
	assert.Equal(t, neferclient.LayerOverlay, c.Level)
	assert.Equal(t, neferclient.AnchorTop|neferclient.AnchorBottom|neferclient.AnchorLeft|neferclient.AnchorRight, c.Anchors)
	assert.Equal(t, neferclient.KeyboardExclusive, c.Keyboard)
	assert.Equal(t, int32(-1), c.ExclusiveZone)
	assert.Nil(t, c.InputRects, "whole surface takes input")
	assert.Equal(t, neferclient.KeyboardOnDemand, layerConfig("DP-2", neferclient.KeyboardOnDemand).Keyboard)
}

func TestSceneGeometry(t *testing.T) {
	m := newTestModel(t)
	s := m.scene()
	assert.Equal(t, frect{0, 0, 1920, 1080}, s.dim[0], "idle: whole output dimmed")
	assert.Equal(t, frect{}, s.edges[0])
	assert.Equal(t, frect{}, s.label)

	m.input(button(100, 50, true))
	m.input(motion(300, 250))
	s = m.scene()
	assert.Equal(t, [4]frect{
		{0, 0, 1920, 50}, {0, 250, 1920, 830}, {0, 50, 100, 200}, {300, 50, 1620, 200},
	}, s.dim)
	assert.Equal(t, frect{100, 50, 200, 1}, s.edges[0])
	assert.Equal(t, frect{100, 249, 200, 1}, s.edges[1])
	assert.Equal(t, frect{100, 50, 1, 200}, s.edges[2])
	assert.Equal(t, frect{299, 50, 1, 200}, s.edges[3])
	assert.Equal(t, frect{100, 50 - labelH - 4, labelW, labelH}, s.label)
	// Dim strips and region tile the output exactly.
	area := 200.0 * 200.0
	for _, d := range s.dim {
		area += d.w * d.h
	}
	assert.Equal(t, 1920.0*1080.0, area)
}

func TestSceneMonitorOutlineAndGrid(t *testing.T) {
	m := newTestModel(t)
	m.input(key("m"))
	m.input(key("g"))
	s := m.scene()
	assert.Equal(t, [4]frect{}, s.dim, "monitor choice is not dimmed")
	assert.Equal(t, frect{0, 0, 1920, 1}, s.edges[0])
	assert.Equal(t, frect{640, 0, 1, 1080}, s.grid[0])
	assert.Equal(t, frect{0, 720, 1920, 1}, s.grid[5])
}

// physicalEdges converts a logical rectangle the way the renderer does:
// each edge is rounded to a physical pixel independently.
func physicalEdges(r frect, scale float64) image.Rectangle {
	return image.Rect(
		int(math.Round(r.x*scale)), int(math.Round(r.y*scale)),
		int(math.Round((r.x+r.w)*scale)), int(math.Round((r.y+r.h)*scale)))
}

func gridModel(t *testing.T, grid bool, w, h int, scale float64) *model {
	t.Helper()
	sess, _ := testSession()
	m := newModel(ports.Screenshot, testOutputs, 0, sess, grid)
	m.resize(w, h, scale)
	require.Equal(t, core.PickActive, m.picker.Status())
	return m
}

func TestGridDefaultAndToggle(t *testing.T) {
	on := gridModel(t, true, 1920, 1080, 1)
	assert.NotEqual(t, [core.GridLineCount]frect{}, on.scene().grid, "configured on: drawn at once")
	assert.True(t, on.input(key("g")))
	assert.Equal(t, [core.GridLineCount]frect{}, on.scene().grid, "G turns it off")
	assert.True(t, on.input(key("G")))
	assert.NotEqual(t, [core.GridLineCount]frect{}, on.scene().grid, "and on again")

	off := gridModel(t, false, 1920, 1080, 1)
	assert.Equal(t, [core.GridLineCount]frect{}, off.scene().grid, "configured off: nothing drawn")
	assert.True(t, off.input(key("g")))
	assert.NotEqual(t, [core.GridLineCount]frect{}, off.scene().grid)
}

func TestGridRendersExactPhysicalPixels(t *testing.T) {
	for _, c := range []struct {
		w, h  int
		scale float64
	}{
		{1920, 1080, 1}, {1600, 900, 1.2}, {1536, 864, 1.25}, {1280, 720, 1.5}, {960, 540, 2},
		{1366, 768, 1}, {1367, 769, 1.25}, {1001, 601, 1.2}, {1279, 719, 1.5}, {853, 481, 2.25},
	} {
		m := gridModel(t, true, c.w, c.h, c.scale)
		want, err := core.MonitorGrid(c.w, c.h, c.scale)
		require.NoError(t, err)
		for i, r := range m.scene().grid {
			got := physicalEdges(r, c.scale)
			assert.Equal(t, want[i], got, "%dx%d@%g line %d", c.w, c.h, c.scale, i)
			if i < 3 {
				assert.Equal(t, 1, got.Dx(), "%dx%d@%g vertical %d is one physical pixel", c.w, c.h, c.scale, i)
			} else {
				assert.Equal(t, 1, got.Dy(), "%dx%d@%g horizontal %d is one physical pixel", c.w, c.h, c.scale, i)
			}
		}
	}
}

func TestGridSweepRendersOnePhysicalPixel(t *testing.T) {
	// Every compositor fractional scale (k/120) over many logical sizes.
	for k := 120; k <= 360; k++ {
		scale := float64(k) / 120
		for w := 640; w <= 3000; w += 37 {
			h := w*9/16 + 1
			m := gridModel(t, true, w, h, scale)
			want, err := core.MonitorGrid(w, h, scale)
			require.NoError(t, err)
			for i, r := range m.scene().grid {
				if got := physicalEdges(r, scale); got != want[i] {
					t.Fatalf("%dx%d@%d/120 line %d renders %v, want %v", w, h, k, i, got, want[i])
				}
			}
		}
	}
}

func TestGridIsPerOutputScale(t *testing.T) {
	sess, _ := testSession()
	a := newModel(ports.Screenshot, testOutputs, 0, sess, true)
	b := newModel(ports.Screenshot, testOutputs, 1, sess, true)
	a.resize(1920, 1080, 1)
	b.resize(1280, 720, 1.5)
	assert.Equal(t, frect{640, 0, 1, 1080}, a.scene().grid[0])
	assert.Equal(t, frect{640 / 1.5, 0, 1 / 1.5, 1080 / 1.5}, b.scene().grid[0], "physical 640 of 1920 on the scaled output")
	assert.Equal(t, image.Rect(640, 0, 641, 1080), physicalEdges(b.scene().grid[0], 1.5))
}

func TestGridStaysOnWholeMonitorWhileDragging(t *testing.T) {
	m := gridModel(t, true, 1920, 1080, 1)
	idle := m.scene().grid
	m.input(button(100, 50, true))
	m.input(motion(300, 250))
	require.True(t, m.picker.Dragging())
	assert.Equal(t, idle, m.scene().grid, "the drag does not move the grid")
	m.input(motion(1000, 700))
	assert.Equal(t, idle, m.scene().grid)
	m.input(key("m"))
	m.input(key("r"))
	assert.Equal(t, idle, m.scene().grid, "nor does a kind change")
	m.input(button(1000, 700, false))
	assert.Equal(t, core.PickAccepted, m.picker.Status())
}

func TestGridToggleSurvivesResizeAndScaleChange(t *testing.T) {
	m := gridModel(t, true, 1920, 1080, 1)
	m.input(key("g"))
	require.False(t, m.grid)
	m.resize(1280, 720, 1.5) // size change resets the picker, not the toggle
	assert.False(t, m.grid)
	assert.Equal(t, [core.GridLineCount]frect{}, m.scene().grid)
	m.input(key("g"))
	m.resize(1280, 720, 2) // scale only
	assert.True(t, m.grid)
	m.resize(1600, 900, 1.2)
	assert.True(t, m.grid)
}

func TestGridScaleOnlyChangeKeepsPickAndRelays(t *testing.T) {
	m := gridModel(t, true, 1920, 1080, 1)
	m.input(button(10, 10, true))
	m.input(motion(200, 200))
	before := m.scene().grid
	m.resize(1920, 1080, 1.25)
	assert.True(t, m.picker.Dragging(), "a scale-only change keeps the drag")
	after := m.scene().grid
	assert.NotEqual(t, before, after, "the grid follows the new scale")
	want, err := core.MonitorGrid(1920, 1080, 1.25)
	require.NoError(t, err)
	for i, r := range after {
		assert.Equal(t, want[i], physicalEdges(r, 1.25), "line %d", i)
	}
}

func TestGridBadScaleDrawsNothingAndKeepsSelecting(t *testing.T) {
	for _, scale := range []float64{0, -1, math.NaN(), math.Inf(1), 100} {
		m := gridModel(t, true, 1920, 1080, scale)
		assert.Equal(t, endNone, m.end, "scale %v", scale)
		assert.Equal(t, [core.GridLineCount]frect{}, m.scene().grid, "scale %v", scale)
	}
}

func TestGridAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	m := gridModel(t, true, 1600, 900, 1.2)
	var s scene
	n := testing.AllocsPerRun(200, func() {
		s = m.scene()
		m.input(key("g"))
		m.input(key("g"))
		m.resize(1600, 900, 1.2)
	})
	assert.Zero(t, n, "grid toggle, relayout and scene do not allocate")
	_ = s
}

func BenchmarkGridResizeScene(b *testing.B) {
	sess, _ := testSession()
	m := newModel(ports.Screenshot, testOutputs, 0, sess, true)
	m.resize(1600, 900, 1.2)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		m.resize(1600, 900, 1.2+float64(i%2)*0.05)
		_ = m.scene()
	}
}

func TestLabelPlacementStaysOnSurface(t *testing.T) {
	// Above the region, with a 4px gap.
	assert.Equal(t, frect{1920 - labelW, 1010 - labelH - 4, labelW, labelH}, labelRect(frect{1800, 1010, 100, 50}, 1920, 1080))
	// No room above: just inside the region's top-left corner.
	assert.Equal(t, frect{4, 4, labelW, labelH}, labelRect(frect{0, 0, 500, 500}, 1920, 1080))
	// Exactly enough room above: the label touches the gap, not the edge.
	assert.Equal(t, frect{10, 0, labelW, labelH}, labelRect(frect{10, labelH + 4, 300, 100}, 1920, 1080))
	// One pixel short falls back to inside the region.
	assert.Equal(t, frect{14, labelH + 3 + 4, labelW, labelH}, labelRect(frect{10, labelH + 3, 300, 100}, 1920, 1080))
	// Right edge: shifted left to stay on the surface.
	assert.Equal(t, frect{1920 - labelW, 100, labelW, labelH}, labelRect(frect{1900, 100 + labelH + 4, 20, 20}, 1920, 1080))
	// Inside fallback near the bottom is clamped onto the surface.
	assert.Equal(t, frect{4, 40 - labelH, labelW, labelH}, labelRect(frect{0, 20, 500, 20}, 1920, 40))
	// A surface smaller than the label pins it to the origin.
	assert.Equal(t, frect{0, 0, labelW, labelH}, labelRect(frect{5, 5, 10, 10}, labelW-1, labelH-1))
}

func TestLabelStaysOnSurfaceWhenItFits(t *testing.T) {
	const w, h = 300.0, 100.0
	for y := 0.0; y <= h; y += 7 {
		for x := 0.0; x <= w; x += 13 {
			l := labelRect(frect{x, y, w - x, h - y}, w, h)
			assert.GreaterOrEqual(t, l.x, 0.0)
			assert.GreaterOrEqual(t, l.y, 0.0)
			assert.LessOrEqual(t, l.x+l.w, w, "region %v,%v", x, y)
			assert.LessOrEqual(t, l.y+l.h, h, "region %v,%v", x, y)
		}
	}
}

// Noto Sans Mono's normal line is approximately 1.37em tall.
// Keep the sizing assumptions explicit rather than partially parsing CSS.
func TestLabelContentHoldsMonospaceLine(t *testing.T) {
	const line, padY = baseFont * 1.37, tagPadY
	require.True(t, strings.Contains(styleSheet, "box-sizing: border-box; padding: 0.25em 0.625em; overflow: hidden;"), "update label sizing when tag box sizing changes")
	content := labelH - 2*padY
	assert.GreaterOrEqual(t, content, line, "label clips its text line")
	assert.GreaterOrEqual(t, content-line+padY, 1.0, "less than 1px below the text line")
}

func TestFooterAndHeaderText(t *testing.T) {
	assert.Contains(t, footerText(ports.Screenshot, core.PickRegion, false, false, false), "shot · R region")
	assert.Contains(t, footerText(ports.Record, core.PickRegion, false, false, false), "rec · R region")
	assert.NotContains(t, footerText(ports.Record, core.PickRegion, false, false, false), "No recording indicator")
	assert.Contains(t, footerText(ports.Record, core.PickRegion, false, false, true), "No recording indicator on this compositor")
	assert.NotContains(t, footerText(ports.Screenshot, core.PickRegion, false, false, true), "No recording indicator", "only record mode needs the hint")
	assert.Equal(t, "DP-1 · 1/2 · keys 1-2 pick monitor", headerText(testOutputs, 0))
	assert.Equal(t, "DP-1", headerText(testOutputs[:1], 0))
	i, ok := outputKey("2", 2)
	assert.True(t, ok)
	assert.Equal(t, 1, i)
	_, ok = outputKey("0", 2)
	assert.False(t, ok)
	_, ok = outputKey("12", 2)
	assert.False(t, ok)
}

func TestSelectorAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	m := newTestModel(t)
	m.input(button(100, 50, true))
	same := []nefergui.Input{motion(300, 250), motion(300, 250), motion(300, 250)}
	for _, ev := range same { // warm the label buffer
		m.input(ev)
	}
	var s scene
	n := testing.AllocsPerRun(200, func() {
		for _, ev := range same {
			m.input(ev)
		}
		s = m.scene()
	})
	assert.Equal(t, 0.0, n, "unchanged pointer position and scene geometry allocate nothing")

	// A changed size builds exactly one label string and nothing else.
	i := 0
	n = testing.AllocsPerRun(200, func() {
		i++
		m.input(motion(float64(300+i%2*7), 250))
		s = m.scene()
	})
	assert.LessOrEqual(t, n, 1.0, "a changed size allocates only its label text")
	_ = s
}

func BenchmarkMotion(b *testing.B) {
	m, _ := newBareModel()
	m.resize(1920, 1080, 1)
	m.input(button(100, 50, true))
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		m.input(motion(float64(200+i%300), 250))
		_ = m.scene()
	}
}

func TestIdleSceneEmitsNoEmptyChip(t *testing.T) {
	m := newTestModel(t)
	s := m.scene()
	assert.Equal(t, frect{}, s.label, "no label box without a size")
	assert.Zero(t, s.label.w)
	assert.Equal(t, frect{}, s.edges[0])
	assert.Greater(t, s.header.w, 0.0)
	assert.Greater(t, s.footer.w, 0.0)
}

func TestInputReportsOnlyVisibleChanges(t *testing.T) {
	m := newTestModel(t)
	assert.False(t, m.input(motion(50, 50)), "hover draws nothing")
	assert.False(t, m.input(nefergui.Input{Kind: nefergui.InputPointerAxis, DX: 1, DY: 2}), "axis is ignored")
	other := button(50, 50, true)
	other.Button = 0x111
	assert.False(t, m.input(other), "other buttons are ignored")
	assert.False(t, m.input(nefergui.Input{Kind: nefergui.InputFocusIn}))
	assert.False(t, m.input(nefergui.Input{Kind: nefergui.InputPointerLeave}))
	release := key("g")
	release.Pressed = false
	assert.False(t, m.input(release), "release")
	repeat := key("g")
	repeat.Repeat = true
	assert.False(t, m.input(repeat), "repeat")
	assert.False(t, m.input(key("q")), "unknown key")
	assert.False(t, m.input(key("w")), "W without a workspace")

	assert.False(t, m.input(button(100, 50, true)), "a press alone changes nothing visible")
	assert.False(t, m.input(motion(102, 51)), "below the drag threshold")
	assert.True(t, m.input(motion(300, 250)), "the drag appears")
	assert.False(t, m.input(motion(300, 250)), "same position")
	assert.False(t, m.input(motion(300, 250)))
	assert.True(t, m.input(motion(300, 251)), "the rectangle grew")

	m = newTestModel(t)
	assert.True(t, m.input(key("g")), "grid toggles")
	assert.True(t, m.input(key("m")), "monitor outline")
	assert.False(t, m.input(key("m")), "already chosen")
	assert.True(t, m.input(key("r")))
	assert.False(t, m.input(key("r")))
}

func TestInputClampedEdgeMotionIsNotVisible(t *testing.T) {
	m := newTestModel(t)
	m.input(button(100, 100, true))
	require.True(t, m.input(motion(1920, 200)))
	assert.False(t, m.input(motion(2500, 200)), "the clamped rectangle did not change")
	assert.False(t, m.input(motion(3000, 200)))
}

func TestInputReportsClosing(t *testing.T) {
	m := newTestModel(t)
	assert.True(t, m.input(key("Escape")), "canceling closes the overlay")
	assert.False(t, m.input(key("g")), "a closed overlay ignores input")

	m = newTestModel(t)
	assert.True(t, m.input(key("2")), "a digit accepts")

	m = newTestModel(t)
	m.input(button(10, 10, true))
	assert.True(t, m.input(button(10, 10, false)), "a click accepts the monitor")

	m = newTestModel(t)
	m.input(button(10, 10, true))
	m.input(motion(200, 200))
	assert.True(t, m.input(button(200, 200, false)), "a drag release accepts the region")
}

func TestCapabilitiesHideWorkspacesAndHintIndicator(t *testing.T) {
	ws := []ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Width: 640, Height: 480}}}
	s := New(ports.Record, ports.VideoSettings{}, ws, false).WithCapabilities(ports.Capabilities{})
	assert.Empty(t, s.workspaces, "no ext-workspace: W is not offered")
	assert.True(t, s.noIndicator)
	s = New(ports.Record, ports.VideoSettings{}, ws, false).WithCapabilities(ports.Capabilities{Workspaces: true, Exclusion: true})
	assert.Len(t, s.workspaces, 1)
	assert.False(t, s.noIndicator)
}
