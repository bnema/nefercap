// Package ffmpeg encodes silent SDR video by streaming raw frames to an FFmpeg
// child process that muxes fragmented MP4 into a reserved output file.
//
// Encoder settings favour a small footprint over compression: libx264 veryfast
// with -tune zerolatency (no B-frames, no lookahead, sliced threads), two
// encoder threads and one filter thread. Measured on 60 paced 30 fps 1080p
// frames, against the earlier "fast" preset with 10-frame lookahead, this cut
// peak RSS from about 335 MB to about 141 MB and CPU time by more than half,
// and made the file about 43% larger (3.8 MB to 5.4 MB) at the same CRF 23.
// ultrafast would save only about 8 MB more but grow files a further 37%.
package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bnema/nefercap/internal/adapters/capturefile"
	"github.com/bnema/nefercap/internal/ports"
)

const (
	defaultBinary = "ffmpeg"
	// closeTimeout bounds how long Close waits for the encoder to drain after
	// EOF before killing it.
	closeTimeout = 15 * time.Second
	// killTimeout bounds the wait for the reaper after a kill.
	killTimeout = 5 * time.Second
	// exitProbe bounds how long a failed pipe write waits for the exit status.
	exitProbe = time.Second
	// waitDelay bounds Wait's I/O draining once the process has exited.
	waitDelay = 2 * time.Second
	// stderrLimit caps retained encoder diagnostics.
	stderrLimit = 4 << 10
)

// ErrNoFrames is returned by Close when no frame was written, so no video exists.
var ErrNoFrames = errors.New("ffmpeg: no frames written")

// past is a write deadline that is always expired.
var past = time.Unix(1, 0)

const (
	stateIdle int32 = iota
	stateRunning
	stateClosing
	stateClosed
)

// failure is the sticky first error. fatal marks a real stream failure (as
// opposed to a caller's cancellation or abort), which Close must report even
// if the encoder itself exits cleanly.
type failure struct {
	err   error
	fatal bool
}

// Writer implements ports.VideoWriter. Start, Write and Close have one owner;
// Abort may be called concurrently with Write. A Writer is single-use.
type Writer struct {
	binary       string
	closeTimeout time.Duration

	state atomic.Int32
	mu    sync.Mutex // serializes state transitions

	// Set by Start before state becomes running; read-only afterwards.
	cmd      *exec.Cmd
	stdin    *os.File
	res      *capturefile.Reservation
	width    int
	height   int
	format   ports.PixelFormat
	stderr   *boundedBuffer
	waitDone chan struct{} // closed once cmd.Wait has returned
	waitErr  error         // valid after waitDone is closed

	frames atomic.Int64
	cause  atomic.Pointer[failure] // sticky first failure or interruption

	watchMu   sync.Mutex
	watchDead bool
	startStop func() bool
	writeStop func() bool
	writeDone <-chan struct{}
	writeGen  uint64
	active    atomic.Uint64 // generation of the Write using a per-call watcher

	abortCh   chan struct{}
	abortOnce sync.Once
	termDone  chan struct{}
	termErr   error
}

var _ ports.VideoWriter = (*Writer)(nil)

// New returns a Writer that runs the ffmpeg found on PATH.
func New() *Writer { return NewWithBinary(defaultBinary) }

// NewWithBinary returns a Writer that runs the given encoder executable, which
// must accept FFmpeg's command line. It is used to test with helper processes.
func NewWithBinary(binary string) *Writer {
	return &Writer{
		binary:       binary,
		closeTimeout: closeTimeout,
		abortCh:      make(chan struct{}),
		termDone:     make(chan struct{}),
	}
}

