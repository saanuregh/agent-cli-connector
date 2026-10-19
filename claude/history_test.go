package claude_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
)

func stamped(e map[string]any, ts string) map[string]any {
	e["timestamp"] = ts
	return e
}

func TestReadHistoryFidelity(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	todo := map[string]any{"type": "tool_use", "id": "td", "name": "TodoWrite", "input": map[string]any{"todos": []any{
		map[string]any{"content": "Read", "status": "completed"}, map[string]any{"content": "Write", "status": "in_progress"}}}}
	create := map[string]any{"type": "tool_use", "id": "tc", "name": "TaskCreate", "input": map[string]any{"subject": "Ship"}}
	created := entry("user", "r2", "a3", []any{map[string]any{"type": "tool_result", "tool_use_id": "tc", "content": "Task #1 created"}})
	created["toolUseResult"] = map[string]any{"task": map[string]any{"id": "1", "subject": "Ship"}}
	shot := map[string]any{"type": "tool_use", "id": "ts", "name": "mcp__b__shot", "input": map[string]any{}}
	boundary := map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "cb", "parentUuid": nil, "logicalParentUuid": "int",
		"compactMetadata": map[string]any{"trigger": "manual", "preTokens": 900}}
	writeTranscript(t, configDir, cwd, id, []map[string]any{
		stamped(entry("user", "u1", "", "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>x.go</command-args>"), "2026-10-01T10:00:00Z"),
		entry("assistant", "a1", "u1", text("No response requested.")),
		entry("assistant", "a2", "a1", []any{todo}),
		entry("user", "r1", "a2", []any{map[string]any{"type": "tool_result", "tool_use_id": "td", "content": "ok"}}),
		entry("user", "int", "r1", text("[Request interrupted by user]")),
		boundary,
		entry("user", "u2", "cb", "<task-notification><task-id>b1</task-id></task-notification>"),
		entry("assistant", "a3", "u2", []any{create}),
		created,
		entry("assistant", "a4", "r2", []any{shot}),
		entry("user", "r3", "a4", []any{map[string]any{"type": "tool_result", "tool_use_id": "ts", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString([]byte("img"))}}}}}),
	})
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	items, err := p.ReadHistory(context.Background(), id, agent.ReadHistoryOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, string(it.Kind)+":"+it.ID+":"+it.TurnID)
	}
	want := []string{"user_message:u1:u1", "tool_call:td:u1", "plan:td/plan:u1", "compaction:cb:u1", "user_message:u2:u2",
		"tool_call:tc:u2", "plan:tc/plan:u2", "tool_call:ts:u2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("items = %v\nwant    %v", got, want)
	}
	if items[0].Text != "/review x.go" || items[0].Synthetic || items[0].Time.IsZero() {
		t.Errorf("command item = %+v", items[0])
	}
	if items[2].Text != "- [x] Read\n- [-] Write" || items[3].Text != "manual" || !items[4].Synthetic {
		t.Errorf("plan %q, compaction %q, notification synthetic %v", items[2].Text, items[3].Text, items[4].Synthetic)
	}
	var steps []agent.PlanStep
	if json.Unmarshal(items[6].Raw, &steps) != nil || len(steps) != 1 || steps[0].ID != "1" || items[6].Text != "- [ ] Ship" {
		t.Errorf("TaskCreate plan = %q %s", items[6].Text, items[6].Raw)
	}
	if imgs := items[7].Tool.Images; len(imgs) != 1 || string(imgs[0].Data) != "img" {
		t.Errorf("history images = %+v", imgs)
	}
	page, err := p.ReadHistory(context.Background(), id, agent.ReadHistoryOptions{Cwd: cwd, Limit: 3, Offset: 2})
	if err != nil || len(page) != 3 || page[0].ID != items[3].ID || page[2].ID != items[5].ID {
		t.Errorf("history page = %+v, %v", page, err)
	}
}

func TestListSessionsPreviewsAndSymlinkedCwd(t *testing.T) {
	configDir := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	const id = "11111111-2222-4333-8444-555555555555"
	model := func(e map[string]any, m string) map[string]any {
		e["message"].(map[string]any)["model"] = m
		e["effort"] = "high"
		return e
	}
	meta := entry("user", "m0", "", "<local-command-caveat>x</local-command-caveat>")
	meta["isMeta"] = true
	writeTranscript(t, configDir, resolved, id, []map[string]any{
		meta,
		entry("user", "u1", "m0", "<command-name>/plan</command-name><command-args>the   thing</command-args>"),
		model(entry("assistant", "a1", "u1", text("ok")), "claude-opus-x"),
		entry("user", "u2", "a1", strings.Repeat("word ", 50)),
		model(entry("assistant", "a2", "u2", text("x")), "<synthetic>"),
	})
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	list, err := p.ListSessions(context.Background(), agent.ListSessionsOptions{Cwd: link})
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	s := list[0]
	if s.FirstPrompt != "/plan the thing" || len(s.LastPrompt) != 160 || strings.Contains(s.LastPrompt, "  ") || s.Model != "claude-opus-x" || s.Effort != "high" || s.Title != "/plan the   thing" {
		t.Errorf("summary = first %q last %q (%d) model %q effort %q title %q", s.FirstPrompt, s.LastPrompt, len(s.LastPrompt), s.Model, s.Effort, s.Title)
	}
}

