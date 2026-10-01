package wayland

import (
	"context"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/ports"
)

func bigOutput() []testserver.OutputSpec {
	return []testserver.OutputSpec{{Name: "BIG", Width: 64, Height: 48, Scale: 1}}
}

// Without the extension a region is cropped from the output frame, borrowing
// the shared storage: no pixel is copied and the full stride is kept.
func TestCaptureRegionCropsOutputFrame(t *testing.T) {
	s, srv := newSource(t, testserver.Config{Outputs: bigOutput()})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 10, Y: 5, Width: 20, Height: 8}})
	require.NoError(t, err)
	require.NoError(t, f.Validate())
	require.Equal(t, [3]int{20, 8, 64 * 4}, [3]int{f.Width, f.Height, f.Stride})
	want := testserver.Pixel(10, 5, 1)
	require.Equal(t, want[:], f.Row(0)[:4])
	want = testserver.Pixel(29, 12, 1)
	require.Equal(t, want[:], f.Row(7)[19*4:])
	require.Equal(t, "output", srv.Stats().LastSource)
	require.Equal(t, 1, srv.Stats().Buffers, "a single screenshot allocates a single buffer")
}

func TestCropIsAWindowOfTheFrame(t *testing.T) {
	out := &output{id: 1, width: 64, height: 48, scale: 1}
	f := ports.Frame{Pixels: make([]byte, 64*48*4), Width: 64, Height: 48, Stride: 64 * 4, Format: ports.XRGB8888}
	c, err := cropFrame(f, ports.Region{X: 10, Y: 5, Width: 20, Height: 8}, out)
	require.NoError(t, err)
	require.Equal(t, unsafe.Add(unsafe.Pointer(&f.Pixels[0]), 5*64*4+10*4), unsafe.Pointer(&c.Pixels[0]))
	require.Equal(t, 7*64*4+20*4, len(c.Pixels), "the window ends with the last row's pixels")
}

func TestCropRoundsToBufferPixels(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Outputs: []testserver.OutputSpec{{Name: "HI", Width: 64, Height: 48, Scale: 2}}})
	out := firstOutput(t, s)
	// logical size is 32x24 (mode over scale): one logical pixel is two buffer pixels
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 3, Y: 2, Width: 5, Height: 4}})
	require.NoError(t, err)
	require.Equal(t, [2]int{10, 8}, [2]int{f.Width, f.Height})
	want := testserver.Pixel(6, 4, 1)
	require.Equal(t, want[:], f.Row(0)[:4])
}

func TestCropClipsToOutputAndRejectsOutside(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Outputs: bigOutput()})
	out := firstOutput(t, s)
	f, err := s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 60, Y: 40, Width: 100, Height: 100}})
	require.NoError(t, err)
	require.Equal(t, [2]int{4, 8}, [2]int{f.Width, f.Height})
	_, err = s.Capture(context.Background(), ports.Target{OutputID: out.ID, Region: ports.Region{X: 64, Y: 0, Width: 4, Height: 4}})
	require.ErrorIs(t, err, ports.ErrInvalidRegion)
}

func TestCropUsesXdgOutputLogicalSize(t *testing.T) {
	s, _ := newSource(t, testserver.Config{Outputs: []testserver.OutputSpec{{Name: "FR", Width: 64, Height: 48, Scale: 1, LogicalWidth: 32, LogicalHeight: 24}}})
	f, err := s.Capture(context.Background(), ports.Target{OutputID: firstOutput(t, s).ID, Region: ports.Region{Width: 16, Height: 12}})
	require.NoError(t, err)
	require.Equal(t, [2]int{32, 24}, [2]int{f.Width, f.Height}, "fractional scale: logical 16x12 is 32x24 buffer pixels")
}

// The crop must not allocate: it is a window, not a copy.
func TestCropAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("race instrumentation changes allocation counts")
	}
	out := &output{id: 1, width: 1920, height: 1080, scale: 1}
	f := ports.Frame{Pixels: make([]byte, 1920*1080*4), Width: 1920, Height: 1080, Stride: 1920 * 4, Format: ports.XRGB8888}
	r := ports.Region{X: 100, Y: 100, Width: 800, Height: 600}
	runtime.GC()
	avg := testing.AllocsPerRun(1000, func() {
		if _, err := cropFrame(f, r, out); err != nil {
			panic(err)
		}
	})
	require.Zero(t, avg)
}
