package shim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstall(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	if err := os.WriteFile(src, []byte("binary-content"), 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(d, "a", "b", "rclone")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o600); err != nil { // overwritten
		t.Fatal(err)
	}
	if err := Install(src, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "binary-content" {
		t.Fatalf("content %q, %v", got, err)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestInstall_CreatesParents(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	os.WriteFile(src, []byte("x"), 0o755)
	if err := Install(src, filepath.Join(d, "new", "dir", "rclone")); err != nil {
		t.Fatal(err)
	}
}

func TestInstall_Errors(t *testing.T) {
	d := t.TempDir()
	if err := Install(filepath.Join(d, "missing"), filepath.Join(d, "x")); err == nil {
		t.Fatal("missing source must fail")
	}
}
