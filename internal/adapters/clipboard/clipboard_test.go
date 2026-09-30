package clipboard

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/nefercap/internal/adapters/png"
	mocks "github.com/bnema/nefercap/internal/mocks/ports"
	"github.com/bnema/nefercap/internal/ports"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var frame = ports.Frame{Pixels: []byte{1, 2, 3, 0}, Width: 1, Height: 1, Stride: 4, Format: ports.XRGB8888}

func TestWriterDestinations(t *testing.T) {
	for _, both := range []bool{false, true} {
		t.Run(map[bool]string{false: "clipboard only", true: "file and clipboard"}[both], func(t *testing.T) {
			dir := t.TempDir()
			path := ""
			if both {
				path = filepath.Join(dir, "shot.png")
			}
			clip := mocks.NewMockClipboard(t)
			var data []byte
			clip.EXPECT().Copy(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r io.Reader) error {
				var err error
				data, err = io.ReadAll(r)
				return err
			}).Once()
			w := NewWriter(png.New(), clip)
			require.NoError(t, w.Save(context.Background(), frame, path))
			require.Equal(t, both, w.Saved)
			require.True(t, strings.HasPrefix(string(data), "\x89PNG"))
			if both {
				disk, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, disk, data)
			} else {
				files, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Empty(t, files)
			}
		})
	}
}

func TestSavedFileSurvivesClipboardFailureAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shot.png")
	clip := mocks.NewMockClipboard(t)
	failure := errors.New("clipboard unavailable")
	clip.EXPECT().Copy(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r io.Reader) error {
		require.NoError(t, os.Rename(path, path+".saved"))
		require.NoError(t, os.Symlink(path+".replacement", path))
		data, err := io.ReadAll(r)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(string(data), "\x89PNG"))
		return failure
	}).Once()
	w := NewWriter(png.New(), clip)
	require.ErrorIs(t, w.Save(context.Background(), frame, path), failure)
	require.True(t, w.Saved)
	disk, err := os.ReadFile(path + ".saved")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(disk), "\x89PNG"))
}

func TestWriterRejectsBeforeClipboard(t *testing.T) {
	clip := mocks.NewMockClipboard(t)
	w := NewWriter(png.New(), clip)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "shot.png")
	require.ErrorIs(t, w.Save(ctx, frame, path), context.Canceled)
	require.Error(t, w.Save(context.Background(), ports.Frame{}, path))
	require.NoFileExists(t, path)
	require.NoError(t, os.WriteFile(path, []byte("keep"), 0600))
	require.ErrorIs(t, w.Save(context.Background(), frame, path), ports.ErrPathExists)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "keep", string(disk))
}

func TestEncodeFailureAndEarlyClipboardExit(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "failed.png")} {
		codec := mocks.NewMockScreenshotWriter(t)
		clip := mocks.NewMockClipboard(t)
		failure := errors.New("encode failed")
		codec.EXPECT().Encode(mock.Anything, mock.Anything, frame).Return(failure).Once()
		if path == "" {
			clip.EXPECT().Copy(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }).Once()
		}
		w := NewWriter(codec, clip)
		require.ErrorIs(t, w.Save(context.Background(), frame, path), failure)
		require.False(t, w.Saved)
		if path != "" {
			require.NoFileExists(t, path)
		}
	}
	clip := mocks.NewMockClipboard(t)
	failure := errors.New("early exit")
	clip.EXPECT().Copy(mock.Anything, mock.Anything).Return(failure).Once()
	require.ErrorIs(t, NewWriter(png.New(), clip).Save(context.Background(), frame, ""), failure)
}

// Real executable fixtures exercise os/exec without a handwritten process fake.
func TestNativeProcessFailures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	require.ErrorContains(t, New().Copy(context.Background(), strings.NewReader("PNG")), "install wl-clipboard")
	executable := filepath.Join(dir, "wl-copy")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'no compositor' >&2\nexit 7\n"), 0700))
	require.ErrorContains(t, New().Copy(context.Background(), strings.NewReader("PNG")), "no compositor")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexec /bin/sleep 10\n"), 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	began := time.Now()
	require.ErrorIs(t, New().Copy(ctx, strings.NewReader("PNG")), context.DeadlineExceeded)
	require.Less(t, time.Since(began), time.Second)
}

func TestNativeSuccessfulForkDoesNotWaitForStderrOwner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	// This is a real child process, retaining only stderr after handoff.
	executable := filepath.Join(dir, "wl-copy")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n/bin/cat >/dev/null\n/bin/sleep 1 >/dev/null 2>&2 &\nexit 0\n"), 0o700))
	began := time.Now()
	require.NoError(t, New().Copy(context.Background(), strings.NewReader("PNG")))
	require.Less(t, time.Since(began), 500*time.Millisecond)
}

func TestCopyCancellationJoinsEncoder(t *testing.T) {
	codec := mocks.NewMockScreenshotWriter(t)
	clip := mocks.NewMockClipboard(t)
	started, encoded := make(chan struct{}), make(chan struct{})
	codec.EXPECT().Encode(mock.Anything, mock.Anything, frame).RunAndReturn(func(_ context.Context, out io.Writer, _ ports.Frame) error {
		close(started)
		_, err := out.Write([]byte("PNG")) // backpressure until the reader is closed
		close(encoded)
		return err
	}).Once()
	clip.EXPECT().Copy(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ io.Reader) error {
		<-ctx.Done()
		return ctx.Err()
	}).Once()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- NewWriter(codec, clip).Save(ctx, frame, "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("encoder did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("clipboard cancellation did not terminate encoding")
	}
	select {
	case <-encoded:
	default:
		t.Fatal("Save returned before the encoder released its borrowed frame")
	}
}

func TestStderrBound(t *testing.T) {
	b := &limitedError{}
	n, err := b.Write(make([]byte, 1<<20))
	require.NoError(t, err)
	require.Equal(t, 1<<20, n)
	require.Len(t, b.data, 4096)
}

func BenchmarkClipboardEncode(b *testing.B) {
	f := ports.Frame{Pixels: make([]byte, 1920*1080*4), Width: 1920, Height: 1080, Stride: 1920 * 4, Format: ports.XRGB8888}
	b.ReportAllocs()
	for b.Loop() {
		if err := png.New().Encode(context.Background(), io.Discard, f); err != nil {
			b.Fatal(err)
		}
	}
}
