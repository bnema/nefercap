package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

func TestVersion(t *testing.T) {
	original := version
	defer func() { version = original }()

	var out, errOut bytes.Buffer
	if code := printVersion(nil, &out, &errOut); code != 0 || out.String() != "dev\n" || errOut.Len() != 0 {
		t.Fatalf("default: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	version = "1.2.3"
	out.Reset()
	if code := printVersion(nil, &out, &errOut); code != 0 || out.String() != "1.2.3\n" {
		t.Fatalf("set: exit %d, stdout %q", code, out.String())
	}
	out.Reset()
	if code := printVersion([]string{"extra"}, &out, &errOut); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), `"extra"`) {
		t.Fatalf("extra: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

func TestVersionCommandRuns(t *testing.T) {
	original := os.Args
	defer func() { os.Args = original }()
	os.Args = []string{"nefercap", "version"}
	if code := run(); code != 0 {
		t.Fatalf("version: exit %d", code)
	}
	os.Args = []string{"nefercap", "version", "x"}
	if code := run(); code != 2 {
		t.Fatalf("version x: exit %d, want 2", code)
	}
}
