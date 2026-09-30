package selection

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/ports"
)

// TestSmokeSelect opens the real overlays. It needs a Wayland compositor with
// layer-shell and Vulkan plus an input driver, so it runs only when
// NEFERCAP_SELECTION_SMOKE names the expected result: "region" (on the first
// output), "monitor" (whole monitor of output number NEFERCAP_SELECTION_WANT,
// default 1) or "abort". NEFERCAP_SELECTION_OUTPUTS lists output names,
// comma-separated; output IDs are their 1-based positions.
func TestSmokeSelect(t *testing.T) {
	want := os.Getenv("NEFERCAP_SELECTION_SMOKE")
	if want == "" {
		t.Skip("set NEFERCAP_SELECTION_SMOKE=region|monitor|abort on a Wayland session")
	}
	var outs []ports.Output
	for i, name := range strings.Split(os.Getenv("NEFERCAP_SELECTION_OUTPUTS"), ",") {
		outs = append(outs, ports.Output{ID: uint32(i + 1), Name: name})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	peak := samplePeakRSS(ctx)
	sel, ok, err := New(ports.Record, ports.VideoSettings{}, nil, false).Select(ctx, outs)
	cancel()
	fmt.Printf("SMOKE outputs=%d peak_rss_kib=%d\n", len(outs), <-peak)
	require.NoError(t, err)
	switch want {
	case "abort":
		assert.False(t, ok)
	case "monitor":
		n, _ := strconv.Atoi(os.Getenv("NEFERCAP_SELECTION_WANT"))
		n = max(n, 1)
		require.True(t, ok)
		assert.Equal(t, ports.Target{OutputID: uint32(n)}, sel.Target)
		assert.Equal(t, DefaultFPS, sel.Video.FPS)
	case "region":
		require.True(t, ok)
		assert.Equal(t, ports.Target{OutputID: 1, Region: ports.Region{X: 100, Y: 80, Width: 200, Height: 120}}, sel.Target)
	}
}

// samplePeakRSS reports the highest VmRSS of this process seen until ctx ends.
func samplePeakRSS(ctx context.Context) <-chan int {
	out := make(chan int, 1)
	go func() {
		peak := 0
		for t := time.NewTicker(50 * time.Millisecond); ; {
			select {
			case <-ctx.Done():
				out <- peak
				return
			case <-t.C:
				peak = max(peak, vmRSS())
			}
		}
	}()
	return out
}

func vmRSS() int {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			n, _ := strconv.Atoi(strings.Fields(v)[0])
			return n
		}
	}
	return 0
}