func TestListSubagents(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	spawn := map[string]any{"type": "tool_use", "id": "tu1", "name": "Agent", "input": map[string]any{"name": "scout", "subagent_type": "Explore", "description": "Look around"}}
	wf := map[string]any{"type": "tool_use", "id": "tw", "name": "Workflow", "input": map[string]any{}}
	path := writeTranscript(t, configDir, cwd, id, []map[string]any{
		entry("user", "u1", "", "go"),
		entry("assistant", "a1", "u1", []any{spawn, wf}),
		entry("user", "r1", "a1", []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "done"}}),
		entry("user", "r2", "r1", []any{map[string]any{"type": "tool_result", "tool_use_id": "tw", "content": "Workflow started.\nRun ID: wf_1"}}),
	})
	dir := strings.TrimSuffix(path, ".jsonl")
	write := func(rel string, lines ...map[string]any) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		for _, l := range lines {
			data, _ := json.Marshal(l)
			sb.Write(data)
			sb.WriteByte('\n')
		}
		if err := os.WriteFile(full, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	answer := entry("assistant", "s2", "s1", text("found"))
	answer["message"].(map[string]any)["model"] = "claude-haiku-x"
	answer["message"].(map[string]any)["stop_reason"] = "end_turn"
	write("subagents/agent-ag1.jsonl", entry("user", "s1", "", "look"), answer)
	write("subagents/agent-ag1.meta.json", map[string]any{"agentType": "Explore", "description": "meta desc", "toolUseId": "tu1", "spawnDepth": 1, "requestShape": "foreground"})
	write("subagents/workflows/wf_1/agent-m1.jsonl", entry("user", "w1", "", "step"))
	write("workflows/wf_1.json", map[string]any{"runId": "wf_1", "summary": "Build it", "status": "completed", "totalTokens": 99, "result": map[string]any{"answer": "42"}})
	write("workflows/broken.json")

	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	tasks, err := p.ListSubagents(context.Background(), id, agent.ListSubagentsOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]agent.Task{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks = %+v", tasks)
	}
	ag := byID["ag1"]
	if ag.Kind != agent.TaskSubagent || ag.ToolCallID != "tu1" || ag.Subagent.Type != "Explore" || ag.Subagent.Name != "scout" || ag.Subagent.Depth != 1 ||
		ag.Description != "Look around" || ag.Output != "found" || ag.Status != agent.TaskCompleted || ag.Subagent.Model != "claude-haiku-x" || ag.Subagent.Prompt != "look" {
		t.Errorf("subagent = %+v %+v", ag, ag.Subagent)
	}
	if m := byID["m1"]; m.ParentID != "wf_1" {
		t.Errorf("workflow member = %+v", m)
	}
	w := byID["wf_1"]
	if w.Kind != agent.TaskWorkflow || w.ToolCallID != "tw" || w.Description != "Build it" || w.Output != "42" || w.Status != agent.TaskCompleted || w.Usage.TotalTokens != 99 {
		t.Errorf("workflow = %+v", w)
	}
	if items, err := p.ReadHistory(context.Background(), id, agent.ReadHistoryOptions{Cwd: cwd, SubagentID: "ag1"}); err != nil || len(items) != 2 {
		t.Errorf("subagent history = %+v, %v", items, err)
	}
}

