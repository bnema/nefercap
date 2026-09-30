package ports

import (
	"testing"
)

func TestFrameValidation(t *testing.T) {
	valid := Frame{Pixels: make([]byte, 24), Width: 2, Height: 2, Stride: 12, Format: XRGB8888}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Frame)
	}{
		{"zero width", func(f *Frame) { f.Width = 0 }},
		{"negative height", func(f *Frame) { f.Height = -1 }},
		{"oversized width", func(f *Frame) { f.Width = MaxDimension + 1 }},
		{"short stride", func(f *Frame) { f.Stride = 7 }},
		{"oversized storage", func(f *Frame) { f.Stride = MaxFrameBytes }},
		{"unsupported format", func(f *Frame) { f.Format = 42 }},
		{"short storage", func(f *Frame) { f.Pixels = f.Pixels[:23] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := valid
			tc.change(&f)
			if f.Validate() == nil {
				t.Fatal("accepted invalid frame")
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
