// Package config reads the selector's persistent settings.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxBytes = 4096

// Settings controls the selector's initial appearance. G toggles Grid for
// the current selection without changing the saved setting.
type Settings struct {
	Grid bool
}

// Load reads $XDG_CONFIG_HOME/nefercap/config (or ~/.config/nefercap/config).
// An absent file or setting enables the full monitor grid. Invalid settings
// fail explicitly rather than silently changing the requested appearance.
func Load() (Settings, error) {
	settings := Settings{Grid: true}
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
		return Settings{}, fmt.Errorf("open selector config: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Settings{}, fmt.Errorf("stat selector config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Settings{}, fmt.Errorf("selector config %s: expected a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return Settings{}, fmt.Errorf("read selector config %s: %w", path, err)
	}
	if len(data) > maxBytes {
		return Settings{}, fmt.Errorf("selector config %s exceeds %d bytes", path, maxBytes)
	}
	seen := false
	for index, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "selector.grid" || seen {
			return Settings{}, fmt.Errorf("selector config %s line %d: expected one selector.grid = on/off setting", path, index+1)
		}
		switch strings.TrimSpace(value) {
		case "on":
			settings.Grid = true
		case "off":
			settings.Grid = false
		default:
			return Settings{}, fmt.Errorf("selector config %s line %d: selector.grid must be on or off", path, index+1)
		}
		seen = true
	}
	return settings, nil
}
