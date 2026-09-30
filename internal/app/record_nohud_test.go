package app

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/control"
	"github.com/bnema/nefercap/internal/adapters/wayland"
	"github.com/bnema/nefercap/internal/adapters/wayland/testserver"
	"github.com/bnema/nefercap/internal/ports"
)

// Without NeferWL's extension recording shows no HUD, runs against a real
// (in-process) compositor and is stopped through the control socket, as
// `nefercap stop` or the shortcut do.
func TestRecordWithoutHUDStopsViaControlSocket(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	runtime := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(runtime, 0o700))
	t.Setenv("XDG_RUNTIME_DIR", runtime)

	ctx := context.Background()
	srv := testserver.Start(t, testserver.Config{
		Outputs: []testserver.OutputSpec{{Name: "TEST-1", Width: 64, Height: 48, Scale: 1}},
	})
	source, err := wayland.New(ctx, srv.Path)
	require.NoError(t, err)
	defer source.Close()
	require.False(t, source.Capabilities().Exclusion)
	outputs, err := source.Outputs(ctx)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "clip.mp4")
	sel := ports.Selection{
		Mode: ports.Record, Target: ports.Target{OutputID: outputs[0].ID}, Path: path,
		Video: ports.VideoSettings{FPS: 30},
	}
	type result struct {
		outcome recordingOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		o, err := recordInteractive(ctx, io.Discard, source, outputs, sel)
		done <- result{o, err}
	}()
	require.Eventually(t, func() bool {
		active, err := control.Query(ctx)
		return err == nil && active
	}, 5*time.Second, 10*time.Millisecond, "the recording listens on the control socket")
	time.Sleep(200 * time.Millisecond) // a few frames
	stopped, err := control.Stop(ctx)
	require.NoError(t, err)
	require.True(t, stopped)

	select {
	case r := <-done:
		require.NoError(t, r.err, "a user stop is success")
		require.True(t, r.outcome.Saved)
	case <-time.After(15 * time.Second):
		t.Fatal("recording did not stop")
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NotZero(t, info.Size())
}
