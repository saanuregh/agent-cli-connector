package providerkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CacheDir returns the private per-user cache directory
// agent-cli-connector/<sub> under os.UserCacheDir, or the
// agent-cli-connector directory itself when sub is empty, creating it
// when needed. The returned directory and agent-cli-connector must be
// plain directories this user owns; their mode is tightened to 0700 when
// other users can access them. Folders above them are not checked.
func CacheDir(sub string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no user cache directory: %w", err)
	}
	root := filepath.Join(cache, "agent-cli-connector")
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}
	if err := CheckOwnDir(root, "cache directory"); err != nil {
		return "", err
	}
	if dir != root {
		if err := CheckOwnDir(dir, "cache directory"); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// CheckOwnDir verifies that dir is a plain directory owned by this user
// that nobody else can write, tightening its mode to 0700 when needed.
// label names the directory in errors, such as "cache directory".
func CheckOwnDir(dir, label string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s %s is not a plain directory", label, dir)
	}
	if owner, ok := fileOwner(info); ok && owner != os.Getuid() {
		return fmt.Errorf("%s %s is owned by another user", label, dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return errors.Join(fmt.Errorf("%s %s is open to other users", label, dir), err)
		}
	}
	return nil
}
