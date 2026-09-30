package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bnema/nefercap/internal/adapters/ffmpeg"
	"github.com/bnema/nefercap/internal/core"
	portsmocks "github.com/bnema/nefercap/internal/mocks/ports"
	"github.com/bnema/nefercap/internal/ports"
)

// A generated source mock supplies pixels; the production core and FFmpeg
// writer run together, exercising cancellation at their owning boundary.
func TestRecordingFinalizesAfterCancellation(t *testing.T) {
	testRecordingFinalizes(t, "cancel")
}

func TestRecordingFinalizesAfterLateCaptureFailure(t *testing.T) {
	testRecordingFinalizes(t, "capture")
}

func TestRecordingFinalizesAfterLateGeometryFailure(t *testing.T) {
	testRecordingFinalizes(t, "geometry")
}

func testRecordingFinalizes(t *testing.T, failure string) {
	t.Helper()
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip(binary + " unavailable")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frame := ports.Frame{Pixels: make([]byte, 64*48*4), Width: 64, Height: 48, Stride: 64 * 4, Format: ports.XRGB8888}
	for i := 0; i < len(frame.Pixels); i += 4 {
		frame.Pixels[i+2] = 255
	}
	target := ports.Target{OutputID: 1}
	source := portsmocks.NewMockSource(t)
	n := 0
	captureErr := errors.New("capture session lost")
	source.EXPECT().Capture(ctx, target).RunAndReturn(func(context.Context, ports.Target) (ports.Frame, error) {
		n++
		if n == 5 {
			switch failure {
			case "capture":
				return ports.Frame{}, captureErr
			case "geometry":
				return ports.Frame{}, nil
			default:
				cancel()
				return ports.Frame{}, context.Canceled
			}
		}
		return frame, nil
	})
	path := filepath.Join(t.TempDir(), "recording.mp4")
	video := ffmpeg.New()
	runErr := core.New(source, nil, video).Run(ctx, ports.Selection{Mode: ports.Record, Target: target, Path: path, Video: ports.VideoSettings{FPS: 30}})
	switch failure {
	case "capture":
		if !errors.Is(runErr, captureErr) {
			t.Fatalf("capture failure lost: %v", runErr)
		}
	case "geometry":
		if runErr == nil || !strings.Contains(runErr.Error(), "invalid frame dimensions") {
			t.Fatalf("geometry failure lost: %v", runErr)
		}
	default:
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	if !video.Saved {
		t.Fatal("finalized recording not reported as saved")
	}
	var out bytes.Buffer
	if err := finishCapture(&out, path, video.Saved, runErr); err != runErr || out.String() != path+"\n" {
		t.Fatalf("retained path or recording error lost: %q %v", out.String(), err)
	}
	probeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	data, err := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames", "-show_entries", "stream=codec_name,nb_read_frames", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Frames string `json:"nb_read_frames"`
		}
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Streams) != 1 || result.Streams[0].Codec != "h264" {
		t.Fatalf("unexpected probe: %s", data)
	}
	frames, err := strconv.Atoi(result.Streams[0].Frames)
	if err != nil || frames < 4 {
		t.Fatalf("complete frames missing: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
}
