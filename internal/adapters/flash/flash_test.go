package flash

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bnema/neferclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

func TestLayerConfigWholeOutput(t *testing.T) {
	c := layerConfig("DP-1", ports.Region{})
	assert.Equal(t, "DP-1", c.Output)
	assert.Equal(t, neferclient.AnchorTop|neferclient.AnchorBottom|neferclient.AnchorLeft|neferclient.AnchorRight, c.Anchors)
	assert.Equal(t, [4]int32{}, c.Margin)
	assert.Zero(t, c.Width)
	assert.Zero(t, c.Height)
	assert.Equal(t, neferclient.KeyboardNone, c.Keyboard)
	assert.NotNil(t, c.InputRects, "an empty non-nil input region lets clicks through")
	assert.Empty(t, c.InputRects)
}

func TestLayerConfigRegion(t *testing.T) {
	c := layerConfig("DP-1", ports.Region{X: 10, Y: 20, Width: 300, Height: 200})
	assert.Equal(t, neferclient.AnchorTop|neferclient.AnchorLeft, c.Anchors)
	assert.Equal(t, [4]int32{20, 0, 0, 10}, c.Margin)
	assert.Equal(t, int32(300), c.Width)
	assert.Equal(t, int32(200), c.Height)
	assert.NotNil(t, c.InputRects)
	assert.Empty(t, c.InputRects)
}

func TestFadeSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fade{done: cancel}
	start := time.Unix(100, 0)
	assert.Equal(t, "s0", f.class(start), "the fade starts with the first frame")
	assert.Equal(t, "s0", f.class(start.Add(step-time.Millisecond)))
	assert.Equal(t, "s1", f.class(start.Add(step)))
	assert.Equal(t, "s9", f.class(start.Add(9*step)))
	assert.NoError(t, ctx.Err())
	assert.False(t, f.ended)
	assert.Equal(t, "s9", f.class(start.Add(10*step)), "past the end the faintest step stays")
	assert.Equal(t, "s9", f.class(start.Add(time.Hour)))
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "the fade ends the run")
	assert.True(t, f.ended)
	assert.Equal(t, "s0", (&fade{start: start, done: cancel}).class(start.Add(-time.Second)), "a reading before start never underflows")
}

func TestShowRequiresOutputName(t *testing.T) {
	err := Show(context.Background(), ports.Output{ID: 3}, ports.Region{})
	require.ErrorIs(t, err, ports.ErrOutputNotFound)
}

// TestSmokeShow shows the real flash. It needs a Wayland compositor with
// layer-shell and Vulkan, so it runs only when NEFERCAP_FLASH_OUTPUT names the
// wl_output to show it on (for example HEADLESS-1 on a NeferWL headless
// compositor).
func TestSmokeShow(t *testing.T) {
	name := os.Getenv("NEFERCAP_FLASH_OUTPUT")
	if name == "" {
		t.Skip("set NEFERCAP_FLASH_OUTPUT to the output name on a Wayland session")
	}
	began := time.Now()
	require.NoError(t, Show(context.Background(), ports.Output{ID: 1, Name: name}, ports.Region{X: 40, Y: 40, Width: 320, Height: 200}))
	assert.Less(t, time.Since(began), limit+time.Second)
}
