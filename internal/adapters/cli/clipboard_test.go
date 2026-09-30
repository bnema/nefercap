package cli

import (
	"github.com/stretchr/testify/require"
	"io"
	"testing"
)

func TestClipboardFlag(t *testing.T) {
	for _, cmd := range []string{"shot", "screenshot"} {
		opts, err := Parse([]string{cmd, "-clipboard"}, io.Discard)
		require.NoError(t, err)
		require.True(t, opts.Clipboard)
		require.Empty(t, opts.Path)
		opts, err = Parse([]string{cmd, "-clipboard", "-file", "shot.png"}, io.Discard)
		require.NoError(t, err)
		require.True(t, opts.Clipboard)
		require.Equal(t, "shot.png", opts.Path)
		_, err = Parse([]string{cmd, "-clipboard", "-file", ""}, io.Discard)
		require.ErrorIs(t, err, ErrUsage)
	}
	for _, cmd := range []string{"rec", "record", "stop", "status", "outputs"} {
		_, err := Parse([]string{cmd, "-clipboard"}, io.Discard)
		require.ErrorIs(t, err, ErrUsage)
	}
}
