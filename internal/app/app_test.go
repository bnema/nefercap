package app

import (
	"context"
	"errors"
	"testing"

	"github.com/bnema/nefercap/internal/core"
	portsmocks "github.com/bnema/nefercap/internal/mocks/ports"
	"github.com/bnema/nefercap/internal/ports"
)

func TestScreenshotWithoutVideoWriter(t *testing.T) {
	ctx := context.Background()
	target := ports.Target{OutputID: 1}
	frame := ports.Frame{Pixels: make([]byte, 16), Width: 2, Height: 2, Stride: 8, Format: ports.XRGB8888}
	source := portsmocks.NewMockSource(t)
	writer := portsmocks.NewMockScreenshotWriter(t)
	source.EXPECT().Capture(ctx, target).Return(frame, nil).Once()
	writer.EXPECT().Save(ctx, frame, "shot.png").Return(nil).Once()
	if err := core.New(source, writer, nil).Run(ctx, ports.Selection{Mode: ports.Screenshot, Target: target, Path: "shot.png"}); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadline := discoveryError(ctx, "list outputs", context.DeadlineExceeded)
	if !errors.Is(deadline, context.DeadlineExceeded) || deadline == context.DeadlineExceeded {
		t.Fatalf("timeout lost phase: %v", deadline)
	}
	cancel()
	if err := discoveryError(ctx, "connect Wayland", context.Canceled); err != context.Canceled {
		t.Fatalf("signal wrapped: %v", err)
	}
	real := errors.New("protocol failure")
	if err := discoveryError(ctx, "list outputs", real); !errors.Is(err, real) || err == real {
		t.Fatalf("failure lost phase: %v", err)
	}
}

func TestResolveOutput(t *testing.T) {
	outputs := []ports.Output{{ID: 1, Name: "DP-1"}, {ID: 2, Name: "HDMI-A-1"}}
	got, err := resolveOutput(outputs, "HDMI-A-1")
	if err != nil || got.ID != 2 {
		t.Fatalf("named output: %v, %v", got, err)
	}
	if _, err := resolveOutput(outputs, ""); err == nil {
		t.Fatal("ambiguous default accepted")
	}
	if _, err := resolveOutput(outputs, "missing"); !errors.Is(err, ports.ErrOutputNotFound) {
		t.Fatalf("missing output: %v", err)
	}
	if _, err := resolveOutput(nil, ""); err == nil {
		t.Fatal("empty default accepted")
	}
	got, err = resolveOutput(outputs[:1], "")
	if err != nil || got.ID != 1 {
		t.Fatalf("single output: %v, %v", got, err)
	}
}

func TestResolveOutputAllocations(t *testing.T) {
	outputs := []ports.Output{{ID: 1, Name: "DP-1"}}
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := resolveOutput(outputs, "DP-1"); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("output lookup: %g allocations", allocs)
	}
}
