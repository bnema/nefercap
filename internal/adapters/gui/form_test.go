package gui

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

var testOutputs = []ports.Output{
	{ID: 7, Name: "DP-1", Width: 2560, Height: 1440, Scale: 1},
	{ID: 9, Name: "", Width: 1920, Height: 1080, Scale: 2},
	{ID: 11},
}

func newTestModel(t testing.TB) *model {
	t.Helper()
	m, err := newModel(testOutputs, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	require.NoError(t, err)
	return m
}

func TestParseScreenshotDefaults(t *testing.T) {
	m := newTestModel(t)
	require.NoError(t, m.parse())
	assert.Equal(t, ports.Selection{
		Mode:   ports.Screenshot,
		Target: ports.Target{OutputID: 7},
		Path:   "nefercap-20260102-030405.png",
	}, m.sel)
}

func TestParseRecordDefaults(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	require.NoError(t, m.parse())
	assert.Equal(t, ports.Selection{
		Mode:     ports.Record,
		Target:   ports.Target{OutputID: 7},
		Path:     "nefercap-20260102-030405.png",
		Video:    ports.VideoSettings{FPS: 30},
		Duration: 10 * time.Second,
	}, m.sel)
}

func TestParseRecordFull(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	m.output = m.outputs[1].key
	m.path = "  out.mp4 "
	m.region = " 10, 20, 640x480 "
	m.fps = "60"
	m.size = "1280X720"
	m.duration = "1m30s"
	require.NoError(t, m.parse())
	assert.Equal(t, ports.Selection{
		Mode:     ports.Record,
		Target:   ports.Target{OutputID: 9, Region: ports.Region{X: 10, Y: 20, Width: 640, Height: 480}},
		Path:     "out.mp4",
		Video:    ports.VideoSettings{FPS: 60, Width: 1280, Height: 720},
		Duration: 90 * time.Second,
	}, m.sel)
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name string
		edit func(*model)
		want error
	}{
		{"unknown output", func(m *model) { m.output = "nope" }, errOutput},
		{"empty path", func(m *model) { m.path = "  " }, errPath},
		{"directory path", func(m *model) { m.path = "dir/" }, errPath},
		{"bad mode", func(m *model) { m.mode = "" }, errMode},
		{"region fields", func(m *model) { m.region = "1,2" }, errRegion},
		{"region no comma", func(m *model) { m.region = "1x2" }, errRegion},
		{"region zero width", func(m *model) { m.region = "0,0,0x5" }, errRegion},
		{"region zero height", func(m *model) { m.region = "0,0,5x0" }, errRegion},
		{"region negative", func(m *model) { m.region = "-1,0,5x5" }, errRegion},
		{"region junk", func(m *model) { m.region = "a,b,cxd" }, errRegion},
		{"region huge", func(m *model) { m.region = "0,0,99999999999999999999x5" }, errRegion},
		{"region far edge x", func(m *model) { m.region = "16000,0,385x5" }, errRegion},
		{"region far edge y", func(m *model) { m.region = "0,16000,5x385" }, errRegion},
		{"region empty size", func(m *model) { m.region = "0,0," }, errRegion},
		{"fps empty", func(m *model) { m.mode = modeRecord; m.fps = "" }, errFPS},
		{"fps zero", func(m *model) { m.mode = modeRecord; m.fps = "0" }, errFPS},
		{"fps high", func(m *model) { m.mode = modeRecord; m.fps = "121" }, errFPS},
		{"fps fraction", func(m *model) { m.mode = modeRecord; m.fps = "29.97" }, errFPS},
		{"fps sign", func(m *model) { m.mode = modeRecord; m.fps = "+30" }, errFPS},
		{"size one dim", func(m *model) { m.mode = modeRecord; m.size = "1280" }, errSize},
		{"size zero", func(m *model) { m.mode = modeRecord; m.size = "0x720" }, errSize},
		{"size missing height", func(m *model) { m.mode = modeRecord; m.size = "1280x" }, errSize},
		{"size odd width", func(m *model) { m.mode = modeRecord; m.size = "1281x720" }, errSizeOdd},
		{"size odd height", func(m *model) { m.mode = modeRecord; m.size = "1280x721" }, errSizeOdd},
		{"size over frame bytes", func(m *model) { m.mode = modeRecord; m.size = "16384x16384" }, errSizeSize},
		{"duration junk", func(m *model) { m.mode = modeRecord; m.duration = "soon" }, errDuration},
		{"duration negative", func(m *model) { m.mode = modeRecord; m.duration = "-5s" }, errDuration},
		{"duration too long", func(m *model) { m.mode = modeRecord; m.duration = "25h" }, errDuration},
		{"duration huge seconds", func(m *model) { m.mode = modeRecord; m.duration = "999999999999999999999" }, errDuration},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t)
			tt.edit(m)
			assert.ErrorIs(t, m.parse(), tt.want)
		})
	}
}

