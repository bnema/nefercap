package selection

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

func TestTargetKeysShowToastAndPulse(t *testing.T) {
	m := newTestModel(t)
	for _, c := range []struct {
		key  string
		kind core.PickKind
		text string
	}{{"m", core.PickMonitor, toastMonitor}, {"r", core.PickRegion, toastRegion}, {"m", core.PickMonitor, toastMonitor}} {
		assert.True(t, m.input(key(c.key)), c.key)
		assert.Equal(t, c.kind, m.picker.Kind())
		assert.Equal(t, c.text, m.toast.text)
		assert.False(t, m.toast.warn)
		assert.True(t, m.pulse.on)
	}
}

func TestWorkspaceKeyUsesActiveWorkspace(t *testing.T) {
	m, _ := newBareModel()
	m.setWorkspaces([]ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Y: 30, Width: 1920, Height: 1050}}})
	m.resize(1920, 1080, 1)
	m.input(key("w"))
	assert.Equal(t, core.PickWorkspace, m.picker.Kind())
	assert.Equal(t, toastWorkspace, m.toast.text)
	assert.False(t, m.toast.warn)
}

func TestWorkspaceKeyWithoutWorkspaceFallsBackToMonitor(t *testing.T) {
	m := newTestModel(t)
	assert.True(t, m.input(key("w")))
	assert.Equal(t, core.PickMonitor, m.picker.Kind())
	assert.Equal(t, toastNoWS, m.toast.text)
	assert.True(t, m.toast.warn)
	m.input(key("Return"))
	require.Equal(t, endAccepted, m.end)
	res, ok, err := m.sess.outcome(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, core.PickMonitor, res.Kind)
	assert.Equal(t, uint32(7), res.OutputID)
}

func TestTargetKeysIgnoredWhileDragging(t *testing.T) {
	m := newTestModel(t)
	m.input(button(100, 100, true))
	m.input(motion(300, 300))
	require.True(t, m.picker.Dragging())
	for _, k := range []string{"m", "w", "r"} {
		m.input(key(k))
		assert.True(t, m.picker.Dragging())
		assert.False(t, m.toast.on, k)
	}
}

func TestTabShowsModeToastOnEveryOverlay(t *testing.T) {
	sess, _ := testSession()
	a := newModel(ports.Screenshot, testOutputs, 0, sess, false)
	b := newModel(ports.Screenshot, testOutputs, 1, sess, false)
	a.toggle, b.toggle = true, true
	a.resize(1920, 1080, 1)
	b.resize(1280, 720, 1)
	a.input(key("Tab"))
	assert.Equal(t, toastRec, a.toast.text)
	b.syncMode() // what b's view does when woken
	assert.Equal(t, toastRec, b.toast.text)
	assert.True(t, b.pulse.on)
	a.input(key("Tab"))
	assert.Equal(t, toastShot, a.toast.text)
}

func TestToastHoldsFadesAndEnds(t *testing.T) {
	m := newTestModel(t)
	m.input(key("m"))
	t0 := time.Unix(1000, 0)
	m.animate(t0)
	assert.Equal(t, 0, m.toast.step)
	assert.Greater(t, m.scene().toast.w, 0.0)
	m.animate(t0.Add(targetHold))
	assert.Equal(t, 0, m.toast.step, "held at full opacity")
	m.animate(t0.Add(targetHold + fxStep))
	assert.Equal(t, 2, m.toast.step)
	m.animate(t0.Add(targetHold + toastFade - time.Millisecond))
	assert.Equal(t, len(opacityClasses)-1, m.toast.step)
	m.animate(t0.Add(targetHold + toastFade))
	assert.False(t, m.toast.on)
	assert.Equal(t, frect{}, m.scene().toast)
}

func TestPulseThickensThenRestoresTheBorder(t *testing.T) {
	m := newTestModel(t)
	m.input(key("m"))
	t0 := time.Unix(1000, 0)
	m.animate(t0)
	assert.Equal(t, edge+pulseExtra, m.edgeW)
	assert.Equal(t, frect{0, 0, 1920, edge + pulseExtra}, m.scene().edges[0])
	m.animate(t0.Add(pulseSpan / 2))
	assert.Greater(t, m.edgeW, edge)
	assert.Less(t, m.edgeW, edge+pulseExtra)
	m.animate(t0.Add(pulseSpan))
	assert.Equal(t, edge, m.edgeW)
	assert.Equal(t, frect{0, 0, 1920, edge}, m.scene().edges[0])
}

func TestPulseNeverExceedsATinyRegion(t *testing.T) {
	m := newTestModel(t)
	m.input(button(100, 100, true))
	m.input(motion(103, 103))
	m.pulseBorder()
	m.animate(time.Unix(1000, 0))
	for _, e := range m.scene().edges {
		assert.LessOrEqual(t, e.w, 3.0)
		assert.LessOrEqual(t, e.h, 3.0)
	}
}

