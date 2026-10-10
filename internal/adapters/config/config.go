// Package config reads nefercap's persistent settings.
package config

import (
	"errors"
	"fmt"
	"os"
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

const knownKeys = "selector.grid, selector.freeze, screenshot.dir, screenshot.output, video.dir"

// Strict: a line without "=", an unknown key or a duplicate is fatal. A "#"
// only starts a comment at the start of a line, and values are never unquoted.
var options = kvconf.Options{Strict: true, Comments: kvconf.CommentLine, RawValues: true, MaxBytes: maxBytes}

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
		return kvconf.ValueError("must be a non-empty path")
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
	settings := Settings{Grid: true, Freeze: true, Output: OutputFile}
	path, err := kvconf.DefaultPath("nefercap", "config")
	if err != nil {
		return settings, nil
	}
	data, err := kvconf.ReadFile(path, options)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return settings, nil
	case errors.Is(err, kvconf.ErrTooLarge):
		return Settings{}, fmt.Errorf("config %s exceeds %d bytes", path, maxBytes)
	case errors.Is(err, kvconf.ErrNotRegular):
		return Settings{}, fmt.Errorf("config %s: expected a regular file", path)
	case err != nil:
		return Settings{}, fmt.Errorf("read config: %w", err)
	}
	f := file{Grid: true, Freeze: true, Output: OutputFile}
	if _, err := kvconf.Unmarshal(data, &f, options); err != nil {
		var w kvconf.Warning
		if !errors.As(err, &w) {
			return Settings{}, fmt.Errorf("config %s: %w", path, err)
		}
		msg := w.Key + " " + w.Msg
		if w.Key == "" || w.Msg == "unknown key" || strings.HasPrefix(w.Msg, "overrides line") {
			msg = "expected each of " + knownKeys + " at most once as key = value"
		}
		return Settings{}, fmt.Errorf("config %s line %d: %s", path, w.Line, msg)
	}
	return Settings{bool(f.Grid), bool(f.Freeze), string(f.ShotDir), string(f.VideoDir), f.Output}, nil
}