// outputSize validates settings and returns the encoded dimensions. Zero
// dimensions retain the source resolution exactly, which yuv420p only allows
// for even sizes; odd sources are rejected rather than cropped. Explicit
// sizes must be even and no larger than a captured frame may be.
func outputSize(f ports.Frame, s ports.VideoSettings) (int, int, error) {
	if s.FPS < 1 || s.FPS > ports.MaxFPS {
		return 0, 0, fmt.Errorf("ffmpeg: fps %d outside 1..%d", s.FPS, ports.MaxFPS)
	}
	if s.Width == 0 && s.Height == 0 {
		if f.Width%2 != 0 || f.Height%2 != 0 {
			return 0, 0, fmt.Errorf("ffmpeg: source %dx%d has an odd dimension; set an even output size to scale", f.Width, f.Height)
		}
		return f.Width, f.Height, nil
	}
	if s.Width < 2 || s.Height < 2 || s.Width > ports.MaxDimension || s.Height > ports.MaxDimension ||
		s.Width%2 != 0 || s.Height%2 != 0 {
		return 0, 0, fmt.Errorf("ffmpeg: output size %dx%d must be even, positive and at most %d", s.Width, s.Height, ports.MaxDimension)
	}
	if s.Width > ports.MaxFrameBytes/ports.BytesPerPixel/s.Height {
		return 0, 0, fmt.Errorf("ffmpeg: output size %dx%d exceeds %d bytes per frame", s.Width, s.Height, ports.MaxFrameBytes)
	}
	return s.Width, s.Height, nil
}

// buildArgs returns the FFmpeg argument vector (no shell). Input is raw bgr0
// on stdin; MP4 is fragmented to fd 3, an inherited pre-opened output file.
// The scale filter always runs: with unchanged geometry it only performs the
// required RGB to BT.709 limited-range YUV conversion and does not resample.
func buildArgs(f ports.Frame, outW, outH, fps int) []string {
	filter := "scale=" + strconv.Itoa(outW) + ":" + strconv.Itoa(outH) +
		":flags=bicubic:out_color_matrix=bt709:out_range=tv,format=yuv420p"
	rate := strconv.Itoa(fps)
	return []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "bgr0",
		"-video_size", strconv.Itoa(f.Width) + "x" + strconv.Itoa(f.Height),
		"-framerate", rate, "-i", "pipe:0",
		"-filter_threads", "1", "-an", "-vf", filter,
		"-c:v", "libx264", "-threads", "2", "-preset", "veryfast", "-tune", "zerolatency", "-crf", "23",
		"-g", strconv.Itoa(fps * 2), "-r", rate,
		"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:3",
	}
}

// Start launches the encoder and reserves path with O_EXCL; an existing path
// yields ports.ErrPathExists and is never touched. Frame geometry and format
// are fixed from firstFrame; the frame itself is not written. Cancelling ctx
// interrupts a blocked Write but does not kill the encoder: call Close to
// finalize the video.
func (w *Writer) Start(ctx context.Context, first ports.Frame, path string, s ports.VideoSettings) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state.Load() != stateIdle || w.cmd != nil {
		return errors.New("ffmpeg: writer already started")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := first.Validate(); err != nil {
		return err
	}
	if path == "" {
		return errors.New("ffmpeg: empty path")
	}
	outW, outH, err := outputSize(first, s)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(w.binary)
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	out, res, err := capturefile.Create(path)
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	// From here we own the created file and remove it (not a replacement) on failure.
	fail := func(err error, closers ...*os.File) error {
		for _, c := range closers {
			_ = c.Close()
		}
		if rerr := res.Remove(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("ffmpeg: %w", rerr))
		}
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail(fmt.Errorf("ffmpeg: stdin pipe: %w", err), out)
	}
	stderr := &boundedBuffer{limit: stderrLimit}
	cmd := exec.Command(bin, buildArgs(first, outW, outH, s.FPS)...)
	cmd.Stdin = pr
	cmd.Stderr = stderr
	cmd.ExtraFiles = []*os.File{out}
	cmd.WaitDelay = waitDelay
	// Own process group: a terminal SIGINT aimed at the recorder must not kill
	// the encoder before it receives EOF and finalizes the file.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return fail(fmt.Errorf("ffmpeg: start: %w", err), out)
	}
	// The child holds its own copies of these descriptors.
	_ = pr.Close()
	_ = out.Close()

	w.cmd, w.stdin, w.res, w.stderr = cmd, pw, res, stderr
	w.width, w.height, w.format = first.Width, first.Height, first.Format
	w.waitDone = make(chan struct{})
	go func() { // sole reaper: runs Wait exactly once, also after any Kill
		w.waitErr = cmd.Wait()
		close(w.waitDone)
	}()
	if ctx.Done() != nil {
		w.startStop = context.AfterFunc(ctx, func() { w.interrupt(ctx.Err()) })
	}
	w.state.Store(stateRunning)
	return nil
}

