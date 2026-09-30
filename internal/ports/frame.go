package ports

import "fmt"

const (
	BytesPerPixel = 4
	MaxDimension  = 16384
	MaxFrameBytes = 256 << 20
	MaxFPS        = 120
)

// Validate bounds all arithmetic before adapters map or consume shared storage.
func (f Frame) Validate() error {
	if f.Width < 1 || f.Height < 1 || f.Width > MaxDimension || f.Height > MaxDimension {
		return fmt.Errorf("invalid frame dimensions %dx%d", f.Width, f.Height)
	}
	if f.Stride < f.Width*BytesPerPixel || f.Stride%BytesPerPixel != 0 || f.Stride > MaxFrameBytes/f.Height {
		return fmt.Errorf("invalid frame stride %d", f.Stride)
	}
	if f.Format != XRGB8888 && f.Format != ARGB8888 {
		return fmt.Errorf("%w: %d", ErrUnsupportedFormat, f.Format)
	}
	// The last row needs only its pixels: a cropped frame borrows a window of
	// a larger buffer, with no stride padding after its final row.
	if len(f.Pixels) < (f.Height-1)*f.Stride+f.Width*BytesPerPixel {
		return fmt.Errorf("short frame storage: got %d bytes", len(f.Pixels))
	}
	return nil
}

// Row returns a packed view in top-to-bottom order, excluding stride padding.
// Call Validate before using Row. The returned slice borrows frame storage.
func (f Frame) Row(y int) []byte {
	offset := y * f.Stride
	return f.Pixels[offset : offset+f.Width*BytesPerPixel]
}
