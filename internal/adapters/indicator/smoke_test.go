package indicator

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bnema/wlturbo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

// TestSmokeRun shows the real HUD. It needs a Wayland compositor with
// layer-shell and Vulkan, so it runs only when NEFERCAP_INDICATOR_SMOKE is
// "click" (an input driver presses Stop) or "cancel" (the context ends).
// NEFERCAP_INDICATOR_OUTPUT names the wl_output to show it on (for example
// HEADLESS-1 on a NeferWL headless compositor) and is required with it.
func TestSmokeRun(t *testing.T) {
	mode := os.Getenv("NEFERCAP_INDICATOR_SMOKE")
	if mode == "" {
		t.Skip("set NEFERCAP_INDICATOR_SMOKE=click|cancel on a Wayland session")
	}
	name := os.Getenv("NEFERCAP_INDICATOR_OUTPUT")
	if name == "" {
		t.Skip("set NEFERCAP_INDICATOR_OUTPUT to the output name")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stop := make(chan struct{}, 1)
	var authorized bool
	ind := New(func(_ context.Context, display *wlturbo.Display, surface wlturbo.Proxy) error {
		authorized = display != nil && surface != nil && surface.Context() == display.Context()
		return nil
	}, stop)
	done := make(chan error, 1)
	go func() { done <- ind.Run(ctx, ports.Output{ID: 1, Name: name}) }()
	switch mode {
	case "click":
		select {
		case <-stop:
		case err := <-done:
			t.Fatalf("Run ended before Stop: %v", err)
		case <-ctx.Done():
			t.Fatal("Stop was never requested")
		}
	case "cancel":
		time.Sleep(3 * time.Second)
	}
	cancel()
	require.NoError(t, <-done)
	assert.True(t, authorized, "authorize must run with the live surface")
}