func TestListSessionsOffset(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	ids := []string{"11111111-2222-4333-8444-000000000001", "11111111-2222-4333-8444-000000000002", "11111111-2222-4333-8444-000000000003"}
	for i, id := range ids {
		path := writeTranscript(t, configDir, cwd, id, []map[string]any{entry("user", "u1", "", "prompt "+id)})
		mod := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	page, err := p.ListSessions(context.Background(), agent.ListSessionsOptions{Cwd: cwd, Offset: 1, Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != ids[1] {
		t.Errorf("page = %+v, %v", page, err)
	}
}

func TestReadHistoryToolResultsMatchLive(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	write := map[string]any{"type": "tool_use", "id": "tw", "name": "Write", "input": map[string]any{"file_path": "/x/new.go", "content": "package x\n"}}
	written := entry("user", "r1", "a1", []any{map[string]any{"type": "tool_result", "tool_use_id": "tw", "content": "created"}})
	written["toolUseResult"] = map[string]any{"type": "create", "filePath": "/x/new.go", "content": "package x\n"}
	edit := map[string]any{"type": "tool_use", "id": "te", "name": "Edit", "input": map[string]any{"file_path": "/x/a.go", "old_string": "a", "new_string": "b"}}
	edited := entry("user", "r2", "a2", []any{map[string]any{"type": "tool_result", "tool_use_id": "te", "content": "ok"}})
	edited["toolUseResult"] = map[string]any{"filePath": "/x/a.go", "structuredPatch": []any{map[string]any{"oldStart": 3, "oldLines": 1, "newStart": 3, "newLines": 1, "lines": []any{"-a", "+b"}}}}
	bash := map[string]any{"type": "tool_use", "id": "tb", "name": "Bash", "input": map[string]any{"command": "sleep 9"}}
	ran := entry("user", "r3", "a3", []any{map[string]any{"type": "tool_result", "tool_use_id": "tb", "content": "partial"}})
	ran["toolUseResult"] = map[string]any{"stdout": "partial", "stderr": "", "interrupted": true}
	search := map[string]any{"type": "server_tool_use", "id": "srv1", "name": "web_search", "input": map[string]any{"query": "go"}}
	searched := map[string]any{"type": "web_search_tool_result", "tool_use_id": "srv1", "content": []any{map[string]any{"type": "web_search_result", "url": "https://go.dev"}}}
	mcp := map[string]any{"type": "mcp_tool_use", "id": "mc1", "name": "lookup", "server_name": "docs", "input": map[string]any{}}
	mcpDone := map[string]any{"type": "mcp_tool_result", "tool_use_id": "mc1", "content": []any{map[string]any{"type": "text", "text": "found"}}}
	lost := map[string]any{"type": "tool_use", "id": "tl", "name": "Bash", "input": map[string]any{"command": "make"}}
	pending := map[string]any{"type": "tool_use", "id": "tp", "name": "Bash", "input": map[string]any{"command": "make"}}
	writeTranscript(t, configDir, cwd, id, []map[string]any{
		entry("user", "u1", "", "go"),
		entry("assistant", "a1", "u1", []any{write}),
		written,
		entry("assistant", "a2", "r1", []any{edit}),
		edited,
		entry("assistant", "a3", "r2", []any{bash}),
		ran,
		entry("assistant", "a4", "r3", []any{search, searched}),
		entry("assistant", "a5", "a4", []any{mcp, mcpDone}),
		entry("assistant", "a6", "a5", []any{lost}),
		entry("user", "u2", "a6", "again"),
		entry("assistant", "a7", "u2", []any{pending}),
	})
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	items, err := p.ReadHistory(context.Background(), id, agent.ReadHistoryOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]agent.Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if c := byID["tw"].Tool.Changes; len(c) != 1 || c[0].Kind != agent.FileAdded {
		t.Errorf("write changes = %+v", c)
	}
	if c := byID["te"].Tool.Changes; len(c) != 1 || !strings.Contains(c[0].Diff, "@@ -3,1 +3,1 @@") {
		t.Errorf("edit changes = %+v", c)
	}
	if b := byID["tb"]; b.Status != agent.ItemCancelled || b.Tool.Output != "partial" {
		t.Errorf("interrupted shell = %+v", b)
	}
	if s := byID["srv1"]; s.Status != agent.ItemDone || s.Tool.Kind != agent.ToolWeb {
		t.Errorf("server tool = %+v %+v", s, s.Tool)
	}
	if m := byID["mc1"]; m.Status != agent.ItemDone || m.Tool.Name != "mcp__docs__lookup" || m.Tool.Kind != agent.ToolMCP || m.Tool.Server != "docs" || m.Tool.Output != "found" {
		t.Errorf("mcp tool = %+v %+v", m, m.Tool)
	}
	if byID["tl"].Status != agent.ItemCancelled || byID["tp"].Status != agent.ItemRunning {
		t.Errorf("unanswered calls: earlier turn %s, last turn %s", byID["tl"].Status, byID["tp"].Status)
	}
}

func TestSessionTitleCutsOnRuneBoundary(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, id, []map[string]any{entry("user", "u1", "", "a"+strings.Repeat("é", 150))})
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	sum, err := p.SessionInfo(context.Background(), id, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(sum.Title) || len(sum.Title) > 200 || len(sum.Title) < 199 {
		t.Errorf("title = %q (%d bytes)", sum.Title, len(sum.Title))
	}
}

func TestListSubagentsPrefersHandbackReport(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	path := writeTranscript(t, configDir, cwd, id, []map[string]any{entry("user", "u1", "", "go")})
	dir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	handback := map[string]any{"type": "tool_use", "id": "hb", "name": "SubagentHandback", "input": map[string]any{"message": "Found two bugs."}}
	var sb strings.Builder
	for _, e := range []map[string]any{
		entry("user", "s1", "", "look"),
		entry("assistant", "s2", "s1", []any{handback}),
		entry("user", "s3", "s2", []any{map[string]any{"type": "tool_result", "tool_use_id": "hb", "content": "ok"}}),
		entry("assistant", "s4", "s3", text("Task complete.")),
	} {
		data, _ := json.Marshal(e)
		sb.Write(append(data, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-ag1.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	tasks, err := p.ListSubagents(context.Background(), id, agent.ListSubagentsOptions{Cwd: cwd})
	if err != nil || len(tasks) != 1 || tasks[0].Output != "Found two bugs." {
		t.Errorf("tasks = %+v, %v", tasks, err)
	}
}
