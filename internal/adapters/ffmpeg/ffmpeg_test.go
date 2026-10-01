package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/bnema/nefercap/internal/ports"
)

const helperEnv = "NEFERCAP_FFMPEG_HELPER"

// TestMain turns the test binary into a stand-in encoder when helperEnv is set.
// The Writer runs it with FFmpeg's arguments; they are ignored.
func TestMain(m *testing.M) {
	switch mode := os.Getenv(helperEnv); mode {
	case "":
		os.Exit(m.Run())
	case "exit1": // fails immediately with a large diagnostic
		fmt.Fprint(os.Stderr, strings.Repeat("boom ", 10000))
		os.Exit(1)
	case "hang": // never reads stdin and ignores EOF
		time.Sleep(time.Hour)
	case "drain": // reads to EOF, writes output to fd 3, exits cleanly
		n, _ := io.Copy(io.Discard, os.Stdin)
		out := os.NewFile(3, "out")
		fmt.Fprintf(out, "bytes=%d", n)
		out.Close()
		os.Exit(0)
	case "slowclose": // drains, then refuses to exit after EOF
		_, _ = io.Copy(io.Discard, os.Stdin)
		time.Sleep(time.Hour)
	}
	os.Exit(2)
}

func newFrame(w, h, stride int, fill byte) ports.Frame {
	pix := make([]byte, stride*h)
	for i := range pix {
		pix[i] = fill
	}
	return ports.Frame{Pixels: pix, Width: w, Height: h, Stride: stride, Format: ports.XRGB8888}
}

func helperWriter(t *testing.T, mode string) *Writer {
	t.Helper()
	t.Setenv(helperEnv, mode)
	w := NewWithBinary(os.Args[0])
	w.closeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { _ = w.Abort() })
	return w
}

func gone(pid int) bool { return syscall.Kill(pid, 0) == syscall.ESRCH }

func settings() ports.VideoSettings { return ports.VideoSettings{FPS: 30} }

func TestStartValidation(t *testing.T) {
	f := newFrame(64, 48, 256, 0)
	odd := newFrame(63, 47, 256, 0)
	cases := []struct {
		name  string
		frame ports.Frame
		set   ports.VideoSettings
		ok    bool
	}{
		{"fps zero", f, ports.VideoSettings{}, false},
		{"fps too high", f, ports.VideoSettings{FPS: 121}, false},
		{"one dimension", f, ports.VideoSettings{FPS: 30, Width: 32}, false},
		{"odd output", f, ports.VideoSettings{FPS: 30, Width: 33, Height: 32}, false},
		{"huge output", f, ports.VideoSettings{FPS: 30, Width: 16386, Height: 32}, false},
		{"odd source without size", odd, ports.VideoSettings{FPS: 30}, false},
		{"odd width source without size", newFrame(63, 48, 256, 0), ports.VideoSettings{FPS: 30}, false},
		{"odd source explicit scale", odd, ports.VideoSettings{FPS: 30, Width: 32, Height: 24}, true},
		{"even source retained", f, ports.VideoSettings{FPS: 30}, true},
		{"output over frame limit", f, ports.VideoSettings{FPS: 30, Width: 16384, Height: 16384}, false},
		{"even scale", f, ports.VideoSettings{FPS: 120, Width: 32, Height: 24}, true},
		{"1x1 source without size", newFrame(1, 1, 4, 0), ports.VideoSettings{FPS: 30}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := helperWriter(t, "drain")
			path := filepath.Join(t.TempDir(), "v.mp4")
			err := w.Start(context.Background(), tc.frame, path, tc.set)
			if (err == nil) != tc.ok {
				t.Fatalf("Start error = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok {
				if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
					t.Fatalf("rejected Start left a file: %v", serr)
				}
			}
		})
	}
}

