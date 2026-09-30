package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	return filepath.Join(root, "nefercap", "config")
}

func saveConfig(t *testing.T, path, text string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
}

func TestLoadDefault(t *testing.T) {
	configRoot(t)
	got, err := Load()
	require.NoError(t, err)
	assert.True(t, got.Grid)
}

func TestLoadGrid(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		grid       bool
	}{
		{"empty", "", true},
		{"comments", "# selector settings\n\n", true},
		{"on", "selector.grid = on\n", true},
		{"off", "selector.grid = off\n", false},
		{"whitespace", "  selector.grid\t= off \n", false},
		{"CRLF", "# config\r\nselector.grid = off\r\n", false},
		{"BOM", "\ufeffselector.grid = off\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := configRoot(t)
			saveConfig(t, path, tc.text)
			got, err := Load()
			require.NoError(t, err)
			assert.Equal(t, tc.grid, got.Grid)
		})
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"selector.grid = maybe", "line 1: selector.grid must be on or off"},
		{"selector.grid", "line 1: expected one"},
		{"selector.grid =", "line 1: selector.grid must be on or off"},
		{"selector.other = on", "line 1: expected one"},
		{"selector.grid = on\nselector.grid = off", "line 2: expected one"},
		{strings.Repeat("#", maxBytes+1), "exceeds 4096 bytes"},
	} {
		t.Run(tc.text[:min(len(tc.text), 40)], func(t *testing.T) {
			path := configRoot(t)
			saveConfig(t, path, tc.text)
			_, err := Load()
			require.ErrorContains(t, err, tc.want)
			assert.Contains(t, err.Error(), path)
		})
	}
}

func TestLoadHomeFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	saveConfig(t, filepath.Join(home, ".config", "nefercap", "config"), "selector.grid = off\n")
	got, err := Load()
	require.NoError(t, err)
	assert.False(t, got.Grid)
}

func TestLoadRelativeXDGUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative-config")
	saveConfig(t, filepath.Join(home, ".config", "nefercap", "config"), "selector.grid = off\n")
	got, err := Load()
	require.NoError(t, err)
	assert.False(t, got.Grid)
}

func TestLoadNoConfigDirectoryUsesDefaults(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err := Load()
	require.NoError(t, err)
	assert.True(t, got.Grid)
}

func TestLoadFIFORejectedWithoutBlocking(t *testing.T) {
	path := configRoot(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	_, err := Load()
	require.ErrorContains(t, err, "expected a regular file")
}

func TestLoadSizeBoundary(t *testing.T) {
	path := configRoot(t)
	saveConfig(t, path, strings.Repeat("#", maxBytes))
	got, err := Load()
	require.NoError(t, err)
	assert.True(t, got.Grid)
}

func TestLoadDirectoryRejected(t *testing.T) {
	path := configRoot(t)
	require.NoError(t, os.MkdirAll(path, 0o700))
	_, err := Load()
	require.ErrorContains(t, err, "expected a regular file")
}