func TestFPSBoundIsPortsMax(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	m.fps = strconv.Itoa(ports.MaxFPS)
	require.NoError(t, m.parse())
	assert.Equal(t, ports.MaxFPS, m.sel.Video.FPS)
	m.fps = strconv.Itoa(ports.MaxFPS + 1)
	assert.ErrorIs(t, m.parse(), errFPS)
	assert.Contains(t, errFPS.Error(), strconv.Itoa(ports.MaxFPS))
	assert.Equal(t, "1-"+strconv.Itoa(ports.MaxFPS), fpsPlaceholder)
}

func TestOddRegionAllowedEvenSizeChecked(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	m.region = "1,3,641x481"
	require.NoError(t, m.parse())
	assert.Equal(t, ports.Region{X: 1, Y: 3, Width: 641, Height: 481}, m.sel.Target.Region)
	m.mode = modeScreenshot
	m.size = "1281x721"
	require.NoError(t, m.parse(), "screenshot ignores video size")
}

func TestVideoSizeAtFrameLimit(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	m.size = "16384x4096"
	require.NoError(t, m.parse())
	m.size = "16384x4098"
	assert.ErrorIs(t, m.parse(), errSizeSize)
}

func TestRegionAtMaxDimension(t *testing.T) {
	m := newTestModel(t)
	m.region = "0,0,16384x16384"
	require.NoError(t, m.parse())
	m.region = "1,0,16384x1"
	assert.ErrorIs(t, m.parse(), errRegion)
}

func TestScreenshotIgnoresRecordFields(t *testing.T) {
	m := newTestModel(t)
	m.fps, m.size, m.duration = "bad", "bad", "bad"
	require.NoError(t, m.parse())
	assert.Equal(t, ports.VideoSettings{}, m.sel.Video)
	assert.Zero(t, m.sel.Duration)
}

func TestInvalidParseKeepsPreviousSelection(t *testing.T) {
	m := newTestModel(t)
	require.NoError(t, m.parse())
	want := m.sel
	m.path = ""
	require.Error(t, m.parse())
	assert.Equal(t, want, m.sel)
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 0, "0": 0, "5": 5 * time.Second, " 90 ": 90 * time.Second,
		"1.5s": 1500 * time.Millisecond, "2m": 2 * time.Minute, "24h": 24 * time.Hour, "86400": 24 * time.Hour,
	} {
		got, ok := parseDuration(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
}

func TestParseUintBounds(t *testing.T) {
	n, ok := parseUint("16384", ports.MaxDimension)
	assert.True(t, ok)
	assert.Equal(t, 16384, n)
	_, ok = parseUint("16385", ports.MaxDimension)
	assert.False(t, ok)
	_, ok = parseUint("", ports.MaxDimension)
	assert.False(t, ok)
}

// The status line runs every frame and must not allocate for valid or invalid
// forms: parsing uses only the standard library on the model's own strings.
func TestFormAllocations(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeRecord
	m.region = "10,20,640x480"
	m.size = "1280x720"
	m.duration = "1m30s"
	require.NoError(t, m.parse())
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = m.parse() }), "valid form")
	m.size = "1281x720"
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = m.parse() }), "odd size")
	m.size = "1280x720"
	assert.Zero(t, testing.AllocsPerRun(100, func() { _, _, _ = m.status() }), "status")
	m.fps = "abc"
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = m.parse() }), "invalid form")
	m.fps = "30"
	m.duration = "0"
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = m.parse() }), "seconds duration")
	m.duration = "soon"
	require.ErrorIs(t, m.parse(), errDuration)
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = m.parse() }), "invalid duration")
	assert.Zero(t, testing.AllocsPerRun(100, func() { _, _, _ = m.status() }), "invalid duration status")
	m.duration = "-5s"
	require.ErrorIs(t, m.parse(), errDuration)
	m.duration = "1m"
	require.NoError(t, m.parse(), "cache follows edits")
	assert.Equal(t, time.Minute, m.sel.Duration)
}

func BenchmarkFormParse(b *testing.B) {
	m := newTestModel(b)
	m.mode = modeRecord
	m.region = "10,20,640x480"
	m.size = "1280x720"
	m.duration = "1m30s"
	b.ReportAllocs()
	for b.Loop() {
		if err := m.parse(); err != nil {
			b.Fatal(err)
		}
	}
}
