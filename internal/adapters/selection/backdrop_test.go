package selection

import (
	"context"
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

func frozenModel(t *testing.T, index int, sess *session) *model {
	t.Helper()
	m := newModel(ports.Screenshot, testOutputs, index, sess, false)
	m.backdrop = image.NewNRGBA(image.Rect(0, 0, 1, 1))
	m.wake = make(chan struct{}, 1)
	m.resize(1920, 1080, 1)
	return m
}

// F switches every overlay between the frozen and the live screen, the
// footer says what F does next, and the accepted pick records the state.
func TestFreezeKeyTogglesEveryOverlay(t *testing.T) {
	sess, _ := testSession()
	sess.canFreeze = true
	a, b := frozenModel(t, 0, sess), frozenModel(t, 1, sess)
	sess.wakes = []chan struct{}{a.wake, b.wake}
	require.True(t, drawsBackdrop(a))
	assert.Contains(t, a.footerText, "G grid · F live · Esc cancel")

	assert.True(t, a.input(key("f")), "F redraws")
	assert.False(t, drawsBackdrop(a))
	assert.Equal(t, toastLive, a.toast.text)
	assert.Contains(t, a.footerText, "F freeze")
	require.Len(t, b.wake, 1, "the other overlay is woken")
	b.input(motion(1, 1))
	assert.False(t, drawsBackdrop(b), "the other overlay follows")

	b.input(key("F"))
	assert.True(t, drawsBackdrop(b))
	a.input(motion(2, 2))
	assert.True(t, drawsBackdrop(a))

	a.input(key("f"))
	a.input(key("m"))
	a.input(key("Return"))
	_, ok, err := sess.outcome(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, sess.acceptedFrozen(), "a pick on the live screen is captured live")
}

// Without a frozen screen, or in record mode, F does nothing and is not shown.
func TestFreezeKeyHiddenWithoutBackdrop(t *testing.T) {
	m := newTestModel(t)
	assert.NotContains(t, m.footerText, "F ")
	assert.False(t, m.input(key("f")))

	sess, _ := testSession()
	sess.canFreeze = true
	m = frozenModel(t, 0, sess)
	m.mode = ports.Record
	m.refreshFooter()
	assert.NotContains(t, m.footerText, "F ")
	assert.False(t, m.input(key("f")))
	m.input(key("m"))
	m.input(key("Return"))
	assert.True(t, sess.acceptedFrozen(), "untouched, the frozen state is kept for the app to thaw")
}