func TestChipsShowModeAndTarget(t *testing.T) {
	m := toggleModel(t, ports.Screenshot, true)
	s := m.scene()
	assert.Greater(t, s.modes[0].w, 0.0, "SHOT chip")
	assert.Greater(t, s.modes[1].w, 0.0, "REC chip offered with Tab")
	for i := range s.kinds {
		assert.Greater(t, s.kinds[i].w, 0.0, targetTexts[i])
	}
	assert.Equal(t, s.badge.y, s.target.y, "one row on a wide output")
	assert.False(t, overlaps(s.badge, s.target))
	assert.False(t, overlaps(s.target, s.header))

	fixed := toggleModel(t, ports.Record, false)
	s = fixed.scene()
	assert.Zero(t, s.modes[0].w, "without Tab only the current mode shows")
	assert.Greater(t, s.modes[1].w, 0.0)

	assert.Equal(t, "shot", modeClass(ports.Screenshot))
	assert.Equal(t, "rec", modeClass(ports.Record))
	assert.Contains(t, styleSheet, "box.chip.shot")
	assert.Contains(t, styleSheet, "box.chip.rec")
	assert.Contains(t, styleSheet, "box.edge.rec")
}

func TestChipsStayOnSurfaceAndClearOfHeader(t *testing.T) {
	for _, w := range []int{3840, 1920, 1024, 640, 320, 200, 100, 40} {
		m := toggleModel(t, ports.Screenshot, true)
		m.resize(w, 480, 1)
		s := m.scene()
		for _, r := range append(s.modes[:], s.kinds[:]...) {
			if r.w == 0 {
				continue
			}
			assert.GreaterOrEqual(t, r.x, 0.0)
			assert.LessOrEqual(t, r.x+r.w, float64(w)+1e-9, "width %d", w)
			assert.LessOrEqual(t, r.y+r.h, 480.0)
		}
		if w >= 640 {
			assert.False(t, overlaps(s.target, s.header), "target/header at %d", w)
		}
	}
}

func TestEffectsAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	// Each run triggers both effects once, then draws them through the hold,
	// the fade, the pulse decay and the end, like the frames of one key press.
	m := newTestModel(t)
	m.input(key("w")) // falls back to the monitor: later presses keep the legend
	steps := [...]time.Duration{0, fxStep, pulseSpan / 2, pulseSpan, warnHold, warnHold + 4*fxStep, warnHold + toastFade}
	now := time.Unix(1000, 0)
	n := testing.AllocsPerRun(100, func() {
		m.input(key("w"))
		start := now
		for _, d := range steps {
			m.animate(start.Add(d))
			_ = m.scene()
		}
		now = now.Add(time.Second)
	})
	assert.Zero(t, n, "toasts, pulses and their frames allocate nothing")

	// A full HUD fade out and back in, with a toast fading along, allocates
	// nothing per frame. The fade restarts every run, so warm-up does not
	// leave it settled.
	m.input(button(100, 100, true))
	m.input(motion(400, 300))
	n = testing.AllocsPerRun(100, func() {
		m.hud = hudFade{}
		m.showToast(toastMonitor, targetHold, false)
		for i := range hudSteps + 1 {
			m.animate(now.Add(time.Duration(i) * fxStep))
			_ = m.scene()
			_, _ = m.hudClass(), m.toastClass()
		}
		m.hud.hiding = false // as after a rejected release
		for i := range hudSteps + 1 {
			m.animate(now.Add(time.Second + time.Duration(i)*fxStep))
			_ = m.scene()
			_, _ = m.hudClass(), m.toastClass()
		}
		m.hud.hiding = true
		now = now.Add(2 * time.Second)
	})
	assert.Zero(t, n, "HUD fade frames allocate nothing")
	m.input(motion(102, 102))
	m.input(button(102, 102, false))
	m.animate(now.Add(time.Minute))
	m.animate(now.Add(2 * time.Minute))
	assert.False(t, m.toast.on)
	assert.False(t, m.pulse.on)

	n = testing.AllocsPerRun(100, func() {
		m.input(key("r"))
		m.input(key("m"))
	})
	t.Logf("target switch allocs/run: %v", n)
	// Measured on main before the effects: 3 (two legends and a size label).
	assert.LessOrEqual(t, n, 3.0, "effects add nothing to a target switch")
}

func TestAnimatorWakesOnlyWhileEffectsRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newAnimator()
		wake := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { a.run(ctx, wake); close(done) }()

		time.Sleep(10 * fxStep)
		synctest.Wait()
		assert.Empty(t, wake, "an idle animator never wakes the overlay")

		a.extend(time.Now().Add(2 * fxStep))
		time.Sleep(fxStep)
		synctest.Wait()
		require.Len(t, wake, 1, "a running effect wakes the overlay")
		<-wake

		time.Sleep(5 * fxStep) // past the end and its final frame
		synctest.Wait()
		for len(wake) > 0 {
			<-wake
		}
		time.Sleep(10 * fxStep)
		synctest.Wait()
		assert.Empty(t, wake, "an ended effect stops waking the overlay")

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("animator did not stop with its context")
		}
	})
}

