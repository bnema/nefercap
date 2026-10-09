package app

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/adapters/config"
	"github.com/bnema/nefercap/internal/ports"
)

// Only selectors that can take a screenshot freeze the screen; rec records the
// live screen, and selector.freeze = off keeps every selector live.
func TestFreezes(t *testing.T) {
	on, off := config.Settings{Freeze: true}, config.Settings{}
	for _, tc := range []struct {
		command  cli.Command
		settings config.Settings
		want     bool
	}{
		{cli.Shot, on, true},
		{cli.AllInOne, on, true},
		{cli.Rec, on, false},
		{cli.Shot, off, false},
		{cli.AllInOne, off, false},
	} {
		assert.Equal(t, tc.want, freezes(cli.Options{Command: tc.command}, tc.settings), "%s freeze=%v", tc.command, tc.settings.Freeze)
	}
}

// Outputs too large for a backdrop are not frozen, so their screenshot is
// live like their selector.
func TestFrozenOutputs(t *testing.T) {
	outputs := []ports.Output{{ID: 7, Width: 3840, Height: 2160}, {ID: 8, Width: 5120, Height: 2880}, {ID: 9, Width: 1080, Height: 1920}, {ID: 10}}
	assert.Equal(t, []uint32{7, 9}, frozenOutputs(outputs))
}
