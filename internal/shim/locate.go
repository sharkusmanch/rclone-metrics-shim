package shim

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNotFound means no real rclone could be located.
var ErrNotFound = errors.New("real rclone not found on PATH")

// FindReal returns the path of the real rclone: override if set, otherwise the
// first executable regular file named "rclone" on pathEnv that is not the shim
// itself (self), compared by file identity so symlinks to the shim are skipped.
func FindReal(pathEnv, self, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	var selfInfo os.FileInfo
	if self != "" {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		selfInfo, _ = os.Stat(self)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
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
