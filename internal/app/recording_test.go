package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	source.EXPECT().Capture(ctx, target).RunAndReturn(func(context.Context, ports.Target) (ports.Frame, error) {
		n++
		if n == 5 {
			cancel()
			return ports.Frame{}, context.Canceled
		}
		return frame, nil
	})
	path := filepath.Join(t.TempDir(), "recording.mp4")
	if err := core.New(source, nil, ffmpeg.New()).Run(ctx, ports.Selection{Mode: ports.Record, Target: target, Path: path, Video: ports.VideoSettings{FPS: 30}}); err != nil {
		t.Fatal(err)
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