func TestBuildArgs(t *testing.T) {
	f := newFrame(63, 47, 252, 0)
	args := strings.Join(buildArgs(f, 62, 46, 30), " ")
	for _, want := range []string{"-pix_fmt bgr0", "-video_size 63x47", "-vf scale=62:46:", "-preset veryfast", "-tune zerolatency", "libx264", "-threads 2", "-filter_threads 1", "pipe:3", "-an"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "crop") {
		t.Error("frames must never be cropped")
	}
}

func TestOutputSize(t *testing.T) {
	const limit = ports.MaxFrameBytes / ports.BytesPerPixel
	f := newFrame(64, 48, 256, 0)
	for _, tc := range []struct {
		name         string
		frame        ports.Frame
		set          ports.VideoSettings
		wantW, wantH int
		ok           bool
	}{
		{"retain", f, ports.VideoSettings{FPS: 1}, 64, 48, true},
		{"odd height source", newFrame(64, 47, 256, 0), ports.VideoSettings{FPS: 1}, 0, 0, false},
		{"explicit scale of odd source", newFrame(63, 47, 252, 0), ports.VideoSettings{FPS: 1, Width: 64, Height: 48}, 64, 48, true},
		{"only width", f, ports.VideoSettings{FPS: 1, Width: 64}, 0, 0, false},
		{"negative", f, ports.VideoSettings{FPS: 1, Width: -2, Height: 2}, 0, 0, false},
		{"at frame byte limit", f, ports.VideoSettings{FPS: 1, Width: 8192, Height: limit / 8192}, 8192, limit / 8192, true},
		{"over frame byte limit", f, ports.VideoSettings{FPS: 1, Width: 8192, Height: limit/8192 + 2}, 0, 0, false},
		{"max dimensions", f, ports.VideoSettings{FPS: 1, Width: ports.MaxDimension, Height: ports.MaxDimension}, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := outputSize(tc.frame, tc.set)
			if (err == nil) != tc.ok || w != tc.wantW || h != tc.wantH {
				t.Fatalf("got %dx%d, %v; want %dx%d ok=%v", w, h, err, tc.wantW, tc.wantH, tc.ok)
			}
		})
	}
}

func TestOutputFilesArePrivate(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(4, 4, 16, 0)
	if err := w.Start(context.Background(), f, path, settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v; want 0600", info.Mode(), err)
	}
}

func TestStartRefusesExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.mp4")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := helperWriter(t, "drain")
	err := w.Start(context.Background(), newFrame(4, 4, 16, 0), path, settings())
	if !errors.Is(err, ports.ErrPathExists) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("got %v, want ErrPathExists and fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep" {
		t.Fatalf("existing file changed: %q", got)
	}
	if err := w.Write(context.Background(), newFrame(4, 4, 16, 0)); !errors.Is(err, ports.ErrClosed) {
		t.Fatalf("write on unstarted writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep" {
		t.Fatalf("Close touched foreign file: %q", got)
	}
	if w.Saved {
		t.Fatal("reported a pre-existing file as saved")
	}
}

func TestStartFailureRemovesReservedPath(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "encoder")
	if err := os.WriteFile(notExec, []byte("x"), 0o755); err != nil { // not a valid executable
		t.Fatal(err)
	}
	path := filepath.Join(dir, "v.mp4")
	w := NewWithBinary(notExec)
	if err := w.Start(context.Background(), newFrame(4, 4, 16, 0), path, settings()); err == nil {
		t.Fatal("Start succeeded with a broken encoder")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved path remains: %v", err)
	}
	w = NewWithBinary(filepath.Join(dir, "missing"))
	if err := w.Start(context.Background(), newFrame(4, 4, 16, 0), path, settings()); err == nil {
		t.Fatal("Start succeeded with a missing encoder")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserved path remains: %v", err)
	}
}

func TestDrainLifecycle(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second
	path := filepath.Join(t.TempDir(), "v.mp4")
	ctx := context.Background()
	first := newFrame(4, 4, 16, 1)
	if err := w.Start(ctx, first, path, settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, first, path, settings()); err == nil {
		t.Fatal("second Start accepted")
	}
	// Stride changes are allowed; geometry and format changes are not.
	if err := w.Write(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(ctx, newFrame(4, 4, 32, 2)); err != nil {
		t.Fatalf("stride change rejected: %v", err)
	}
	for _, bad := range []ports.Frame{newFrame(8, 4, 32, 0), newFrame(4, 8, 16, 0)} {
		if err := w.Write(ctx, bad); !errors.Is(err, ports.ErrGeometryChanged) {
			t.Fatalf("got %v, want ErrGeometryChanged", err)
		}
	}
	argb := newFrame(4, 4, 16, 0)
	argb.Format = ports.ARGB8888
	if err := w.Write(ctx, argb); !errors.Is(err, ports.ErrGeometryChanged) {
		t.Fatalf("format change: got %v", err)
	}
	// A rejected frame does not poison the stream.
	if err := w.Write(ctx, first); err != nil {
		t.Fatal(err)
	}
	pid := w.cmd.Process.Pid
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort after Close: %v", err)
	}
	if !w.Saved {
		t.Fatal("finalized recording not reported as saved")
	}
	if err := w.Write(ctx, first); !errors.Is(err, ports.ErrClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
	if !gone(pid) {
		t.Fatal("encoder not reaped")
	}
	// 3 frames of 4x4x4 bytes reached the encoder exactly (no padding).
	if got, _ := os.ReadFile(path); string(got) != "bytes=192" {
		t.Fatalf("encoder output %q", got)
	}
}

func TestEncoderExitReportsBoundedDiagnostics(t *testing.T) {
	w := helperWriter(t, "exit1")
	path := filepath.Join(t.TempDir(), "v.mp4")
	ctx := context.Background()
	f := newFrame(64, 64, 256, 0)
	if err := w.Start(ctx, f, path, settings()); err != nil {
		t.Fatal(err)
	}
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for err == nil && time.Now().Before(deadline) {
		err = w.Write(ctx, f)
	}
	if err == nil {
		t.Fatal("Write never failed")
	}
	if !strings.Contains(err.Error(), "boom") || len(err.Error()) > stderrLimit+512 {
		t.Fatalf("unexpected diagnostic (%d bytes): %.200s", len(err.Error()), err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatal("diagnostics were not bounded")
	}
	if err2 := w.Write(ctx, f); err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("failure not sticky: %v", err2)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close hid encoder failure")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed recording left a file: %v", err)
	}
}

func TestCancelInterruptsBlockedWrite(t *testing.T) {
	w := helperWriter(t, "hang")
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(1024, 1024, 4096, 0) // 4 MiB fills the pipe
	if err := w.Start(context.Background(), f, path, settings()); err != nil {
		t.Fatal(err)
	}
	pid := w.cmd.Process.Pid
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- w.Write(ctx, f) }()
	select {
	case err := <-res:
		t.Fatalf("Write returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not interrupt blocked Write")
	}
	if err := w.Write(context.Background(), f); !errors.Is(err, context.Canceled) {
		t.Fatalf("Write after interruption: %v", err)
	}
	start := time.Now()
	if err := w.Close(); err == nil {
		t.Fatal("hung encoder reported success")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %s", d)
	}
	if !gone(pid) {
		t.Fatal("hung encoder not killed and reaped")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unfinished output remains: %v", err)
	}
}

func TestStartContextCancelInterruptsWrite(t *testing.T) {
	w := helperWriter(t, "hang")
	f := newFrame(1024, 1024, 4096, 0)
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx, f, filepath.Join(t.TempDir(), "v.mp4"), settings()); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { res <- w.Write(context.Background(), f) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start context cancel did not interrupt Write")
	}
}

