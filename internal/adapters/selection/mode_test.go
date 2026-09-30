package selection

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
)

func toggleModel(t *testing.T, mode ports.Mode, toggle bool) *model {
	t.Helper()
	m := newTestModel(t)
	m.base, m.mode, m.toggle = mode, mode, toggle
	m.footerMode = mode
	m.footerText = footerText(mode, core.PickRegion, false, toggle)
	return m
}

func TestTabTogglesModeKeepingTarget(t *testing.T) {
	m := toggleModel(t, ports.Screenshot, true)
	assert.Equal(t, "shot · Tab rec · R region · M monitor · G grid · Esc cancel\ndrag + release capture region · click capture monitor", m.footerText)
	assert.Equal(t, "SHOT", badgeText(m.mode))
	m.input(key("m"))
	require.Equal(t, core.PickMonitor, m.picker.Kind())
	assert.True(t, m.input(key("Tab")), "a mode change redraws")
	assert.Equal(t, ports.Record, m.mode)
	assert.Equal(t, core.PickMonitor, m.picker.Kind(), "target is kept")
	assert.Equal(t, "rec · Tab shot · R region · M monitor · G grid · Esc cancel\nEnter record monitor · click record monitor", m.footerText)
	assert.Equal(t, "REC", badgeText(m.mode))
	m.input(key("Tab"))
	assert.Equal(t, ports.Screenshot, m.mode)
	assert.Contains(t, m.footerText, "Enter capture monitor")
}

func TestTabIgnoredWhenModeFixed(t *testing.T) {
	for _, mode := range []ports.Mode{ports.Screenshot, ports.Record} {
		m := toggleModel(t, mode, false)
		before := m.footerText
		assert.False(t, m.input(key("Tab")))
		assert.Equal(t, mode, m.mode)
		assert.Equal(t, before, m.footerText)
		assert.NotContains(t, m.footerText, "Tab")
	}
}

func TestTabSharedAcrossOverlaysAndResult(t *testing.T) {
	sess, _ := testSession()
	a := newModel(ports.Screenshot, testOutputs, 0, sess, false)
	b := newModel(ports.Screenshot, testOutputs, 1, sess, false)
	a.toggle, b.toggle = true, true
	a.wake, b.wake = make(chan struct{}, 1), make(chan struct{}, 1)
	sess.wakes = []chan struct{}{a.wake, b.wake}
	a.resize(1920, 1080, 1)
	b.resize(1280, 720, 1)
	a.input(key("Tab"))
	require.Len(t, b.wake, 1, "the other overlay is woken")
	b.input(motion(1, 1))
	assert.Equal(t, ports.Record, b.mode)
	assert.Contains(t, b.footerText, "rec · Tab shot")
	s := New(ports.Screenshot, ports.VideoSettings{}, nil, false).AllowToggle()
	sel := s.selectionFor(modeAfter(s.mode, sess.toggles.Load()), core.PickResult{OutputID: 7, Kind: core.PickMonitor})
	assert.Equal(t, ports.Record, sel.Mode)
	assert.Equal(t, DefaultFPS, sel.Video.FPS)
	sel = s.selectionFor(ports.Screenshot, core.PickResult{OutputID: 7, Kind: core.PickMonitor})
	assert.Equal(t, ports.VideoSettings{}, sel.Video)
}

func TestBadgeClassAndText(t *testing.T) {
	assert.Equal(t, "SHOT", badgeText(ports.Screenshot))
	assert.Equal(t, "REC", badgeText(ports.Record))
	assert.NotContains(t, styleSheet, "box.rec", "the rec badge keeps the neutral tag style")
}

func TestBadgeTopLeftAndClearOfHeaderAndLabel(t *testing.T) {
	for _, w := range []int{1920, 640, 320, 200, 100, 40} {
		for _, mode := range []ports.Mode{ports.Screenshot, ports.Record} {
			m := toggleModel(t, mode, true)
			m.resize(w, 480, 1)
			s := m.scene()
			assert.GreaterOrEqual(t, s.badge.x, 0.0)
			assert.LessOrEqual(t, s.badge.x+s.badge.w, float64(w))
			assert.LessOrEqual(t, s.badge.y+s.badge.h, 480.0)
			if w >= 640 {
				assert.Equal(t, margin, s.badge.x)
				assert.Equal(t, margin, s.badge.y)
			}
			assert.False(t, overlaps(s.badge, s.header), "badge/header at %d", w)
			m.input(key("m"))
			s = m.scene()
			assert.False(t, overlaps(s.badge, s.header), "badge/header at %d", w)
			assert.False(t, overlaps(s.label, s.badge), "label/badge at %d", w)
		}
	}
}

func TestBadgeAvoidsWorkspaceLabelAtLeftEdge(t *testing.T) {
	m, _ := newBareModel()
	m.setWorkspaces([]ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Y: 0, Width: 300, Height: 400}}})
	m.resize(640, 480, 1)
	m.input(key("w"))
	s := m.scene()
	assert.False(t, overlaps(s.label, s.badge))
	assert.False(t, overlaps(s.label, s.header))
}

func TestModeToggleAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	m := toggleModel(t, ports.Screenshot, true)
	m.input(key("m"))
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		m.input(motion(10, 10))
		_ = m.scene()
		_ = badgeText(m.mode)
	}), "unchanged mode allocates nothing")
	n := testing.AllocsPerRun(100, func() {
		m.input(key("Tab"))
		_ = m.scene()
	})
	t.Logf("Tab toggle allocs/run: %v", n)
	assert.LessOrEqual(t, n, 1.0, "a toggle allocates only the rebuilt legend text")
}
