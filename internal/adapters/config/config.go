// Package config reads nefercap's persistent settings.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
// selection without changing the saved setting. The directories are absolute,
// or empty for the XDG default.
type Settings struct {
	Grid          bool
	ScreenshotDir string
	VideoDir      string
	Output        ScreenshotOutput
}

const knownKeys = "selector.grid, screenshot.dir, screenshot.output, video.dir"

// Load reads $XDG_CONFIG_HOME/nefercap/config (or ~/.config/nefercap/config).
// An absent file or setting enables the full monitor grid. Invalid settings
// fail explicitly rather than silently changing the requested appearance.
func Load() (Settings, error) {
	settings := Settings{Grid: true, Output: OutputFile}
	root := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil {
			return settings, nil
		}
		root = filepath.Join(home, ".config")
	}
	path := filepath.Join(root, "nefercap", "config")
	// Nonblocking open also protects against a FIFO substituted before Stat.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Settings{}, fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Settings{}, fmt.Errorf("config %s: expected a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return Settings{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if len(data) > maxBytes {
		return Settings{}, fmt.Errorf("config %s exceeds %d bytes", path, maxBytes)
	}
	var seen [4]bool
	for index, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		slot := -1
		if ok {
			slot = slices.Index(keys[:], key)
		}
		if slot < 0 || seen[slot] {
			return Settings{}, fmt.Errorf("config %s line %d: expected each of %s at most once as key = value", path, index+1, knownKeys)
		}
		seen[slot] = true
		if err := apply(&settings, key, value); err != nil {
			return Settings{}, fmt.Errorf("config %s line %d: %w", path, index+1, err)
		}
	}
	return settings, nil
}

var keys = [...]string{"selector.grid", "screenshot.dir", "screenshot.output", "video.dir"}

func apply(s *Settings, key, value string) (err error) {
	switch key {
	case "selector.grid":
		switch value {
		case "on":
			s.Grid = true
		case "off":
			s.Grid = false
		default:
			return errors.New("selector.grid must be on or off")
		}
	case "screenshot.output":
		switch out := ScreenshotOutput(value); out {
		case OutputFile, OutputFileClipboard, OutputClipboard:
			s.Output = out
		default:
			return errors.New("screenshot.output must be file, file+clipboard or clipboard")
		}
	case "screenshot.dir":
		s.ScreenshotDir, err = dir(key, value)
	case "video.dir":
		s.VideoDir, err = dir(key, value)
	}
	return err
}

// dir accepts an absolute path or one starting with ~/ (the home directory).
func dir(key, value string) (string, error) {
	if value == "" || len(value) > maxPath || strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("%s must be a non-empty path", key)
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", fmt.Errorf("%s: no absolute home directory for ~", key)
		}
		value = filepath.Join(home, value[1:])
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute path or start with ~/", key)
	}
	return filepath.Clean(value), nil
}