func TestNilAnimatorIsSafe(t *testing.T) {
	var a *animator
	a.extend(time.Now())
}

func TestChipStatesFollowModeAndTarget(t *testing.T) {
	assert.Equal(t, "SHOT", badgeText(ports.Screenshot))
	assert.Equal(t, "REC", badgeText(ports.Record))
	m := toggleModel(t, ports.Screenshot, true)
	s := m.scene()
	assert.Equal(t, [2]string{"shot", "idle"}, s.modeState)
	assert.Equal(t, [3]string{"shot", "idle", "off"}, s.kindState, "region active, no workspace here")
	m.input(key("m"))
	m.input(key("Tab"))
	s = m.scene()
	assert.Equal(t, [2]string{"idle", "rec"}, s.modeState)
	assert.Equal(t, [3]string{"idle", "rec", "off"}, s.kindState)
}

func TestDragFadesTheHUDOutAndBackIn(t *testing.T) {
	m := toggleModel(t, ports.Screenshot, true)
	m.grid = true
	m.layoutGrid()
	t0 := time.Unix(1000, 0)
	m.input(button(100, 100, true))
	m.animate(t0)
	assert.Equal(t, 0, m.hud.level, "a press alone keeps the HUD")

	m.input(motion(400, 300))
	require.True(t, m.picker.Dragging())
	m.input(key("Tab")) // its toast fades along with the HUD
	levels := []int{}
	for i := range hudSteps + 1 {
		m.animate(t0.Add(time.Duration(i) * fxStep))
		levels = append(levels, m.hud.level)
	}
	assert.Equal(t, []int{1, 2, 3, 4, 4}, levels, "one step per frame, then hidden")
	s := m.scene()
	assert.True(t, m.hudHidden())
	assert.Equal(t, frect{}, s.header)
	assert.Equal(t, frect{}, s.footer)
	assert.Equal(t, frect{}, s.toast)
	assert.Equal(t, [2]frect{}, s.modes)
	assert.Equal(t, [3]frect{}, s.kinds)
	assert.Greater(t, s.edges[0].w, 0.0, "the border stays")
	assert.Greater(t, s.dim[0].w, 0.0, "the dimming stays")
	assert.Greater(t, s.label.w, 0.0, "the size stays")
	assert.Greater(t, s.grid[0].h, 0.0, "the guides stay")

	// Releasing a too-small drag keeps picking: the HUD fades back in.
	m.input(motion(103, 103))
	m.input(button(103, 103, false))
	require.Equal(t, endNone, m.end)
	t1 := t0.Add(time.Second)
	levels = levels[:0]
	for i := range hudSteps + 1 {
		m.animate(t1.Add(time.Duration(i) * fxStep))
		levels = append(levels, m.hud.level)
	}
	assert.Equal(t, []int{3, 2, 1, 0, 0}, levels)
	assert.Greater(t, m.scene().header.w, 0.0)
	assert.Equal(t, "o0", m.hudClass())
}

func TestShortDragReversesTheFadeWithoutAJump(t *testing.T) {
	m := newTestModel(t)
	t0 := time.Unix(1000, 0)
	m.input(button(100, 100, true))
	m.input(motion(400, 300))
	m.animate(t0)
	m.animate(t0.Add(fxStep))
	require.Equal(t, 2, m.hud.level)
	m.input(motion(103, 103))
	m.input(button(103, 103, false))
	m.animate(t0.Add(2 * fxStep))
	assert.Equal(t, 1, m.hud.level, "fades back from where it was")
}

func TestFadingHUDKeepsTheDragLabelInPlace(t *testing.T) {
	m := toggleModel(t, ports.Screenshot, true)
	m.input(button(100, 50, true))
	m.input(motion(300, 250))
	m.animate(time.Unix(1000, 0)) // level 1: chips still drawn
	assert.Equal(t, frect{100, 50 - labelH - 4, labelW, labelH}, m.scene().label)
}

func TestToastFadesWithTheHUD(t *testing.T) {
	m := newTestModel(t)
	t0 := time.Unix(1000, 0)
	m.input(key("m"))
	m.animate(t0)
	require.Equal(t, "o0", m.toastClass())

	m.input(button(100, 100, true))
	m.input(motion(400, 300))
	classes := []string{}
	for i := range hudSteps - 1 {
		m.animate(t0.Add(time.Duration(i) * fxStep))
		classes = append(classes, m.toastClass())
		assert.Greater(t, m.scene().toast.w, 0.0, "drawn until the HUD is gone")
	}
	assert.Equal(t, []string{"o3", "o5", "o7"}, classes)
	m.animate(t0.Add((hudSteps - 1) * fxStep))
	assert.True(t, m.hudHidden())
	assert.Equal(t, frect{}, m.scene().toast)

	m.input(motion(103, 103))
	m.input(button(103, 103, false))
	t1 := t0.Add(5 * fxStep)
	m.animate(t1)
	assert.Equal(t, "o7", m.toastClass(), "comes back faint, not at once")
	m.animate(t1.Add(fxStep))
	assert.Equal(t, "o5", m.toastClass())
}
