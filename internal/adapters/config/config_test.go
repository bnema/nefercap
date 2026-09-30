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
		{"selector.grid", "line 1: expected each of"},
		{"selector.grid =", "line 1: selector.grid must be on or off"},
		{"selector.other = on", "line 1: expected each of"},
		{"selector.grid = on\nselector.grid = off", "line 2: expected each of"},
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

func TestLoadPathsAndOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := configRoot(t)
	saveConfig(t, path, "# c\nscreenshot.dir = ~/Pics/Shots\nvideo.dir=/var/tmp/vids/\nscreenshot.output = file+clipboard\nselector.grid = off\n")
	got, err := Load()
	require.NoError(t, err)
	assert.Equal(t, Settings{Grid: false, ScreenshotDir: filepath.Join(home, "Pics", "Shots"), VideoDir: "/var/tmp/vids", Output: OutputFileClipboard}, got)

	saveConfig(t, path, "screenshot.dir = ~\nscreenshot.output = clipboard\n")
	got, err = Load()
	require.NoError(t, err)
	assert.Equal(t, home, got.ScreenshotDir)
	assert.Equal(t, OutputClipboard, got.Output)
	assert.Empty(t, got.VideoDir)
}

func TestLoadDefaultsOutputFile(t *testing.T) {
	configRoot(t)
	got, err := Load()
	require.NoError(t, err)
	assert.Equal(t, Settings{Grid: true, Output: OutputFile}, got)
}

func TestLoadRejectsInvalidNewKeys(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"screenshot.dir = Pictures", "line 1: screenshot.dir must be an absolute path or start with ~/"},
		{"video.dir = ./v", "line 1: video.dir must be an absolute path"},
		{"video.dir = ~user/v", "line 1: video.dir must be an absolute path"},
		{"video.dir =", "line 1: video.dir must be a non-empty path"},
		{"screenshot.output = both", "line 1: screenshot.output must be file, file+clipboard or clipboard"},
		{"screenshot.output = File", "screenshot.output must be"},
		{"screenshot.dir = /a\nscreenshot.dir = /b", "line 2: expected each of"},
		{"screenshot.output = file\n\nscreenshot.output = file", "line 3: expected each of"},
		{"video.dir = /a\nvideo.dir = /a", "line 2: expected each of"},
		{"video.output = file", "line 1: expected each of"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := configRoot(t)
			saveConfig(t, path, tc.text)
			_, err := Load()
			require.ErrorContains(t, err, tc.want)
			assert.Contains(t, err.Error(), path)
		})
	}
}
