package gui

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bnema/nefergui"
)

// TestSmokeRunFrames opens the real panel for a few frames. It needs a Wayland
// session with Vulkan, so it runs only when NEFERCAP_GUI_SMOKE=1.
func TestSmokeRunFrames(t *testing.T) {
	if os.Getenv("NEFERCAP_GUI_SMOKE") != "1" {
		t.Skip("set NEFERCAP_GUI_SMOKE=1 on a Wayland session to open the panel")
	}
	m := newTestModel(t)
	m.mode = modeRecord
	sheet, remove, err := writeStyleSheet()
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := nefergui.RunFrames(ctx, 3, m, view, nil,
		nefergui.Title("NeferCap smoke"), nefergui.Size(640, m.windowHeight()), nefergui.Styles(sheet)); err != nil {
		t.Fatal(err)
	}
}
