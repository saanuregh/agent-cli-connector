package providerkit

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

func fileTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"a/s1/tasks/x.output", "a/s1/log.jsonl", "b/s1/tasks/y.output", "b/s2.jsonl", "c.txt", "-p/id.jsonl", "-p-x/id.jsonl"} {
		path := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestGlobMatchesFilepath(t *testing.T) {
	root := fileTree(t)
	// "*/id.jsonl" orders by directory first, as filepath.Glob does: -p
	// before -p-x, although "-p-x/id.jsonl" sorts before "-p/id.jsonl".
	for _, p := range []string{"*", "*/s1", "*/s1/tasks/*.output", "b/*.jsonl", "c.txt", "missing", "*/missing/*", "[ab]/s1", "*/id.jsonl"} {
		pattern := filepath.Join(root, filepath.FromSlash(p))
		want, _ := filepath.Glob(pattern)
		got, err := Glob(agent.LocalFileSystem, pattern)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Glob(%s) = %v, %v; filepath.Glob gives %v", p, got, err, want)
		}
	}
	if _, err := Glob(agent.LocalFileSystem, filepath.Join(root, "[")); err == nil {
		t.Error("malformed pattern accepted")
	}
}

func TestWalkDirMatchesFilepath(t *testing.T) {
	root := fileTree(t)
	walk := func(w func(string, fs.WalkDirFunc) error) []string {
		var seen []string
		_ = w(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			seen = append(seen, path)
			if d.Name() == "s1" && filepath.Base(filepath.Dir(path)) == "b" {
				return fs.SkipDir
			}
			if d.Name() == "log.jsonl" {
				return fs.SkipDir // skips the rest of a/s1
			}
			return nil
		})
		return seen
	}
	want := walk(filepath.WalkDir)
	got := walk(func(root string, fn fs.WalkDirFunc) error { return WalkDir(agent.LocalFileSystem, root, fn) })
	if !slices.Equal(got, want) {
		t.Errorf("WalkDir visited %v, filepath.WalkDir %v", got, want)
	}
	data, err := ReadFile(agent.LocalFileSystem, filepath.Join(root, "c.txt"))
	if err != nil || string(data) != "c.txt" {
		t.Errorf("ReadFile = %q, %v", data, err)
	}
}
