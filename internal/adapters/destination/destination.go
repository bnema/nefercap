// Package destination chooses default capture locations.
//
// Screenshot and Video return a fresh, timestamped file path inside the user's
// default directory, which exists on return. The path is only a proposal: it is not
// reserved, so the writer must still create the file with O_EXCL (all capture
// writers do) and report a conflict instead of overwriting.
package destination

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	dirMode = 0o700
	// stampLayout has nanosecond precision so two captures started in the
	// same second get different names.
	stampLayout = "20060102-150405.000000000"
	prefix      = "nefercap-"
	maxPath     = 4096
	maxConfig   = 64 << 10 // user-dirs.dirs is a handful of short lines
)

// ErrNoHome means no absolute home directory is available for a default.
var ErrNoHome = errors.New("destination: no absolute home directory")

// Screenshot returns a new .png path in the user's XDG pictures directory
// (Screenshots subdirectory), falling back to $HOME/Pictures/Screenshots.
func Screenshot() (string, error) { return ScreenshotIn("") }

// ScreenshotIn is Screenshot inside dir, an absolute directory that is created
// when missing. An empty dir selects the XDG default.
func ScreenshotIn(dir string) (string, error) {
	if dir != "" {
		return propose(".png", func() (string, error) { return dir, nil })
	}
	return propose(".png", func() (string, error) {
		return userDir("XDG_PICTURES_DIR", "Pictures", "Screenshots")
	})
}

// Video returns a new .mp4 path in the user's XDG videos directory, falling
// back to $HOME/Videos.
func Video() (string, error) { return VideoIn("") }

// VideoIn is Video inside dir, an absolute directory that is created when
// missing. An empty dir selects the XDG default.
func VideoIn(dir string) (string, error) {
	if dir != "" {
		return propose(".mp4", func() (string, error) { return dir, nil })
	}
	return propose(".mp4", func() (string, error) {
		return userDir("XDG_VIDEOS_DIR", "Videos")
	})
}

func propose(ext string, auto func() (string, error)) (string, error) {
	dir, err := auto()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("destination: create directory: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("destination: stat directory: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("destination: %w: not a directory", os.ErrInvalid)
	}
	return filepath.Join(dir, prefix+time.Now().Format(stampLayout)+ext), nil
}

// userDir resolves the XDG user directory named by key: the environment
// variable first, then user-dirs.dirs, then $HOME/<fallback...>. Elements of
// fallback after the first (Screenshots) are also appended to an XDG-provided
// directory.
func userDir(key string, fallback ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", ErrNoHome
	}
	home = filepath.Clean(home)
	sub := fallback[1:]

	if d, ok := expand(os.Getenv(key), home); ok {
		return filepath.Join(append([]string{d}, sub...)...), nil
	}
	if d, ok := expand(fromConfig(key, home), home); ok {
		return filepath.Join(append([]string{d}, sub...)...), nil
	}
	return filepath.Join(append([]string{home}, fallback...)...), nil
}

// expand accepts an absolute path, optionally starting with $HOME or ${HOME}.
// No shell, no other variables, no command substitution. The home directory
// itself is rejected: user-dirs uses it to mean "disabled".
func expand(v, home string) (string, bool) {
	if v == "" || len(v) > maxPath || strings.IndexByte(v, 0) >= 0 {
		return "", false
	}
	switch {
	case v == "$HOME" || strings.HasPrefix(v, "$HOME/"):
		v = home + v[len("$HOME"):]
	case v == "${HOME}" || strings.HasPrefix(v, "${HOME}/"):
		v = home + v[len("${HOME}"):]
	}
	if strings.ContainsAny(v, "$`") || !filepath.IsAbs(v) {
		return "", false
	}
	v = filepath.Clean(v)
	if v == home || v == string(filepath.Separator) {
		return "", false
	}
	return v, true
}

// fromConfig reads KEY="value" from the user-dirs.dirs file, bounded in size.
func fromConfig(key, home string) string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(cfg) {
		cfg = filepath.Join(home, ".config")
	}
	f, err := os.Open(filepath.Join(cfg, "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxConfig))
	sc.Buffer(make([]byte, 0, 1024), maxPath+256)
	var val string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		val = unquote(strings.TrimSpace(v)) // the last assignment wins
	}
	return val
}

// unquote handles the double-quoted, backslash-escaped form written by
// xdg-user-dirs-update, and bare values.
func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' {
		return v
	}
	var b strings.Builder
	for i := 1; i < len(v); i++ {
		switch c := v[i]; c {
		case '\\':
			if i+1 < len(v) {
				i++
				b.WriteByte(v[i])
			}
		case '"':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return "" // unterminated
}
