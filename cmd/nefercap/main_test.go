package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	if interrupted(ctx, context.Canceled) {
		t.Fatal("live invocation hid internal cancellation")
	}
	cancel()
	if !interrupted(ctx, context.Canceled) {
		t.Fatal("signal cancellation rejected")
	}
	for _, err := range []error{nil, context.DeadlineExceeded, fmt.Errorf("phase: %w", context.Canceled), errors.Join(context.Canceled, errors.New("cleanup failed"))} {
		if interrupted(ctx, err) {
			t.Fatalf("hid failure: %v", err)
		}
	}
}

func TestUsageExitCodes(t *testing.T) {
	original := os.Args
	defer func() { os.Args = original }()
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"nefercap", "--help"}, 0},
		{[]string{"nefercap", "unknown-command"}, 2},
		{[]string{"nefercap", "screenshot"}, 2},
	} {
		os.Args = tc.args
		if code := run(); code != tc.want {
			t.Fatalf("%v: exit %d, want %d", tc.args, code, tc.want)
		}
	}
}
