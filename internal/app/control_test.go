package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/adapters/control"
)

func TestControlCommands(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	var out bytes.Buffer
	ctx := context.Background()
	if handled, err := controlCommand(ctx, cli.Status, &out); !handled || err != nil || out.String() != "idle\n" {
		t.Fatalf("idle: %t %v %s", handled, err, out.String())
	}
	if handled, err := controlCommand(ctx, cli.Rec, &out); handled || err != nil {
		t.Fatalf("idle rec: %t %v", handled, err)
	}
	server, err := control.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	out.Reset()
	if handled, err := controlCommand(ctx, cli.Status, &out); !handled || err != nil || out.String() != "recording\n" {
		t.Fatalf("active: %t %v %s", handled, err, out.String())
	}
	if handled, err := controlCommand(ctx, cli.Rec, &out); !handled || err != nil {
		t.Fatalf("toggle: %t %v", handled, err)
	}
	select {
	case got := <-server.Requests():
		if got != control.RequestStop {
			t.Fatalf("request: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("stop not delivered")
	}
}

func TestStopOnRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan control.Request, 1)
	done := stopOnRequest(ctx, requests, cancel)
	requests <- control.RequestStop
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("control waiter did not stop")
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("stop did not cancel recording")
	}
}
