package selection

import (
	"strings"
	"testing"

	"github.com/bnema/nefercap/internal/core"
	"github.com/bnema/nefercap/internal/ports"
	"github.com/stretchr/testify/assert"
)

func TestContextualFooter(t *testing.T) {
	m := newTestModel(t)
	assert.Contains(t, m.footerText, "R region · M monitor")
	assert.Contains(t, m.footerText, "G grid · Esc cancel")
	assert.NotContains(t, m.footerText, "W workspace")
	assert.Contains(t, m.footerText, "drag + release capture region")
	m.input(key("m"))
	assert.Contains(t, m.footerText, "Enter capture monitor")
	m.input(key("r"))
	assert.Contains(t, m.footerText, "drag + release capture region")

	m, _ = newBareModel()
	m.setWorkspaces([]ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Width: 640, Height: 480}}})
	m.resize(640, 480, 1)
	assert.Contains(t, m.footerText, "W workspace")
	m.input(key("w"))
	assert.Equal(t, core.PickWorkspace, m.picker.Kind())
	assert.Contains(t, m.footerText, "Enter capture workspace")
	m.resize(600, 400, 1)
	assert.Contains(t, m.footerText, "drag + release capture region", "resize resets the mode")
}

func TestHintsBoundsAndMetrics(t *testing.T) {
	for _, size := range [][2]int{{1920, 1080}, {640, 480}, {480, 320}, {200, 100}, {20, 20}} {
		m := newTestModel(t)
		m.resize(size[0], size[1], 1)
		for _, r := range []frect{m.scene().header, m.scene().footer} {
			assert.GreaterOrEqual(t, r.x, 0.0)
			assert.GreaterOrEqual(t, r.y, 0.0)
			assert.LessOrEqual(t, r.x+r.w, float64(size[0]))
			assert.LessOrEqual(t, r.y+r.h, float64(size[1]))
		}
	}
	assert.Contains(t, styleSheet, "font-size: 1rem")
	assert.Contains(t, styleSheet, "justify-content: center")
	assert.Contains(t, styleSheet, "align-items: center")
	assert.Equal(t, 2, len(strings.Split(newTestModel(t).footerText, "\n")))
	assert.GreaterOrEqual(t, footerH-2*tagPadY, 2*baseFont*1.37)
}

func TestNarrowWorkspaceFooterHoldsFourLines(t *testing.T) {
	m, _ := newBareModel()
	m.setWorkspaces([]ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Y: 60, Width: 480, Height: 260}}})
	m.resize(480, 320, 1)
	m.input(key("w"))
	s := m.scene()
	// Each explicit hint row wraps once at 480px: four 24px lines plus
	// two 4px vertical padding edges. Do not derive this from the helper.
	assert.GreaterOrEqual(t, s.footer.h, 104.0)
	assert.LessOrEqual(t, s.footer.y+s.footer.h, 320.0)
	if !raceEnabled {
		assert.Zero(t, testing.AllocsPerRun(100, func() { _ = m.scene() }))
	}
}

func TestWorkspaceLabelAvoidsOutputHeader(t *testing.T) {
	m, _ := newBareModel()
	m.setWorkspaces([]ports.Workspace{{ID: 11, OutputID: 7, Active: true, Region: ports.Region{Y: 60, Width: 640, Height: 360}}})
	m.resize(640, 480, 1)
	m.input(key("w"))
	s := m.scene()
	assert.Equal(t, "640 × 360", m.label.text)
	assert.GreaterOrEqual(t, s.label.y, s.header.y+s.header.h)
	assert.GreaterOrEqual(t, s.label.y, 60.0, "label falls inside the workspace")
	assert.LessOrEqual(t, s.label.y+s.label.h, 480.0)
}

func TestFooterUnchangedAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	m := newTestModel(t)
	m.input(key("m"))
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		m.input(key("m"))
		m.input(motion(10, 10))
		_ = m.scene()
	}))
}
