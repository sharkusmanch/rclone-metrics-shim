package shim

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNotFound means no real rclone could be located.
var ErrNotFound = errors.New("real rclone not found on PATH")

// ErrIsShim means the configured real rclone is the shim itself.
var ErrIsShim = errors.New("RCLONESHIM_REAL points at the shim itself")

// FindReal returns the path of the real rclone: override if set, otherwise the
// first executable regular file named "rclone" on pathEnv that is not the shim
// itself (self), compared by file identity so symlinks to the shim are skipped.
//
// When the shim's own directory is on pathEnv, only the entries after it are
// searched, as a shell would continue a PATH lookup. Two copies of the shim on
// PATH then resolve forward to the real binary instead of to each other.
func FindReal(pathEnv, self, override string) (string, error) {
	var selfInfo os.FileInfo
	selfDir := ""
	if self != "" {
		selfDir = filepath.Dir(self)
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		selfInfo, _ = os.Stat(self)
	}
	if override != "" {
		if info, err := os.Stat(override); err == nil && selfInfo != nil && os.SameFile(info, selfInfo) {
			return "", ErrIsShim
		}
		return override, nil
	}
	dirs := filepath.SplitList(pathEnv)
	for i, dir := range dirs {
		if selfDir != "" && dir != "" && sameDir(dir, selfDir) {
			dirs = dirs[i+1:]
			break
		}
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "rclone")
		info, err := os.Stat(candidate) // follows symlinks
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if selfInfo != nil && os.SameFile(info, selfInfo) {
			continue
		}
		return candidate, nil
	}
	return "", ErrNotFound
}

func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}