func TestAbortInterruptsBlockedWriteAndIsIdempotent(t *testing.T) {
	w := helperWriter(t, "hang")
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(1024, 1024, 4096, 0)
	if err := w.Start(context.Background(), f, path, settings()); err != nil {
		t.Fatal(err)
	}
	pid := w.cmd.Process.Pid
	res := make(chan error, 1)
	go func() { res <- w.Write(context.Background(), f) }()
	time.Sleep(200 * time.Millisecond)
	aborts := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() { aborts <- w.Abort() }()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-aborts:
		case <-time.After(10 * time.Second):
			t.Fatal("Abort hung")
		}
	}
	select {
	case err := <-res:
		if !errors.Is(err, ports.ErrClosed) {
			t.Fatalf("got %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write still blocked after Abort")
	}
	if !gone(pid) {
		t.Fatal("encoder not reaped")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted output remains: %v", err)
	}
}

func TestAbortEscalatesDrainingClose(t *testing.T) {
	w := helperWriter(t, "slowclose")
	w.closeTimeout = time.Minute
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(4, 4, 16, 0)
	if err := w.Start(context.Background(), f, path, settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	pid := w.cmd.Process.Pid
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	time.Sleep(200 * time.Millisecond)
	_ = w.Abort()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("killed encoder reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close still draining after Abort")
	}
	if !gone(pid) {
		t.Fatal("encoder not reaped")
	}
}

func TestCloseWatchdogKillsStuckEncoder(t *testing.T) {
	w := helperWriter(t, "slowclose")
	f := newFrame(4, 4, 16, 0)
	if err := w.Start(context.Background(), f, filepath.Join(t.TempDir(), "v.mp4"), settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	pid := w.cmd.Process.Pid
	start := time.Now()
	err := w.Close()
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("got %v, want kill error", err)
	}
	if time.Since(start) > 5*time.Second || !gone(pid) {
		t.Fatal("watchdog did not bound shutdown")
	}
}

