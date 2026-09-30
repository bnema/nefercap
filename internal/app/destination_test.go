package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/adapters/config"
	"github.com/bnema/nefercap/internal/ports"
)

// A bad configured directory fails before the selector opens, for every
// directory the command can reach.
func TestCheckDirsBeforeSelector(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	good := filepath.Join(root, "good")
	for _, tc := range []struct {
		name     string
		options  cli.Options
		settings config.Settings
		wantKey  string // config key named in the error; empty means no error
	}{
		{"all-in-one bad video dir", cli.Options{Command: cli.AllInOne}, config.Settings{VideoDir: file, ScreenshotDir: good}, "video.dir"},
		{"all-in-one bad screenshot dir", cli.Options{Command: cli.AllInOne}, config.Settings{VideoDir: good, ScreenshotDir: file}, "screenshot.dir"},
		{"all-in-one clipboard skips screenshot dir", cli.Options{Command: cli.AllInOne}, config.Settings{VideoDir: good, ScreenshotDir: file, Output: config.OutputClipboard}, ""},
		{"shot ignores video dir", cli.Options{Command: cli.Shot}, config.Settings{VideoDir: file, ScreenshotDir: good}, ""},
		{"rec ignores screenshot dir", cli.Options{Command: cli.Rec}, config.Settings{VideoDir: good, ScreenshotDir: file}, ""},
		{"rec bad video dir", cli.Options{Command: cli.Rec}, config.Settings{VideoDir: file}, "video.dir"},
		{"explicit -file skips dirs", cli.Options{Command: cli.Shot, Path: "x.png"}, config.Settings{ScreenshotDir: file}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDirs(tc.options, tc.settings)
			if tc.wantKey != "" {
				assert.ErrorContains(t, err, tc.wantKey+":")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAssignScreenshotDestination(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shots")
	for _, tc := range []struct {
		name      string
		output    config.ScreenshotOutput
		options   cli.Options
		wantFile  bool // a file in dir
		wantPath  string
		clipboard bool
	}{
		{"file default", config.OutputFile, cli.Options{}, true, "", false},
		{"file+clipboard", config.OutputFileClipboard, cli.Options{}, true, "", true},
		{"clipboard only", config.OutputClipboard, cli.Options{}, false, "", true},
		{"-file overrides clipboard config", config.OutputClipboard, cli.Options{Path: "x.png"}, false, "x.png", false},
		{"-file overrides file+clipboard", config.OutputFileClipboard, cli.Options{Path: "x.png"}, false, "x.png", false},
		{"-clipboard overrides file config", config.OutputFile, cli.Options{Clipboard: true}, false, "", true},
		{"-clipboard -file", config.OutputFile, cli.Options{Clipboard: true, Path: "x.png"}, false, "x.png", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := ports.Selection{Mode: ports.Screenshot}
			require.NoError(t, assignDestination(&sel, tc.options, config.Settings{Output: tc.output, ScreenshotDir: dir}))
			assert.Equal(t, tc.clipboard, sel.Clipboard)
			if tc.wantFile {
				assert.Equal(t, dir, filepath.Dir(sel.Path))
				assert.True(t, strings.HasSuffix(sel.Path, ".png"))
			} else {
				assert.Equal(t, tc.wantPath, sel.Path)
			}
		})
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestAssignVideoDestination(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vids")
	settings := config.Settings{Output: config.OutputClipboard, VideoDir: dir}
	sel := ports.Selection{Mode: ports.Record}
	require.NoError(t, assignDestination(&sel, cli.Options{}, settings))
	assert.Equal(t, dir, filepath.Dir(sel.Path), "video.dir applies; screenshot.output does not")
	assert.True(t, strings.HasSuffix(sel.Path, ".mp4"))
	assert.False(t, sel.Clipboard)
	sel = ports.Selection{Mode: ports.Record}
	require.NoError(t, assignDestination(&sel, cli.Options{Path: "v.mp4"}, settings))
	assert.Equal(t, "v.mp4", sel.Path)
}
