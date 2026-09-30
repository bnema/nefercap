package indicator

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/nefergui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

func TestAppendElapsed(t *testing.T) {
	for secs, want := range map[int64]string{
		0: "00:00", 5: "00:05", 65: "01:05", 3599: "59:59", 3600: "1:00:00", 36061: "10:01:01",
	} {
		assert.Equal(t, want, string(appendElapsed(nil, secs)), "%d", secs)
	}
}

func TestRefreshFollowsTheReadingAndCachesSecond(t *testing.T) {
	start := time.Unix(100, 0)
	h := newHUD(start, make(chan struct{}, 1), "")
	assert.Equal(t, "● REC 00:00", h.refresh(start))
	assert.Equal(t, "● REC 00:00", h.refresh(start.Add(999*time.Millisecond)))
	assert.Equal(t, "● REC 01:01", h.refresh(start.Add(61*time.Second)))
	assert.Equal(t, "● REC 1:00:00", h.refresh(start.Add(time.Hour)))
	assert.Equal(t, "● REC 00:00", h.refresh(start.Add(-time.Second)), "a reading before start never shows negative time")
}

func TestTargetLabel(t *testing.T) {
	start := time.Unix(100, 0)
	at := start.Add(5 * time.Second)
	for label, want := range map[string]string{
		"":                  "● REC 00:05",
		"DP-1":              "● REC 00:05 · DP-1",
		"web":               "● REC 00:05 · web",
		"123456789012":      "● REC 00:05 · 123456789012",
		"1234567890123":     "● REC 00:05 · 12345678901…",
		"espace de travail": "● REC 00:05 · espace de t…",
		"a\nb\x00c":         "● REC 00:05 · abc",
		"\n\t":              "● REC 00:05",
	} {
		assert.Equal(t, want, newHUD(start, make(chan struct{}, 1), label).refresh(at), "%q", label)
	}
	// The longest line still fits the 228 px text area (about 29 cells).
	long := newHUD(start, make(chan struct{}, 1), "wwwwwwwwwwwwwwwwww").refresh(start.Add(time.Hour))
	assert.LessOrEqual(t, len([]rune(long)), 29)
}

func TestTargetLabelIsSingleLinePrintable(t *testing.T) {
	for name, label := range map[string]string{
		"line separator":      "a\u2028b",
		"paragraph separator": "a\u2029b",
		"bidi override":       "a\u202eb",
		"bidi isolate":        "a\u2066b\u2069",
		"bidi mark":           "a\u200fb\u061c",
		"nbsp":                "a\u00a0b",
		"zero width":          "a\u200bb",
		"private use":         "a\ue000b",
		"next line":           "a\u0085b",
		"unassigned":          "a\U0010ffffb",
	} {
		got := suffixFor(label)
		assert.Equal(t, " · ab", got, name)
	}
	assert.Equal(t, " · café 東京", suffixFor("café 東京"), "printable non-ASCII stays")
	assert.Equal(t, "", suffixFor("\u202e\u2028"))
}

func TestRefreshAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds allocations")
	}
	start := time.Unix(100, 0)
	h := newHUD(start, make(chan struct{}, 1), "workspace-one")
	at := start.Add(7 * time.Second)
	h.refresh(at)
	assert.Equal(t, 0.0, testing.AllocsPerRun(100, func() { h.refresh(at) }))
}

func TestRequestStopNeverBlocks(t *testing.T) {
	stop := make(chan struct{}, 1)
	h := newHUD(time.Now(), stop, "")
	h.requestStop()
	h.requestStop() // channel full: the second request is dropped, not blocked
	assert.Len(t, stop, 1)
}

func TestLayerConfigTakesNoFocusAndOnlyStopInput(t *testing.T) {
	c := layerConfig("DP-1")
	assert.Equal(t, nefergui.KeyboardNone, c.Keyboard)
	assert.Equal(t, nefergui.LayerOverlay, c.Level)
	assert.Equal(t, nefergui.AnchorTop, c.Anchors)
	assert.Equal(t, int32(-1), c.ExclusiveZone)
	require.Len(t, c.InputRects, 1)
	r := c.InputRects[0]
	assert.Equal(t, nefergui.Rect{X: Width - stopWidth, Width: stopWidth, Height: Height}, r)
	assert.LessOrEqual(t, r.X+r.Width, int32(Width))
}

func TestHUDReadabilityAndStopGeometry(t *testing.T) {
	assert.Equal(t, 384, Width)
	assert.Equal(t, 40, Height)
	assert.Equal(t, 90, stopWidth)
	assert.Contains(t, styleSheet, "font-size: 1rem")
	assert.Contains(t, styleSheet, "width: 5.625rem")
	assert.Contains(t, styleSheet, "line-height: 2.5rem")
	assert.Contains(t, styleSheet, "padding: 0; line-height:")
	assert.Equal(t, nefergui.Rect{X: 294, Y: 0, Width: 90, Height: 40}, stopRect())
}

func BenchmarkRefreshUnchanged(b *testing.B) {
	start := time.Unix(100, 0)
	h := newHUD(start, make(chan struct{}, 1), "workspace")
	h.refresh(start)
	b.ReportAllocs()
	for b.Loop() {
		h.refresh(start)
	}
}

func TestRunRejectsBadSetup(t *testing.T) {
	stop := make(chan struct{}, 1)
	ok := func(context.Context, nefergui.WaylandSurface) error { return nil }
	out := ports.Output{ID: 1, Name: "DP-1"}

	assert.ErrorIs(t, New(nil, stop).Run(context.Background(), out), ErrNoAuthorize)
	assert.ErrorIs(t, New(ok, nil).Run(context.Background(), out), ErrNoStop)
	assert.NotErrorIs(t, New(ok, nil).Run(context.Background(), out), ErrNoAuthorize)
	assert.ErrorIs(t, New(ok, stop).Run(context.Background(), ports.Output{ID: 1}), ports.ErrOutputNotFound)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, New(ok, stop).Run(ctx, out), context.Canceled)
}
