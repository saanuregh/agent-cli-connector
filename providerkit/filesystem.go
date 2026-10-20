package providerkit

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
)

// ReadFile reads a whole file from fsys.
func ReadFile(fsys agent.FileSystem, path string) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Glob is filepath.Glob over fsys: the paths matching pattern, in
// filepath.Glob's order (each directory's matches sorted, directories in
// the order they matched), or nil when none match. Like filepath.Glob it
// ignores I/O errors and fails only for a malformed pattern.
func Glob(fsys agent.FileSystem, pattern string) ([]string, error) {
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	if !hasMeta(pattern) {
		if _, err := fsys.Lstat(pattern); err != nil {
			return nil, nil
		}
		return []string{pattern}, nil
	}
	dir, file := filepath.Split(pattern)
	dir = cleanGlobDir(dir)
	dirs := []string{dir}
	if hasMeta(dir) {
		var err error
		if dirs, err = Glob(fsys, dir); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, d := range dirs {
		entries, err := fsys.ReadDir(d)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			if ok, _ := filepath.Match(file, e.Name()); ok {
				names = append(names, e.Name())
			}
		}
		slices.Sort(names)
		for _, name := range names {
			out = append(out, filepath.Join(d, name))
		}
	}
	return out, nil
}

// cleanGlobDir prepares the directory part of a pattern as filepath.Glob
// does: it drops the trailing separator but keeps a root such as "/" or
// "C:\", and turns a bare volume "C:" into "C:.".
func cleanGlobDir(dir string) string {
	vol := filepath.VolumeName(dir)
	switch {
	case dir == "":
		return "."
	case len(dir) == len(vol)+1 && os.IsPathSeparator(dir[len(dir)-1]):
		return dir
	case dir == vol && len(vol) == 2:
		return dir + "."
	}
	return dir[:len(dir)-1]
}

func hasMeta(path string) bool {
	magic := `*?[`
	if filepath.Separator != '\\' {
		magic = `*?[\`
	}
	return strings.ContainsAny(path, magic)
}

// WalkDir is filepath.WalkDir over fsys: it calls fn for root and every
// file and directory below it, in lexical order. fs.SkipDir and fs.SkipAll
// work as in filepath.WalkDir.
func WalkDir(fsys agent.FileSystem, root string, fn fs.WalkDirFunc) error {
	info, err := fsys.Lstat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = walkDir(fsys, root, fs.FileInfoToDirEntry(info), fn)
	}
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

func walkDir(fsys agent.FileSystem, path string, d fs.DirEntry, fn fs.WalkDirFunc) error {
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if errors.Is(err, fs.SkipDir) && d.IsDir() {
			err = nil
		}
		return err
	}
	entries, err := fsys.ReadDir(path)
	if err != nil {
		if err = fn(path, d, err); err != nil {
			if errors.Is(err, fs.SkipDir) && d.IsDir() {
				err = nil
			}
			return err
		}
	}
	for _, e := range entries {
		if err := walkDir(fsys, filepath.Join(path, e.Name()), e, fn); err != nil {
			if errors.Is(err, fs.SkipDir) {
				break
			}
			return err
		}
	}
	return nil
}
