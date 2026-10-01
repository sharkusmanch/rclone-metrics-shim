package shim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExe(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestFindReal_FirstMatchWins(t *testing.T) {
	d := t.TempDir()
	writeExe(t, filepath.Join(d, "a", "rclone"), 0o755)
	writeExe(t, filepath.Join(d, "b", "rclone"), 0o755)
	got, err := FindReal(strings.Join([]string{filepath.Join(d, "a"), filepath.Join(d, "b")}, ":"), "", "")
	if err != nil || got != filepath.Join(d, "a", "rclone") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindReal_SkipsNonExecutableAndDirectory(t *testing.T) {
	d := t.TempDir()
	writeExe(t, filepath.Join(d, "noexec", "rclone"), 0o644)
	if err := os.MkdirAll(filepath.Join(d, "dir", "rclone"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(d, "ok", "rclone"), 0o755)
	path := strings.Join([]string{filepath.Join(d, "noexec"), filepath.Join(d, "dir"), "", filepath.Join(d, "missing"), filepath.Join(d, "ok")}, ":")
	got, err := FindReal(path, "", "")
	if err != nil || got != filepath.Join(d, "ok", "rclone") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindReal_SkipsSelfAndSymlinkToSelf(t *testing.T) {
	d := t.TempDir()
	self := filepath.Join(d, "a", "rclone")
	writeExe(t, self, 0o755)
	if err := os.MkdirAll(filepath.Join(d, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(d, "b", "rclone")); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(d, "c", "rclone")
	writeExe(t, real, 0o755)
	path := strings.Join([]string{filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "c")}, ":")

	for _, selfArg := range []string{self, filepath.Join(d, "b", "rclone")} {
		got, err := FindReal(path, selfArg, "")
		if err != nil || got != real {
			t.Fatalf("self=%s: got %q, %v", selfArg, got, err)
		}
	}
}

func TestFindReal_Override(t *testing.T) {
	got, err := FindReal("/nonexistent", "", "/opt/rclone")
	if err != nil || got != "/opt/rclone" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindReal_NotFound(t *testing.T) {
	d := t.TempDir()
	self := filepath.Join(d, "rclone")
	writeExe(t, self, 0o755)
	if _, err := FindReal(d, self, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := FindReal("", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Two copies of the shim on PATH must resolve forward to the real binary, not
// to each other: each searches only the entries after its own directory.
func TestFindReal_TwoShimsResolveForward(t *testing.T) {
	d := t.TempDir()
	a, b, real := filepath.Join(d, "a", "rclone"), filepath.Join(d, "b", "rclone"), filepath.Join(d, "c", "rclone")
	writeExe(t, a, 0o755)
	writeExe(t, b, 0o755)
	writeExe(t, real, 0o755)
	path := strings.Join([]string{filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "c")}, ":")
	if got, _ := FindReal(path, a, ""); got != b {
		t.Fatalf("a found %q, want b", got)
	}
	if got, _ := FindReal(path, b, ""); got != real {
		t.Fatalf("b found %q, want the real binary (never back to a)", got)
	}
}

func TestFindReal_OverrideIsShim(t *testing.T) {
	d := t.TempDir()
	self := filepath.Join(d, "rclone")
	writeExe(t, self, 0o755)
	if _, err := FindReal("", self, self); !errors.Is(err, ErrIsShim) {
		t.Fatalf("want ErrIsShim, got %v", err)
	}
}
