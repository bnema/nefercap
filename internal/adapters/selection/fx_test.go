package selection

import (
	"context"
	"testing"
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
	// A target change rebuilds the footer legend once (covered by the footer
	// tests); confirming the same target only replays the effects.
	m := newTestModel(t)
	m.input(key("w")) // falls back to the monitor
	now := time.Unix(1000, 0)
	n := testing.AllocsPerRun(100, func() {
		m.input(key("m"))
		m.input(key("w"))
		now = now.Add(fxStep)
		m.animate(now)
		_ = m.scene()
	})
	assert.Zero(t, n, "toasts, pulses and their frames allocate nothing")
	n = testing.AllocsPerRun(100, func() {
		m.input(key("r"))
		m.input(key("m"))
	})
	t.Logf("target switch allocs/run: %v", n)
	// Measured on main before the effects: 3 (two legends and a size label).
	assert.LessOrEqual(t, n, 3.0, "effects add nothing to a target switch")
}

func TestAnimatorWakesOnlyWhileEffectsRun(t *testing.T) {
	a := newAnimator()
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // a failed assertion must not leak the animator
	done := make(chan struct{})
	go func() { a.run(ctx, wake); close(done) }()

	select {
	case <-wake:
		t.Fatal("idle animator woke the overlay")
	case <-time.After(3 * fxStep):
	}
	a.extend(time.Now().Add(2 * fxStep))
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("running effect did not wake the overlay")
	}
	time.Sleep(5 * fxStep)
	for len(wake) > 0 {
		<-wake
	}
	select {
	case <-wake:
		t.Fatal("ended effect kept waking the overlay")
	case <-time.After(3 * fxStep):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("animator did not stop with its context")
	}
}

func TestNilAnimatorIsSafe(t *testing.T) {
	var a *animator
	a.extend(time.Now())
}

func TestChipStatesFollowModeAndTarget(t *testing.T) {
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
