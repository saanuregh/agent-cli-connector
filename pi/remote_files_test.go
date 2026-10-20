package pi_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// remoteSpawner stands for a spawner that runs Pi on another machine. Its
// file system serves paths under root, which do not exist on this
// machine, from the directory backing.
type remoteSpawner struct {
	root, backing string
}

func (remoteSpawner) Spawn(context.Context, agent.Command) (agent.Process, error) {
	return nil, errors.New("remoteSpawner does not start processes")
}

func (r remoteSpawner) FileSystem() agent.FileSystem { return remoteFiles(r) }

type remoteFiles remoteSpawner

func (r remoteFiles) local(path string) (string, error) {
	rel, err := filepath.Rel(r.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return filepath.Join(r.backing, rel), nil
}

func (r remoteFiles) Open(path string) (io.ReadCloser, error) {
	p, err := r.local(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	// Hide *os.File's other methods, as a remote reader has none.
	return struct{ io.ReadCloser }{f}, nil
}

func (r remoteFiles) Stat(path string) (fs.FileInfo, error) {
	p, err := r.local(path)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (r remoteFiles) Lstat(path string) (fs.FileInfo, error) {
	p, err := r.local(path)
	if err != nil {
		return nil, err
	}
	return os.Lstat(p)
}

func (r remoteFiles) ReadDir(path string) ([]fs.DirEntry, error) {
	p, err := r.local(path)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

func TestSessionFilesAreReadOnTheSpawnersMachine(t *testing.T) {
	ctx := context.Background()
	// This machine's Pi directories are empty.
	empty := t.TempDir()
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(empty, "agent"))
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", filepath.Join(empty, "sessions"))

	sp := remoteSpawner{root: filepath.Join(t.TempDir(), "remote-machine"), backing: t.TempDir()}
	remoteSessions := filepath.Join(sp.root, "sessions")
	backingFile := filepath.Join(sp.backing, "sessions", "2026_s1.jsonl")
	writeLines(t, backingFile,
		map[string]any{"type": "session", "id": "s1", "cwd": filepath.Join(sp.root, "work")},
		map[string]any{"type": "message", "id": "u1", "parentId": nil, "message": map[string]any{"role": "user", "content": "remote hello"}})
	p := pi.New(pi.Options{Spawner: sp, SessionDir: remoteSessions})

	sums, err := p.ListSessions(ctx, agent.ListSessionsOptions{})
	if err != nil || len(sums) != 1 || sums[0].ID != "s1" || sums[0].Path != filepath.Join(remoteSessions, "2026_s1.jsonl") || sums[0].FirstPrompt != "remote hello" {
		t.Fatalf("ListSessions = %+v, %v; want the remote session s1", sums, err)
	}
	items, err := p.ReadHistory(ctx, "s1", agent.ReadHistoryOptions{})
	if err != nil || len(items) != 1 || items[0].Text != "remote hello" {
		t.Fatalf("ReadHistory(s1) = %+v, %v", items, err)
	}
	items, err = p.ReadHistory(ctx, sums[0].Path, agent.ReadHistoryOptions{})
	if err != nil || len(items) != 1 {
		t.Fatalf("ReadHistory(%s) = %+v, %v", sums[0].Path, items, err)
	}
	if _, err := p.ListSubagents(ctx, "s1", agent.ListSubagentsOptions{}); err != nil {
		t.Fatalf("ListSubagents(s1) = %v", err)
	}

	// Deleting needs a write on the remote machine, which a FileSystem
	// cannot do.
	if err := p.DeleteSession(ctx, "s1", ""); err == nil {
		t.Error("DeleteSession on a remote file system succeeded")
	}
	if _, err := os.Stat(backingFile); err != nil {
		t.Errorf("remote session file: %v", err)
	}
}
