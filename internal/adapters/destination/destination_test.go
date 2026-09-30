package destination

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// env gives each test an isolated home and config with no XDG overrides.
func env(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_PICTURES_DIR", "")
	t.Setenv("XDG_VIDEOS_DIR", "")
	return home
}

func writeDirs(t *testing.T, content string) {
	t.Helper()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	require.NoError(t, os.MkdirAll(cfg, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cfg, "user-dirs.dirs"), []byte(content), 0o600))
}

var nameRE = regexp.MustCompile(`^nefercap-\d{8}-\d{6}\.\d{9}\.(png|mp4)$`)

func TestFallbackDefaults(t *testing.T) {
	home := env(t)

	shot, err := Screenshot()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Pictures", "Screenshots"), filepath.Dir(shot))
	assert.Regexp(t, nameRE, filepath.Base(shot))
	assert.Equal(t, ".png", filepath.Ext(shot))

	vid, err := Video()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Videos"), filepath.Dir(vid))
	assert.Equal(t, ".mp4", filepath.Ext(vid))
	assert.Regexp(t, nameRE, filepath.Base(vid))

	for _, d := range []string{filepath.Dir(shot), filepath.Dir(vid)} {
		fi, err := os.Stat(d)
		require.NoError(t, err)
		assert.True(t, fi.IsDir())
	}
	_, err = os.Lstat(shot)
	assert.True(t, os.IsNotExist(err), "the file itself is not created")
}

func TestDirectoryMode(t *testing.T) {
	home := env(t)
	old := umaskZero()
	defer old()
	shot, err := Screenshot()
	require.NoError(t, err)
	fi, err := os.Stat(filepath.Dir(shot))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	fi, err = os.Stat(filepath.Join(home, "Pictures"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
}

func TestEnvironmentDirectories(t *testing.T) {
	home := env(t)
	pics := t.TempDir()
	vids := filepath.Join(home, "Movies")
	t.Setenv("XDG_PICTURES_DIR", pics)
	t.Setenv("XDG_VIDEOS_DIR", "$HOME/Movies")

	shot, err := Screenshot()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(pics, "Screenshots"), filepath.Dir(shot))

	vid, err := Video()
	require.NoError(t, err)
	assert.Equal(t, vids, filepath.Dir(vid))

	t.Setenv("XDG_VIDEOS_DIR", "${HOME}/Clips/../Movies2")
	vid, err = Video()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Movies2"), filepath.Dir(vid))
}

func TestConfigFile(t *testing.T) {
	home := env(t)
	writeDirs(t, `# comment
XDG_DESKTOP_DIR="$HOME/Desktop"
XDG_PICTURES_DIR="$HOME/My \"Pics\""
XDG_VIDEOS_DIR="$HOME/Films"
`)
	shot, err := Screenshot()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, `My "Pics"`, "Screenshots"), filepath.Dir(shot))
	vid, err := Video()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Films"), filepath.Dir(vid))

	// The environment variable wins over the file.
	other := t.TempDir()
	t.Setenv("XDG_VIDEOS_DIR", other)
	vid, err = Video()
	require.NoError(t, err)
	assert.Equal(t, other, filepath.Dir(vid))
}

func TestConfigFileDefaultLocation(t *testing.T) {
	home := env(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".config", "user-dirs.dirs"),
		[]byte("XDG_VIDEOS_DIR=\"$HOME/Reels\"\n"), 0o600))
	vid, err := Video()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Reels"), filepath.Dir(vid))
}

func TestUnsafeConfiguredValuesFallBack(t *testing.T) {
	home := env(t)
	for _, v := range []string{
		"relative/dir",
		"$OTHER/dir",
		"$(id)/x",
		"`id`",
		"$HOME", // xdg-user-dirs uses HOME to mean "disabled"
		"$HOME/",
		"/",
		"$HOMEDIR/x",
	} {
		t.Setenv("XDG_VIDEOS_DIR", v)
		vid, err := Video()
		require.NoError(t, err, v)
		assert.Equal(t, filepath.Join(home, "Videos"), filepath.Dir(vid), v)
	}
	// Same for the file, including an unterminated quote.
	t.Setenv("XDG_VIDEOS_DIR", "")
	writeDirs(t, "XDG_VIDEOS_DIR=\"$HOME/Broken\n")
	vid, err := Video()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Videos"), filepath.Dir(vid))
	// Nothing outside the home was created.
	_, err = os.Lstat(filepath.Join(home, "Broken"))
	assert.True(t, os.IsNotExist(err))
}

func TestOversizedConfigIgnored(t *testing.T) {
	home := env(t)
	big := make([]byte, maxConfig)
	for i := range big {
		big[i] = '#'
	}
	writeDirs(t, string(big)+"\nXDG_VIDEOS_DIR=\"$HOME/Late\"\n")
	vid, err := Video()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Videos"), filepath.Dir(vid))
}

func TestConfiguredNonDirectoryFails(t *testing.T) {
	env(t)
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	t.Setenv("XDG_VIDEOS_DIR", file)
	_, err := Video()
	assert.Error(t, err)
	t.Setenv("XDG_VIDEOS_DIR", filepath.Join(file, "sub"))
	_, err = Video()
	assert.Error(t, err)
}

func TestNoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_PICTURES_DIR", "")
	_, err := Screenshot()
	assert.ErrorIs(t, err, ErrNoHome)
	t.Setenv("HOME", "relative")
	_, err = Video()
	assert.ErrorIs(t, err, ErrNoHome)
}

func TestNamesDoNotCollide(t *testing.T) {
	env(t)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := Screenshot()
		require.NoError(t, err)
		seen[p] = true
	}
	assert.Len(t, seen, 200)
}

func TestInExplicitDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b")
	shot, err := ScreenshotIn(root)
	require.NoError(t, err)
	assert.Equal(t, root, filepath.Dir(shot))
	assert.True(t, strings.HasSuffix(shot, ".png"))
	fi, err := os.Stat(root)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	vid, err := VideoIn(root)
	require.NoError(t, err)
	assert.Equal(t, root, filepath.Dir(vid))
	assert.True(t, strings.HasSuffix(vid, ".mp4"))
}
