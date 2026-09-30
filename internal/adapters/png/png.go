// Package png writes opaque SDR captures as PNG files.
package png

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image"
	stdpng "image/png"
	"io"
	"io/fs"
	"os"

	"github.com/bnema/nefercap/internal/ports"
)

// ctxCheckRows bounds how many rows are converted between cancellation checks.
const ctxCheckRows = 64

// Writer implements ports.ScreenshotWriter. It is stateless and safe for
// concurrent use; each Save owns its own conversion buffer.
type Writer struct{}

var _ ports.ScreenshotWriter = (*Writer)(nil)

// New returns a PNG screenshot writer.
func New() *Writer { return &Writer{} }

// Save encodes f as an opaque RGB PNG at path. The path is created with
// O_EXCL, so an existing file (or symlink) is never overwritten and yields
// ports.ErrPathExists. Any file created by a failed Save is removed. All reads
// of f.Pixels complete before Save returns.
func (*Writer) Save(ctx context.Context, f ports.Frame, path string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if path == "" {
		return errors.New("png: empty path")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ports.ErrPathExists, path)
		}
		return fmt.Errorf("png: create output: %w", err)
	}
	// We created the file exclusively, so we own it and may remove it.
	defer func() {
		if err == nil {
			if cerr := file.Close(); cerr != nil {
				err = fmt.Errorf("png: close output: %w", cerr)
			}
		} else {
			_ = file.Close()
		}
		if err != nil {
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("png: remove partial output: %w", rerr))
			}
		}
	}()
	return encode(ctx, file, f)
}

// encode writes f to w. The frame is converted once to opaque RGBA because the
// encoder only has fast paths for the standard image types and stored pixels
// are BGRA; the copy is exactly one frame and is released on return.
func encode(ctx context.Context, w io.Writer, f ports.Frame) error {
	img, err := toRGBA(ctx, f)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(&ctxWriter{ctx: ctx, w: w}, 64<<10)
	if err := stdpng.Encode(bw, img); err != nil {
		return fmt.Errorf("png: encode: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("png: write: %w", err)
	}
	return nil
}

// toRGBA converts top-to-bottom, honoring stride and YInvert, and forces alpha
// to 255 because output is opaque (XRGB padding and ARGB alpha are ignored).
func toRGBA(ctx context.Context, f ports.Frame) (*image.RGBA, error) {
	rowLen := f.Width * ports.BytesPerPixel
	img := &image.RGBA{
		Pix:    make([]byte, rowLen*f.Height),
		Stride: rowLen,
		Rect:   image.Rect(0, 0, f.Width, f.Height),
	}
	for y := 0; y < f.Height; y++ {
		if y%ctxCheckRows == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		src := f.Row(y)
		dst := img.Pix[y*rowLen : (y+1)*rowLen]
		dst = dst[:len(src)]
		for x := 0; x+3 < len(src); x += 4 {
			dst[x] = src[x+2]
			dst[x+1] = src[x+1]
			dst[x+2] = src[x]
			dst[x+3] = 0xff
		}
	}
	return img, nil
}

// ctxWriter makes a long encode stop at the next write after cancellation.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
