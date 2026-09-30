package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestComponent(t *testing.T) {
	var output bytes.Buffer
	ctx := With(context.Background(), &output, false)
	log := For(ctx, "capture")
	log.Info().Msg("capture started")
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["component"] != "capture" || entry["level"] != "info" || entry["message"] != "capture started" {
		t.Fatalf("unexpected entry: %v", entry)
	}
}

func TestDebugLevel(t *testing.T) {
	for _, debug := range []bool{false, true} {
		var output bytes.Buffer
		log := For(With(context.Background(), &output, debug), "app")
		log.Debug().Msg("debug enabled")
		if !debug {
			if output.Len() != 0 {
				t.Fatalf("info logger emitted debug: %s", output.Bytes())
			}
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["component"] != "app" || entry["level"] != "debug" || entry["message"] != "debug enabled" {
			t.Fatalf("unexpected debug entry: %v", entry)
		}
	}
}
