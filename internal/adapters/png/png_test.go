package png

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	stdpng "image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/nefercap/internal/ports"
)

func decode(t *testing.T, path string) image.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := stdpng.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestSaveConvertsBGRAWithStrideAndInvert(t *testing.T) {
	// 2x2 pixels, stride 12 (4 padding bytes per row), stored bottom-first.
	// Pixel bytes are B,G,R,A; alpha values must be ignored.
	pix := []byte{
		// stored row 0 == bottom row
		30, 20, 10, 0, 60, 50, 40, 7, 0xEE, 0xEE, 0xEE, 0xEE,
		// stored row 1 == top row
		3, 2, 1, 255, 6, 5, 4, 0, 0xEE, 0xEE, 0xEE, 0xEE,
	}
	for _, format := range []ports.PixelFormat{ports.ARGB8888, ports.XRGB8888} {
		f := ports.Frame{Pixels: pix, Width: 2, Height: 2, Stride: 12, Format: format, YInvert: true}
		path := filepath.Join(t.TempDir(), "shot.png")
		if err := New().Save(context.Background(), f, path); err != nil {
			t.Fatal(err)
		}
		img := decode(t, path)
		want := [2][2]color.RGBA{
			{{1, 2, 3, 255}, {4, 5, 6, 255}},
			{{10, 20, 30, 255}, {40, 50, 60, 255}},
		}
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				if got := color.RGBAModel.Convert(img.At(x, y)); got != want[y][x] {
					t.Fatalf("format %d pixel (%d,%d): got %v want %v", format, x, y, got, want[y][x])
				}
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if raw[25] != 2 { // IHDR colour type 2 is truecolour without alpha
			t.Fatalf("colour type %d, want 2 (opaque RGB)", raw[25])
		}
	}
}

func TestSaveDoesNotModifyFrame(t *testing.T) {
	pix := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	orig := bytes.Clone(pix)
	f := ports.Frame{Pixels: pix, Width: 2, Height: 1, Stride: 8, Format: ARGB}
	if err := New().Save(context.Background(), f, filepath.Join(t.TempDir(), "a.png")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pix, orig) {
		t.Fatal("frame storage was modified")
	}
}

const ARGB = ports.ARGB8888

func TestSaveNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exists.png")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := ports.Frame{Pixels: make([]byte, 4), Width: 1, Height: 1, Stride: 4, Format: ARGB}
	err := New().Save(context.Background(), f, path)
	if !errors.Is(err, ports.ErrPathExists) {
		t.Fatalf("got %v, want ErrPathExists", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep" {
		t.Fatalf("existing file changed: %q", got)
	}
}

func TestSaveDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	f := ports.Frame{Pixels: make([]byte, 4), Width: 1, Height: 1, Stride: 4, Format: ARGB}
	if err := New().Save(context.Background(), f, link); !errors.Is(err, ports.ErrPathExists) {
		t.Fatalf("got %v, want ErrPathExists", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target was created: %v", err)
	}
}

func TestSaveInvalidInputCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.png")
	bad := ports.Frame{Pixels: make([]byte, 3), Width: 1, Height: 1, Stride: 4, Format: ARGB}
	if err := New().Save(context.Background(), bad, path); err == nil {
		t.Fatal("accepted short frame")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok := ports.Frame{Pixels: make([]byte, 4), Width: 1, Height: 1, Stride: 4, Format: ARGB}
	if err := New().Save(ctx, ok, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("unexpected files: %v", entries)
	}
}

func TestSaveCancelDuringEncodeRemovesPartialOutput(t *testing.T) {
	const side = 2048
	pix := make([]byte, side*side*4)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range pix {
		pix[i] = byte(rng.Uint32())
	}
	f := ports.Frame{Pixels: pix, Width: side, Height: side, Stride: side * 4, Format: ports.XRGB8888}
	path := filepath.Join(t.TempDir(), "partial.png")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(path); err == nil {
				cancel()
				return
			}
			time.Sleep(50 * time.Microsecond)
		}
	}()
	if err := New().Save(ctx, f, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output remains: %v", err)
	}
}

func BenchmarkSave(b *testing.B) {
	for _, size := range []struct {
		name string
		w, h int
	}{{"1080p", 1920, 1080}, {"4K", 3840, 2160}} {
		b.Run(size.name, func(b *testing.B) {
			f := ports.Frame{Pixels: make([]byte, size.w*size.h*4), Width: size.w, Height: size.h, Stride: size.w * 4, Format: ports.XRGB8888}
			dir := b.TempDir()
			w := New()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				b.StopTimer()
				path := filepath.Join(dir, "b.png")
				_ = os.Remove(path)
				b.StartTimer()
				if err := w.Save(context.Background(), f, path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