// interrupt records the first failure and unblocks a pending pipe write.
func (w *Writer) interrupt(err error) {
	w.cause.CompareAndSwap(nil, &failure{err: err})
	_ = w.stdin.SetWriteDeadline(past)
}

// watch makes cancellation of a Write context interrupt the pipe. The watcher
// is registered once per distinct context, not per frame, and only fires while
// a Write using it is active.
func (w *Writer) watch(ctx context.Context) (gen uint64, ok bool) {
	done := ctx.Done()
	if done == nil {
		return 0, false
	}
	w.watchMu.Lock()
	defer w.watchMu.Unlock()
	if w.watchDead {
		return 0, false
	}
	if done != w.writeDone {
		if w.writeStop != nil {
			w.writeStop()
		}
		w.writeGen++
		g := w.writeGen
		w.writeDone = done
		w.writeStop = context.AfterFunc(ctx, func() {
			if w.active.Load() == g {
				w.interrupt(ctx.Err())
			}
		})
	}
	return w.writeGen, true
}

// Write sends one frame. Rows are written straight from borrowed storage, so
// pixels are not retained. Width, height and format must match Start.
func (w *Writer) Write(ctx context.Context, f ports.Frame) error {
	if w.state.Load() != stateRunning {
		return ports.ErrClosed
	}
	if c := w.cause.Load(); c != nil {
		return c.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Validate(); err != nil {
		return err
	}
	if f.Width != w.width || f.Height != w.height || f.Format != w.format {
		return ports.ErrGeometryChanged
	}
	gen, watched := w.watch(ctx)
	if watched {
		w.active.Store(gen)
		defer w.active.Store(0)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := writeRows(w.stdin, f); err != nil {
		return w.writeFailed(err)
	}
	w.frames.Add(1)
	return nil
}

func (w *Writer) writeFailed(err error) error {
	if w.state.Load() != stateRunning {
		return ports.ErrClosed
	}
	if c := w.cause.Load(); c != nil {
		return c.err
	}
	// The encoder most likely exited; give it a moment to report why.
	t := time.NewTimer(exitProbe)
	defer t.Stop()
	select {
	case <-w.waitDone:
	case <-t.C:
	}
	msg := fmt.Errorf("ffmpeg: write frame: %w%s", err, w.diagnostics())
	w.cause.CompareAndSwap(nil, &failure{err: msg, fatal: true})
	return w.cause.Load().err
}

// diagnostics describes the exit state and captured stderr once the process
// has been reaped; it is empty while the process is still running.
func (w *Writer) diagnostics() string {
	select {
	case <-w.waitDone:
	default:
		return ""
	}
	s := ""
	if w.waitErr != nil {
		s = ": " + w.waitErr.Error()
	}
	if d := w.stderr.String(); d != "" {
		s += ": " + d
	}
	return s
}

// writeRows writes the frame in top-to-bottom row order without copying it.
// A tightly packed, upright frame is a single write.
func writeRows(dst io.Writer, f ports.Frame) error {
	rowLen := f.Width * ports.BytesPerPixel
	if !f.YInvert && f.Stride == rowLen {
		return writeFull(dst, f.Pixels[:rowLen*f.Height])
	}
	for y := 0; y < f.Height; y++ {
		if err := writeFull(dst, f.Row(y)); err != nil {
			return err
		}
	}
	return nil
}

// writeFull enforces the io.Writer contract: n < len(p) needs a non-nil error.
func writeFull(dst io.Writer, p []byte) error {
	n, err := dst.Write(p)
	if err == nil && n < len(p) {
		return io.ErrShortWrite
	}
	return err
}

// Close signals EOF, waits for the encoder to finalize the file (bounded, then
// kills it) and reports the outcome. The video is kept when the encoder exits
// cleanly after at least one frame, including after a cancelled context; it is
// removed otherwise. Close and Abort are terminal and the first one wins:
// later Close calls return the first Close result, and Close after an Abort
// returns nil because nothing was kept. Close is only concurrent with Abort
// (besides Write) as described on Abort.
func (w *Writer) Close() error { return w.terminate(false) }

// Abort discards the recording. When it is the first terminal operation it
// interrupts a pending Write, kills the encoder and removes the output file,
// even if frames were already written and the encoder was healthy; use Close to
// keep a video. It returns nil unless the encoder could not be reaped or the
// file could not be removed. It is safe concurrently with Write and idempotent.
//
// If Abort arrives while a Close is still draining, it escalates that Close to
// a kill and the output is removed, unless the encoder had already exited: an
// exit observed first lets Close finish and keep the video. Close's result is
// authoritative in that race, and the Abort call itself then returns nil. Abort
// after Close has finished is a no-op.
func (w *Writer) Abort() error { return w.terminate(true) }

func (w *Writer) terminate(abort bool) error {
	w.mu.Lock()
	first := false
	switch w.state.Load() {
	case stateIdle:
		w.state.Store(stateClosed)
		close(w.termDone)
		w.mu.Unlock()
		return nil
	case stateRunning:
		w.state.Store(stateClosing)
		first = true
	}
	w.mu.Unlock()

	if abort {
		w.abortOnce.Do(func() { close(w.abortCh) })
		w.interrupt(ports.ErrClosed)
	}
	if first {
		w.termErr = w.shutdown(abort)
		w.state.Store(stateClosed)
		close(w.termDone)
	} else {
		<-w.termDone
	}
	if abort && !first {
		return nil // the first terminal call owns the result
	}
	return w.termErr
}

func (w *Writer) shutdown(abort bool) error {
	w.watchMu.Lock()
	w.watchDead = true
	for _, stop := range []func() bool{w.startStop, w.writeStop} {
		if stop != nil {
			stop()
		}
	}
	w.watchMu.Unlock()

	_ = w.stdin.Close() // EOF: the encoder finalizes the file

	if abort {
		return w.discard()
	}
	err := w.awaitExit()
	if err != nil {
		_ = w.cmd.Process.Kill() // ErrProcessDone is harmless
		kt := time.NewTimer(killTimeout)
		defer kt.Stop()
		select {
		case <-w.waitDone:
		case <-kt.C:
			err = errors.Join(err, errors.New("ffmpeg: encoder not reaped after kill"))
		}
	}
	if err == nil {
		select {
		case <-w.waitDone:
			if w.waitErr != nil {
				err = fmt.Errorf("ffmpeg: encoder failed%s", w.diagnostics())
			}
		default:
		}
	} else {
		err = fmt.Errorf("%w%s", err, w.diagnostics())
	}
	if c := w.cause.Load(); err == nil && c != nil && c.fatal {
		// The stream broke mid-recording even though the encoder exited cleanly.
		err = c.err
	}
	if err == nil && w.frames.Load() == 0 {
		err = ErrNoFrames
	}
	if err != nil {
		err = errors.Join(err, w.removeOutput())
	}
	return err
}

// awaitExit waits for the encoder to exit after EOF. It returns nil if the
// process has exited, else the reason the wait was cut short (timeout or
// Abort). An exit that is already visible wins over a timeout or Abort that
// became ready at the same time; the remaining race is inherent and documented
// on Abort.
func (w *Writer) awaitExit() error {
	timer := time.NewTimer(w.closeTimeout)
	defer timer.Stop()
	var err error
	select {
	case <-w.waitDone:
		return nil
	case <-timer.C:
		err = fmt.Errorf("ffmpeg: encoder did not finish within %s; killed", w.closeTimeout)
	case <-w.abortCh:
		err = errors.New("ffmpeg: shutdown aborted; encoder killed")
	}
	select {
	case <-w.waitDone:
		return nil
	default:
		return err
	}
}

// discard kills the encoder, waits for the reaper and removes the output.
func (w *Writer) discard() error {
	_ = w.cmd.Process.Kill() // ErrProcessDone is harmless
	var err error
	kt := time.NewTimer(killTimeout)
	defer kt.Stop()
	select {
	case <-w.waitDone:
	case <-kt.C:
		err = errors.New("ffmpeg: encoder not reaped after kill")
	}
	return errors.Join(err, w.removeOutput())
}

// removeOutput deletes the file Start reserved, but not a replacement.
func (w *Writer) removeOutput() error {
	if err := w.res.Remove(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// boundedBuffer keeps the first limit bytes written and discards the rest, so
// a chatty encoder cannot grow memory.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
		if room < len(p) {
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(bytes.TrimSpace(b.buf))
	if b.truncated {
		s += " [truncated]"
	}
	return s
}
