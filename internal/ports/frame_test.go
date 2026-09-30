package ports

import (
	"errors"
	"strings"
	"testing"
)

func TestFrameValidation(t *testing.T) {
	valid := Frame{Pixels: make([]byte, 24), Width: 2, Height: 2, Stride: 12, Format: XRGB8888}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		frame       Frame
		wantMessage string
		wantIs      error
	}{
		{"zero width", Frame{Width: 0, Height: 1, Stride: 4, Pixels: make([]byte, 4)}, "invalid frame dimensions", nil},
		{"negative height", Frame{Width: 1, Height: -1, Stride: 4, Pixels: make([]byte, 4)}, "invalid frame dimensions", nil},
		{"oversized width", Frame{Width: MaxDimension + 1, Height: 1, Stride: (MaxDimension + 1) * 4, Pixels: make([]byte, (MaxDimension+1)*4)}, "invalid frame dimensions", nil},
		{"oversized height", Frame{Width: 1, Height: MaxDimension + 1, Stride: 4, Pixels: make([]byte, (MaxDimension+1)*4)}, "invalid frame dimensions", nil},
		{"short aligned stride", Frame{Width: 2, Height: 1, Stride: 4, Pixels: make([]byte, 4)}, "invalid frame stride", nil},
		{"unaligned stride", Frame{Width: 1, Height: 1, Stride: 6, Pixels: make([]byte, 6)}, "invalid frame stride", nil},
		// The error family proves storage bounds run before the short-slice check.
		{"excessive stride", Frame{Width: 1, Height: 2, Stride: MaxFrameBytes/2 + 4}, "invalid frame stride", nil},
		{"overflow stride", Frame{Width: 1, Height: 2, Stride: int(^uint(0)>>1) &^ 3}, "invalid frame stride", nil},
		{"unsupported format", Frame{Width: 1, Height: 1, Stride: 4, Pixels: make([]byte, 4), Format: 42}, "", ErrUnsupportedFormat},
		{"short storage", Frame{Width: 2, Height: 2, Stride: 12, Pixels: make([]byte, 23)}, "short frame storage", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.frame.Validate()
			if err == nil {
				t.Fatal("accepted invalid frame")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("got %v, want %v", err, tc.wantIs)
			}
			if tc.wantMessage != "" && !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("got %v, want %s", err, tc.wantMessage)
			}
		})
	}
}

func TestFrameRow(t *testing.T) {
	f := Frame{Pixels: []byte{1, 2, 3, 4, 99, 99, 99, 99, 5, 6, 7, 8, 99, 99, 99, 99}, Width: 1, Height: 2, Stride: 8, Format: ARGB8888}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := f.Row(0); len(got) != 4 || got[0] != 1 {
		t.Fatalf("row: %v", got)
	}
	f.YInvert = true
	if got := f.Row(0); len(got) != 4 || got[0] != 5 {
		t.Fatalf("inverted row: %v", got)
	}
}

func TestFrameAllocations(t *testing.T) {
	f := Frame{Pixels: make([]byte, 16), Width: 2, Height: 2, Stride: 8, Format: XRGB8888}
	if allocs := testing.AllocsPerRun(1000, func() {
		if f.Validate() != nil {
			panic("invalid test frame")
		}
		_ = f.Row(0)
	}); allocs != 0 {
		t.Fatalf("frame view: %g allocations", allocs)
	}
}

func BenchmarkFrameRows(b *testing.B) {
	f := Frame{Pixels: make([]byte, 1920*1080*4), Width: 1920, Height: 1080, Stride: 1920 * 4, Format: XRGB8888}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := f.Validate(); err != nil {
			b.Fatal(err)
		}
		for y := 0; y < f.Height; y++ {
			_ = f.Row(y)
		}
	}
}
