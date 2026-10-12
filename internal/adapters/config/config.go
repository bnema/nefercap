// Package config reads nefercap's persistent settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/bnema/kvconf"
)

const (
	maxBytes = 4096
	maxPath  = 4096
)

// ScreenshotOutput says where an interactive screenshot goes.
type ScreenshotOutput string

const (
	OutputFile          ScreenshotOutput = "file"
	OutputFileClipboard ScreenshotOutput = "file+clipboard"
	OutputClipboard     ScreenshotOutput = "clipboard"
)

// Settings holds the persistent configuration. G toggles Grid for the current
// selection without changing the saved setting. Freeze shows a still copy of
// the screen while a screenshot is selected, and captures that copy. The
// directories are absolute, or empty for the XDG default.
type Settings struct {
	Grid          bool
	Freeze        bool
	ScreenshotDir string
	VideoDir      string
	Output        ScreenshotOutput
}

var keys = [...]string{"selector.grid", "selector.freeze", "screenshot.dir", "screenshot.output", "video.dir"}

var knownKeys = strings.Join(keys[:], ", ")

// A "#" only starts a comment at the start of a line, and values are never
// unquoted. Every problem is collected, so Load can fail on the first one in
// line order: a line without "=", an unknown key, a duplicate or a bad value.
var options = kvconf.Options{Comments: kvconf.CommentLine, RawValues: true, MaxBytes: maxBytes}

// defaults are the settings of an absent file or key.
var defaults = Settings{Grid: true, Freeze: true, Output: OutputFile}

// file is the config file as kvconf decodes it, before it becomes Settings.
type file struct {
	Grid     onOff            `kv:"selector.grid"`
	Freeze   onOff            `kv:"selector.freeze"`
	ShotDir  dirPath          `kv:"screenshot.dir"`
	VideoDir dirPath          `kv:"video.dir"`
	Output   ScreenshotOutput `kv:"screenshot.output" oneof:"file|file+clipboard|clipboard"`
}

// onOff accepts only "on" and "off" (kvconf's bool also takes true/yes/1).
type onOff bool

func (o *onOff) UnmarshalText(text []byte) error {
	switch string(text) {
	case "on":
		*o = true
	case "off":
		*o = false
	default:
		return kvconf.ValueError("must be on or off")
	}
	return nil
}

// dirPath is a kvconf.Path that refuses a bare "~" and paths over maxPath.
type dirPath string

func (d *dirPath) UnmarshalText(text []byte) error {
	if len(text) > maxPath {
		return kvconf.ValueError(fmt.Sprintf("must be at most %d bytes", maxPath))
	}
	if string(text) == "~" {
		return kvconf.ValueError("must be an absolute path or start with ~/")
	}
	var p kvconf.Path
	if err := p.UnmarshalText(text); err != nil {
		return err
	}
	*d = dirPath(p)
	return nil
}

// Load reads $XDG_CONFIG_HOME/nefercap/config (or ~/.config/nefercap/config).
// An absent file or setting enables the full monitor grid and the frozen
// screenshot selector. Invalid settings fail explicitly rather than silently
// changing the requested appearance.
func Load() (Settings, error) {
	path, err := kvconf.DefaultPath("nefercap", "config")
	if err != nil {
		return defaults, nil
	}
	data, err := kvconf.ReadFile(path, options)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return defaults, nil
	case errors.Is(err, kvconf.ErrTooLarge):
		return Settings{}, fmt.Errorf("config %s exceeds %d bytes", path, maxBytes)
	case errors.Is(err, kvconf.ErrNotRegular):
		return Settings{}, fmt.Errorf("config %s: expected a regular file", path)
	case err != nil:
		return Settings{}, fmt.Errorf("read config: %w", err)
	}
	entries, syntax, err := kvconf.ParseBytes(data, options)
	if err != nil {
		return Settings{}, fmt.Errorf("config %s: %w", path, err)
	}
	f := file{Grid: onOff(defaults.Grid), Freeze: onOff(defaults.Freeze), Output: defaults.Output}
	values, err := kvconf.Decode(entries, &f, options)
	if err != nil {
		return Settings{}, fmt.Errorf("config %s: %w", path, err)
	}
	// Fail on the first problem in line order. A line without "=" or a
	// duplicate (from ParseBytes) wins a tie, as a repeated key is wrong
	// whatever its value.
	var first *kvconf.Warning
	generic := false
	if len(syntax) > 0 {
		first, generic = &syntax[0], true
	}
	if len(values) > 0 && (first == nil || values[0].Line < first.Line) {
		first, generic = &values[0], !slices.Contains(keys[:], values[0].Key)
	}
	if first != nil {
		msg := first.Key + " " + first.Msg
		if generic {
			msg = "expected each of " + knownKeys + " at most once as key = value"
		}
		return Settings{}, fmt.Errorf("config %s line %d: %s", path, first.Line, msg)
	}
	return Settings{
		Grid:          bool(f.Grid),
		Freeze:        bool(f.Freeze),
		ScreenshotDir: string(f.ShotDir),
		VideoDir:      string(f.VideoDir),
		Output:        f.Output,
	}, nil
}
