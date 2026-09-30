package app

import (
	"bytes"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestClipboardSavedPathOnFailure(t *testing.T) {
	failure := errors.New("wl-copy failed")
	var out bytes.Buffer
	require.ErrorIs(t, finishCapture(&out, "shot.png", true, failure), failure)
	require.Equal(t, "shot.png\n", out.String())
	out.Reset()
	require.ErrorIs(t, finishCapture(&out, "", false, failure), failure)
	require.Empty(t, out.String())
}
