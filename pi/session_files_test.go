package pi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestSessionIDsResolveOnlyToTheirOwnFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Pi allows "_" in session IDs, so "team_abc" ends in "_abc" too.
	team := filepath.Join(dir, "2026_team_abc.jsonl")
	writeLines(t, team, map[string]any{"type": "session", "id": "team_abc"},
		map[string]any{"type": "message", "id": "u1", "parentId": nil, "message": map[string]any{"role": "user", "content": "team"}})
	// A file named for an ID whose header names another session.
	stray := filepath.Join(dir, "2026_xyz.jsonl")
	writeLines(t, stray, map[string]any{"type": "session", "id": "other"})
	p := pi.New(pi.Options{SessionDir: dir})

	for _, id := range []string{"abc", "xyz"} {
		if err := p.DeleteSession(ctx, id, ""); !errors.Is(err, agent.ErrSessionNotFound) {
			t.Errorf("DeleteSession(%q) = %v, want ErrSessionNotFound", id, err)
		}
		if _, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
			t.Errorf("ReadHistory(%q) = %v, want ErrSessionNotFound", id, err)
		}
		if _, err := p.ListSubagents(ctx, id, agent.ListSubagentsOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
			t.Errorf("ListSubagents(%q) = %v, want ErrSessionNotFound", id, err)
		}
	}
	for _, path := range []string{team, stray} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	items, err := p.ReadHistory(ctx, "team_abc", agent.ReadHistoryOptions{})
	if err != nil || len(items) != 1 || items[0].Text != "team" {
		t.Errorf("ReadHistory(team_abc) = %+v, %v", items, err)
	}
	if err := p.DeleteSession(ctx, "team_abc", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(team); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("team_abc not deleted: %v", err)
	}
}

func TestHistoryEndsToolCallsLikeLiveSessions(t *testing.T) {
	dir := t.TempDir()
	call := func(id string) map[string]any {
		return map[string]any{"type": "toolCall", "id": id, "name": "bash", "arguments": map[string]any{"command": "sleep 9"}}
	}
	writeLines(t, filepath.Join(dir, "2026_s1.jsonl"),
		map[string]any{"type": "session", "id": "s1"},
		map[string]any{"type": "message", "id": "u1", "parentId": nil, "message": map[string]any{"role": "user", "content": "go"}},
		map[string]any{"type": "message", "id": "a1", "parentId": "u1", "message": map[string]any{"role": "assistant", "stopReason": "toolUse",
			"content": []any{call("unanswered"), call("answered")}}},
		map[string]any{"type": "message", "id": "r1", "parentId": "a1", "message": map[string]any{"role": "toolResult", "toolCallId": "answered",
			"content": []any{map[string]any{"type": "text", "text": "shot"}, map[string]any{"type": "image", "data": "cG5n", "mimeType": "image/png"}}}},
		map[string]any{"type": "message", "id": "a2", "parentId": "r1", "message": map[string]any{"role": "assistant", "stopReason": "aborted",
			"content": []any{call("aborted")}}},
		map[string]any{"type": "message", "id": "b1", "parentId": "a2", "message": map[string]any{"role": "bashExecution", "command": "sleep 9", "output": "", "cancelled": true}},
		map[string]any{"type": "message", "id": "b2", "parentId": "b1", "message": map[string]any{"role": "bashExecution", "command": "false", "output": "", "exitCode": 1}},
		map[string]any{"type": "message", "id": "a3", "parentId": "b2", "message": map[string]any{"role": "assistant", "stopReason": "toolUse",
			"content": []any{call("running")}}},
	)
	items, err := pi.New(pi.Options{SessionDir: dir}).ReadHistory(context.Background(), "s1", agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]agent.Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	for id, want := range map[string]agent.ItemStatus{
		"unanswered": agent.ItemCancelled,
		"answered":   agent.ItemDone,
		"aborted":    agent.ItemCancelled,
		"b1":         agent.ItemCancelled,
		"b2":         agent.ItemDone,
		// The last call may still run.
		"running": agent.ItemRunning,
	} {
		if got := byID[id].Status; got != want {
			t.Errorf("%s status = %q, want %q", id, got, want)
		}
	}
	if b2 := byID["b2"].Tool; b2 == nil || !b2.IsError || b2.ExitCode == nil || *b2.ExitCode != 1 || string(b2.Input) != `{"command":"false"}` {
		t.Errorf("b2 tool = %+v", b2)
	}
	if shot := byID["answered"].Tool; shot.Output != "shot" || len(shot.Images) != 1 || string(shot.Images[0].Data) != "png" || shot.Images[0].MediaType != "image/png" {
		t.Errorf("answered tool = %+v", shot)
	}
}