func TestCloseWithoutFrames(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second // helper start-up is slow under -race
	path := filepath.Join(t.TempDir(), "v.mp4")
	if err := w.Start(context.Background(), newFrame(4, 4, 16, 0), path, settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, ErrNoFrames) {
		t.Fatalf("got %v, want ErrNoFrames", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty recording remains: %v", err)
	}
}

func TestNeverStartedTerminalCallsAreNoOps(t *testing.T) {
	w := New()
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherRegisteredOncePerContext(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second
	f := newFrame(4, 4, 16, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx, f, filepath.Join(t.TempDir(), "v.mp4"), settings()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ { // one watcher registration for the whole loop
		if err := w.Write(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if w.writeGen != 1 {
		t.Fatalf("watcher registered %d times for one context", w.writeGen)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	cancel() // after Close: must be harmless
	if err := w.Write(ctx, f); !errors.Is(err, ports.ErrClosed) {
		t.Fatalf("Write after Close: %v, want ErrClosed", err)
	}
	if w.writeGen != 1 {
		t.Fatal("Write after Close registered a watcher")
	}
}

func TestWriteContextAlternation(t *testing.T) {
	w := helperWriter(t, "hang")
	f := newFrame(64, 64, 256, 0) // 16 KiB: one frame fits the pipe, many do not
	session, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()
	if err := w.Start(session, f, filepath.Join(t.TempDir(), "v.mp4"), settings()); err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if err := w.Write(ctxA, f); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { // fill the pipe until a Write blocks on the hung encoder
		for {
			if err := w.Write(ctxB, f); err != nil {
				res <- err
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	cancelA() // an earlier context must not interrupt B's blocked Write
	select {
	case err := <-res:
		t.Fatalf("cancelling A interrupted B: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if w.cause.Load() != nil {
		t.Fatal("cancelling A poisoned the writer")
	}
	cancelSession() // the Start context is the recording session and does interrupt
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session cancel did not interrupt B")
	}
}

func TestCancelCauseNormalizedToContextErr(t *testing.T) {
	custom := errors.New("custom cause")
	f := newFrame(1024, 1024, 4096, 0)
	for _, where := range []string{"start", "write"} {
		t.Run(where, func(t *testing.T) {
			w := helperWriter(t, "hang")
			cause, cancel := context.WithCancelCause(context.Background())
			startCtx, writeCtx := context.Background(), context.Background()
			if where == "start" {
				startCtx = cause
			} else {
				writeCtx = cause
			}
			if err := w.Start(startCtx, f, filepath.Join(t.TempDir(), "v.mp4"), settings()); err != nil {
				t.Fatal(err)
			}
			res := make(chan error, 1)
			go func() { res <- w.Write(writeCtx, f) }()
			time.Sleep(200 * time.Millisecond)
			cancel(custom)
			select {
			case err := <-res:
				if !errors.Is(err, context.Canceled) || errors.Is(err, custom) {
					t.Fatalf("got %v, want context.Canceled without the custom cause", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not interrupt Write")
			}
			// The sticky retry keeps the frozen-contract error.
			if err := w.Write(context.Background(), f); !errors.Is(err, context.Canceled) || errors.Is(err, custom) {
				t.Fatalf("retry: got %v", err)
			}
		})
	}
}

func TestAbortDiscardsHealthyRecording(t *testing.T) {
	w := helperWriter(t, "drain")
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(4, 4, 16, 0)
	if err := w.Start(context.Background(), f, path, settings()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Write(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	pid := w.cmd.Process.Pid
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort = %v, want nil", err)
	}
	if !gone(pid) {
		t.Fatal("encoder not reaped")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Abort kept the recording: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after Abort = %v", err)
	}
	if w.Saved {
		t.Fatal("aborted recording reported as saved")
	}
}

func TestTerminalCleanupSparesReplacementFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*Writer) error
	}{
		{"Abort", func(w *Writer) error { return w.Abort() }},
		{"Close killing a stuck encoder", func(w *Writer) error { _ = w.Close(); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := helperWriter(t, "slowclose")
			dir := t.TempDir()
			path := filepath.Join(dir, "v.mp4")
			f := newFrame(4, 4, 16, 0)
			if err := w.Start(context.Background(), f, path, settings()); err != nil {
				t.Fatal(err)
			}
			pid := w.cmd.Process.Pid
			moved := filepath.Join(dir, "moved.mp4")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("user data"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.end(w); err != nil {
				t.Fatal(err)
			}
			if !gone(pid) {
				t.Fatal("encoder not reaped")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "user data" {
				t.Fatalf("replacement was removed or changed: %q, %v", got, err)
			}
		})
	}
}

func TestSuccessfulCloseReportsOnlyOwnedFile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(t *testing.T, path, moved string)
		wantSaved bool
	}{
		{"kept", func(*testing.T, string, string) {}, true},
		{"moved and replaced", func(t *testing.T, path, moved string) {
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("user data"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"deleted", func(t *testing.T, path, _ string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := helperWriter(t, "drain")
			w.closeTimeout = 10 * time.Second
			dir := t.TempDir()
			path := filepath.Join(dir, "v.mp4")
			f := newFrame(4, 4, 16, 0)
			if err := w.Start(context.Background(), f, path, settings()); err != nil {
				t.Fatal(err)
			}
			if err := w.Write(context.Background(), f); err != nil {
				t.Fatal(err)
			}
			tc.change(t, path, filepath.Join(dir, "moved.mp4"))
			if err := w.Close(); err != nil {
				t.Fatalf("Close = %v", err)
			}
			if w.Saved != tc.wantSaved {
				t.Fatalf("Saved = %v, want %v", w.Saved, tc.wantSaved)
			}
		})
	}
}

func TestRealFFmpeg(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	if out, _ := exec.Command(bin, "-hide_banner", "-encoders").Output(); !bytes.Contains(out, []byte("libx264")) {
		t.Skip("ffmpeg lacks libx264")
	}
	probe, perr := exec.LookPath("ffprobe")

	for _, tc := range []struct {
		name          string
		w, h, stride  int
		set           ports.VideoSettings
		wantW, wantH  int
		cancelInstead bool
	}{
		{"retain padded", 64, 48, 64*4 + 16, ports.VideoSettings{FPS: 30}, 64, 48, false},
		{"odd source scaled", 63, 47, 63 * 4, ports.VideoSettings{FPS: 30, Width: 62, Height: 46}, 62, 46, false},
		{"scaled", 64, 48, 64 * 4, ports.VideoSettings{FPS: 60, Width: 32, Height: 24}, 32, 24, false},
		{"graceful cancel keeps video", 64, 48, 64 * 4, ports.VideoSettings{FPS: 30}, 64, 48, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := New()
			path := filepath.Join(t.TempDir(), "out.mp4")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFrame(tc.w, tc.h, tc.stride, 0)
			if err := w.Start(ctx, f, path, tc.set); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 30; i++ {
				for j := range f.Pixels {
					f.Pixels[j] = byte(i*8 + j)
				}
				if err := w.Write(ctx, f); err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
			}
			if tc.cancelInstead {
				cancel() // signal-style stop: the encoder must still finalize
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() == 0 {
				t.Fatalf("no output: %v", err)
			}
			if perr != nil {
				return
			}
			out, err := exec.Command(probe, "-v", "error", "-select_streams", "v:0", "-count_frames",
				"-show_entries", "stream=codec_name,width,height,nb_read_frames,pix_fmt", "-of", "csv=p=0", path).Output()
			if err != nil {
				t.Fatalf("ffprobe: %v", err)
			}
			want := fmt.Sprintf("h264,%d,%d,yuv420p,30", tc.wantW, tc.wantH)
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("probe = %q, want %q", got, want)
			}
			if streams, _ := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output(); strings.Contains(string(streams), "audio") {
				t.Fatal("output has audio")
			}
		})
	}
}

func TestRealFFmpegRefusesExistingPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "out.mp4")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := New()
	if err := w.Start(context.Background(), newFrame(64, 48, 256, 0), path, settings()); !errors.Is(err, ports.ErrPathExists) {
		t.Fatalf("got %v", err)
	}
}

func TestVideoRowsAllocations(t *testing.T) {
	cases := map[string]ports.Frame{
		"packed": newFrame(64, 32, 256, 1),
		"padded": newFrame(64, 32, 320, 1),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f.Validate(); err != nil {
				t.Fatal(err)
			}
			if allocs := testing.AllocsPerRun(200, func() {
				if err := writeRows(io.Discard, f); err != nil {
					panic(err)
				}
			}); allocs != 0 {
				t.Fatalf("io.Discard: %g allocations", allocs)
			}
			var buf bytes.Buffer
			buf.Grow(f.Width * 4 * f.Height)
			if allocs := testing.AllocsPerRun(200, func() {
				buf.Reset()
				if err := writeRows(&buf, f); err != nil {
					panic(err)
				}
			}); allocs != 0 {
				t.Fatalf("bytes.Buffer: %g allocations", allocs)
			}
		})
	}
}

func TestWriteRowsLayout(t *testing.T) {
	// 2x2, stride 12: the padding is left out.
	pix := []byte{
		1, 1, 1, 1, 2, 2, 2, 2, 9, 9, 9, 9,
		5, 5, 5, 5, 6, 6, 6, 6, 9, 9, 9, 9,
	}
	f := ports.Frame{Pixels: pix, Width: 2, Height: 2, Stride: 12, Format: ports.XRGB8888}
	var buf bytes.Buffer
	if err := writeRows(&buf, f); err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 1, 1, 1, 2, 2, 2, 2, 5, 5, 5, 5, 6, 6, 6, 6}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %v, want %v", buf.Bytes(), want)
	}
}

func TestStderrBounded(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	for i := 0; i < 100; i++ {
		if n, err := b.Write([]byte("abcdef")); n != 6 || err != nil {
			t.Fatal(n, err)
		}
	}
	if got := b.String(); got != "abcdefab [truncated]" {
		t.Fatalf("got %q", got)
	}
}

func BenchmarkVideoRows(b *testing.B) {
	for _, size := range []struct {
		name string
		w, h int
	}{{"1080p", 1920, 1080}, {"4K", 3840, 2160}} {
		b.Run(size.name, func(b *testing.B) {
			f := newFrame(size.w, size.h, size.w*4, 1)
			b.SetBytes(int64(size.w * size.h * 4))
			b.ReportAllocs()
			for b.Loop() {
				if err := writeRows(io.Discard, f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestWriteSteadyStateAllocations(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second
	f := newFrame(64, 32, 320, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx, f, filepath.Join(t.TempDir(), "v.mp4"), settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(ctx, f); err != nil { // registers the one watcher
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(200, func() {
		if err := w.Write(ctx, f); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("Write to a real pipe: %g allocations", allocs)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRowsShortWrite(t *testing.T) {
	packed := newFrame(2, 2, 8, 1)
	padded := newFrame(2, 2, 12, 1)
	for name, tc := range map[string]struct {
		f     ports.Frame
		calls int
	}{"packed": {packed, 1}, "rows": {padded, 1}} {
		t.Run(name, func(t *testing.T) {
			m := NewMockIOWriter(t)
			m.EXPECT().Write(mock.Anything).Return(3, nil).Times(tc.calls)
			if err := writeRows(m, tc.f); !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("got %v, want io.ErrShortWrite", err)
			}
		})
	}
	t.Run("error passes through", func(t *testing.T) {
		boom := errors.New("boom")
		m := NewMockIOWriter(t)
		m.EXPECT().Write(mock.Anything).Return(0, boom).Once()
		if err := writeRows(m, padded); !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCloseReportsStreamFailureEvenIfEncoderExitsCleanly(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(4, 4, 16, 0)
	ctx := context.Background()
	if err := w.Start(ctx, f, path, settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(ctx, f); err != nil {
		t.Fatal(err)
	}
	// Break the pipe under the writer: the encoder sees EOF and exits cleanly.
	_ = w.stdin.Close()
	werr := w.Write(ctx, f)
	if werr == nil {
		t.Fatal("Write to a broken pipe succeeded")
	}
	err := w.Close()
	if err == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close = %v, want the stream failure", err)
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("broken recording remains: %v", serr)
	}
}

func TestCloseKeepsVideoAfterCancelledWrite(t *testing.T) {
	w := helperWriter(t, "drain")
	w.closeTimeout = 10 * time.Second
	path := filepath.Join(t.TempDir(), "v.mp4")
	f := newFrame(4, 4, 16, 0)
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx, f, path, settings()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(ctx, f); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := w.Write(ctx, f); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("cancellation must not fail Close: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "bytes=64" {
		t.Fatalf("encoder output %q", got)
	}
}

func TestAwaitExitPrefersObservedExit(t *testing.T) {
	for name, ready := range map[string]func(*Writer){
		"abort":   func(w *Writer) { close(w.abortCh) },
		"timeout": func(w *Writer) { w.closeTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			w := NewWithBinary("unused")
			w.waitDone = make(chan struct{})
			close(w.waitDone) // the encoder has already exited
			ready(w)
			if err := w.awaitExit(); err != nil {
				t.Fatalf("clean exit reported as %v", err)
			}
		})
	}
	t.Run("abort without exit", func(t *testing.T) {
		w := NewWithBinary("unused")
		w.waitDone = make(chan struct{})
		close(w.abortCh)
		if err := w.awaitExit(); err == nil {
			t.Fatal("abort before exit reported success")
		}
	})
}
