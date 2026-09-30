package uistyle

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteStoresPrivateFileAndRemoves(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	path, remove, err := Write("app { color: red; }", "uistyle-test-*.css")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "app { color: red; }", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	remove()
	remove() // idempotent
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestWriteFailsWithoutTempDir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()+"/missing")
	path, remove, err := Write("x", "uistyle-test-*.css")
	assert.Error(t, err)
	assert.Empty(t, path)
	assert.Nil(t, remove)
}
