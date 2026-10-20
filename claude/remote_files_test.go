package claude_test

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
	"github.com/saanuregh/agent-cli-connector/claude"
)

// remoteSpawner stands for a spawner whose harnesses run on another
// machine: its file system serves the paths under local from the tree
// under remote, so a read that bypasses it sees only the empty local
// directory.
type remoteSpawner struct {
	local, remote string
}

func (remoteSpawner) Spawn(context.Context, agent.Command) (agent.Process, error) {
	return nil, errors.New("remoteSpawner does not spawn")
}

func (sp remoteSpawner) FileSystem() agent.FileSystem { return remoteFiles(sp) }

type remoteFiles struct {
	local, remote string
}

func (r remoteFiles) path(p string) (string, error) {
	rel, err := filepath.Rel(r.local, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &fs.PathError{Op: "remote", Path: p, Err: fs.ErrNotExist}
	}
	return filepath.Join(r.remote, rel), nil
}

func (r remoteFiles) Open(p string) (io.ReadCloser, error) {
	q, err := r.path(p)
	if err != nil {
		return nil, err
	}
	return os.Open(q)
}

func (r remoteFiles) Stat(p string) (fs.FileInfo, error) {
	q, err := r.path(p)
	if err != nil {
		return nil, err
	}
	return os.Stat(q)
}

func (r remoteFiles) Lstat(p string) (fs.FileInfo, error) {
	q, err := r.path(p)
	if err != nil {
		return nil, err
	}
	return os.Lstat(q)
}

func (r remoteFiles) ReadDir(p string) ([]fs.DirEntry, error) {
	q, err := r.path(p)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(q)
}

func TestSessionFilesReadThroughSpawnerFileSystem(t *testing.T) {
	localConfig, remoteConfig, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", localConfig)
	const id = "11111111-2222-4333-8444-555555555555"
	spawn := map[string]any{"type": "tool_use", "id": "tu1", "name": "Agent", "input": map[string]any{"subagent_type": "Explore"}}
	path := writeTranscript(t, remoteConfig, cwd, id, []map[string]any{
		entry("user", "u1", "", "hello remote"),
		entry("assistant", "a1", "u1", []any{spawn}),
	})
	subagents := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	if err := os.MkdirAll(subagents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subagents, "agent-ag1.jsonl"), []byte(`{"type":"user","uuid":"s1","message":{"role":"user","content":"look"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subagents, "agent-ag1.meta.json"), []byte(`{"agentType":"Explore","toolUseId":"tu1"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	p := claude.New(claude.Options{Spawner: remoteSpawner{local: localConfig, remote: remoteConfig}})
	ctx := context.Background()
	for _, dir := range []string{"", cwd} {
		sessions, err := p.ListSessions(ctx, agent.ListSessionsOptions{Cwd: dir})
		if err != nil || len(sessions) != 1 || sessions[0].ID != id || sessions[0].FirstPrompt != "hello remote" {
			t.Errorf("ListSessions(Cwd %q) = %+v, %v", dir, sessions, err)
		}
	}
	if sum, err := p.SessionInfo(ctx, id, ""); err != nil || sum.Title != "hello remote" {
		t.Errorf("SessionInfo = %+v, %v", sum, err)
	}
	items, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{})
	if err != nil || len(items) != 2 || items[0].Text != "hello remote" {
		t.Errorf("ReadHistory = %+v, %v", items, err)
	}
	if items, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{SubagentID: "ag1"}); err != nil || len(items) != 1 || items[0].Text != "look" {
		t.Errorf("ReadHistory(subagent) = %+v, %v", items, err)
	}
	tasks, err := p.ListSubagents(ctx, id, agent.ListSubagentsOptions{})
	if err != nil || len(tasks) != 1 || tasks[0].ID != "ag1" || tasks[0].ToolCallID != "tu1" || tasks[0].Subagent.Prompt != "look" {
		t.Errorf("ListSubagents = %+v, %v", tasks, err)
	}

	if err := p.RenameSession(ctx, id, "", "new title"); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("RenameSession = %v, want unsupported", err)
	}
	if err := p.DeleteSession(ctx, id, ""); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("DeleteSession = %v, want unsupported", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("transcript after refused delete: %v", err)
	}
}
