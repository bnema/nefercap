// Package clipboard streams PNG screenshots to the native Wayland clipboard.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/bnema/nefercap/internal/adapters/capturefile"
	"github.com/bnema/nefercap/internal/ports"
)

// Native uses wl-copy's default fork: the clipboard owner intentionally survives
// nefercap. Only the initial handoff is bounded; never use --foreground here.
type Native struct{}

func New() *Native { return &Native{} }

func (*Native) Copy(ctx context.Context, r io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "wl-copy", "--type", "image/png")
	cmd.Stdin = r
	// Give wl-copy a real descriptor: its forked owner inherits stderr, so
	// exec's automatic stderr copier would wait for that long-lived owner.
	rd, wr, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("wl-copy stderr: %w", err)
	}
	stderr := &limitedError{}
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(stderr, rd); close(drained) }()
	cmd.Stderr = wr
	cmd.WaitDelay = 200 * time.Millisecond
	err = cmd.Run()
	_ = wr.Close()
	// Drain queued diagnostics, but never wait for the forked owner to close.
	if deadlineErr := rd.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); deadlineErr != nil {
		_ = rd.Close()
	}
	<-drained
	_ = rd.Close()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("wl-copy: %w", ctx.Err())
		}
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("wl-copy not found: install wl-clipboard: %w", err)
		}
		return fmt.Errorf("wl-copy: %w: %s", err, stderr.data)
	}
	return nil
}

type limitedError struct{ data []byte }

func (b *limitedError) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 4096 - len(b.data)
	b.data = append(b.data, p[:min(remaining, len(p))]...)
	return n, nil
}

// pngStream keeps diagnostic formatters from inspecting a concurrently active pipe.
type pngStream struct{ io.Reader }

func (pngStream) String() string { return "PNG stream" }

// Writer composes the existing PNG codec and clipboard port. Saved is set only
// after encoding a valid file, and survives clipboard failures.
// Each Writer belongs to one screenshot invocation.
type Writer struct {
	codec     ports.ScreenshotWriter
	clipboard ports.Clipboard
	Saved     bool
}

func NewWriter(codec ports.ScreenshotWriter, clipboard ports.Clipboard) *Writer {
	return &Writer{codec: codec, clipboard: clipboard}
}

func (w *Writer) Encode(ctx context.Context, out io.Writer, f ports.Frame) error {
	return w.codec.Encode(ctx, out, f)
}

func (w *Writer) Save(ctx context.Context, f ports.Frame, path string) (err error) {
	w.Saved = false
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if path == "" {
		r, out := io.Pipe()
		encoded := make(chan error, 1)
		go func() {
			err := w.codec.Encode(ctx, out, f)
			_ = out.CloseWithError(err)
			encoded <- err
		}()
		copyErr := w.clipboard.Copy(ctx, pngStream{r})
		_ = r.CloseWithError(copyErr)
		return errors.Join(<-encoded, copyErr)
	}
	file, reservation, err := capturefile.CreateReadable(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err = w.codec.Encode(ctx, file, f); err != nil {
		return errors.Join(err, reservation.Remove())
	}
	// Reuse the original descriptor, never reopen a potentially replaced path.
	w.Saved = true
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return w.clipboard.Copy(ctx, file)
}
