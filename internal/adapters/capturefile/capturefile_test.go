package capturefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/nefercap/internal/ports"
)

func TestCreateIsExclusiveAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a")
	f, res, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v; want 0600", info.Mode(), err)
	}
	_, _, err = Create(path)
	if !errors.Is(err, ports.ErrPathExists) || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("got %v, want both ErrPathExists and fs.ErrExist", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("path error detail lost: %v", err)
	}
	if err := res.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("own file not removed: %v", err)
	}
	if err := res.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestCreateRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
		t.Skip(err)
	}
	if _, _, err := Create(link); !errors.Is(err, ports.ErrPathExists) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("symlink target created: %v", err)
	}
}

func TestCreateOtherErrorsAreNotPathExists(t *testing.T) {
	_, _, err := Create(filepath.Join(t.TempDir(), "missing", "a"))
	if err == nil || errors.Is(err, ports.ErrPathExists) {
		t.Fatalf("got %v", err)
	}
}

func TestRemoveLeavesReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	f, res, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	moved := filepath.Join(dir, "moved")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	for name, replace := range map[string]func() error{
		"file":    func() error { return os.WriteFile(path, []byte("user"), 0o600) },
		"symlink": func() error { return os.Symlink(moved, path) },
	} {
		if err := replace(); err != nil {
			t.Fatal(err)
		}
		if err := res.Remove(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s replacement was removed: %v", name, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(moved); err != nil {
		t.Fatalf("moved original touched: %v", err)
	}
}

func TestOwnsOnlyTheReservedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	f, res, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !res.Owns() {
		t.Fatal("reserved file not owned")
	}
	if err := os.Rename(path, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	if res.Owns() {
		t.Fatal("missing path reported as owned")
	}
	if err := os.WriteFile(path, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res.Owns() {
		t.Fatal("replacement reported as owned")
	}
}
