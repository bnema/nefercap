package selection

import (
	"image"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

func TestBackdropImageIsOpaqueRGBA(t *testing.T) {
	// One XRGB8888 pixel is B, G, R, X in memory; the stride has padding.
	f := ports.Frame{Pixels: []byte{
		0x10, 0x20, 0x30, 0x00, 0x40, 0x50, 0x60, 0x00, 0xee, 0xee, 0xee, 0xee,
		0x70, 0x80, 0x90, 0x00, 0xa0, 0xb0, 0xc0, 0x00,
	}, Width: 2, Height: 2, Stride: 12, Format: ports.XRGB8888}
	require.NoError(t, f.Validate())
	img := backdropImage(f)
	assert.Equal(t, image.Rect(0, 0, 2, 2), img.Bounds())
	assert.Equal(t, []byte{0x30, 0x20, 0x10, 0xff, 0x60, 0x50, 0x40, 0xff, 0x90, 0x80, 0x70, 0xff, 0xc0, 0xb0, 0xa0, 0xff}, img.Pix)
}

func TestWithBackdrops(t *testing.T) {
	small := ports.Frame{Pixels: make([]byte, 4*4*4), Width: 4, Height: 4, Stride: 16, Format: ports.XRGB8888}
	s, err := New(ports.Screenshot, ports.VideoSettings{}, nil, false).WithBackdrops(map[uint32]ports.Frame{7: small})
	require.NoError(t, err)
	assert.Contains(t, s.backdrops, uint32(7))

	huge := ports.Frame{Width: maxBackdrop + 1, Height: 1, Stride: (maxBackdrop + 1) * 4, Format: ports.XRGB8888}
	_, err = New(ports.Screenshot, ports.VideoSettings{}, nil, false).WithBackdrops(map[uint32]ports.Frame{9: huge})
	assert.ErrorContains(t, err, "at most 4096", "the caller freezes only outputs CanBackdrop accepts")
}

func TestCanBackdrop(t *testing.T) {
	assert.True(t, CanBackdrop(ports.Output{Width: 3840, Height: 2160}))
	assert.True(t, CanBackdrop(ports.Output{Width: 2160, Height: 4096}))
	assert.False(t, CanBackdrop(ports.Output{Width: 5120, Height: 2880}))
	assert.False(t, CanBackdrop(ports.Output{}), "an unknown mode stays live")
}

// The frozen screen is drawn under a screenshot selection only: a recording
// selects on the live screen.
func TestBackdropDrawnOnlyForScreenshots(t *testing.T) {
	m := newTestModel(t)
	m.backdrop = image.NewNRGBA(image.Rect(0, 0, 1920, 1080))
	assert.True(t, drawsBackdrop(m))
	m.mode = ports.Record
	assert.False(t, drawsBackdrop(m))
	m.mode, m.backdrop = ports.Screenshot, nil
	assert.False(t, drawsBackdrop(m))
}
