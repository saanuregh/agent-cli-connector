package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// eventLog records a session's events so tests can wait for one.
type eventLog struct {
	mu   sync.Mutex
	list []agent.Event
}

func record(sess agent.Session) *eventLog {
	l := &eventLog{}
	go func() {
		for ev := range sess.Events() {
			l.mu.Lock()
			l.list = append(l.list, ev)
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *eventLog) all() []agent.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list)
}

// wait returns the first recorded event matching match.
func (l *eventLog) wait(t *testing.T, what string, match func(agent.Event) bool) agent.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range l.all() {
			if match(ev) {
				return ev
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s event", what)
	return nil
}

func completedItem(id string) func(agent.Event) bool {
	return func(ev agent.Event) bool {
		c, ok := ev.(*agent.ItemCompleted)
		return ok && c.Item.ID == id
	}
}

func startTurn(h *fakeproc.Harness) string {
	user := h.RecvWhere(isType("user"))
	uuid := fakeproc.Str(user, "uuid")
	h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
	return uuid
}

func assistantMsg(id, uuid string, parent any, blocks ...any) map[string]any {
	return map[string]any{"type": "assistant", "uuid": uuid, "parent_tool_use_id": parent, "message": map[string]any{"id": id, "content": blocks}}
}

func toolResult(uuid, toolID string, content any, isError bool) map[string]any {
	return map[string]any{"type": "user", "uuid": uuid, "parent_tool_use_id": nil, "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": toolID, "content": content, "is_error": isError}}}}
}

func TestPromptCompletedWithoutTurnFails(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		user := h.RecvWhere(isType("user"))
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": fakeproc.Str(user, "uuid"), "state": "completed"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	turn, err := sess.Send(ctx, agent.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := turn.Wait(waitCtx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || res != nil {
		t.Fatalf("turn = %+v, %v; want an error", res, err)
	}
}

func TestMCPServersViaControlAndTimeouts(t *testing.T) {
	servers := map[string]agent.MCPServer{
		"remote": {URL: "https://mcp.example.com", Headers: map[string]string{"Authorization": "Bearer secret"}, ToolTimeout: 65 * time.Minute},
		"local":  {Command: "srv", StartupTimeout: 30 * time.Second},
	}
	var setReq map[string]any
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		if slices.Contains(h.Cmd.Args, "--mcp-config") {
			h.WaitClosed()
			return
		}
		setReq = h.RecvWhere(isControl("mcp_set_servers"))
		h.Send(controlResponse(fakeproc.Str(setReq, "request_id"), map[string]any{"added": []any{"remote"}, "removed": []any{}, "errors": map[string]any{"local": "spawn failed"}}))
		h.WaitClosed()
	})
	ctx := context.Background()
	p := claude.New(claude.Options{Spawner: spawner})

	sess, err := p.StartSession(ctx, agent.Config{MCPServers: servers})
	if err != nil {
		t.Fatal(err)
	}
	h := <-started
	i := slices.Index(h.Cmd.Args, "--mcp-config")
	if i < 0 || !strings.Contains(h.Cmd.Args[i+1], `"timeout":3900000`) {
		t.Errorf("args = %v", h.Cmd.Args)
	}
	sess.Close()

	sess, err = p.StartSession(ctx, agent.Config{MCPServers: servers, ProviderOptions: &claude.SessionOptions{MCPViaControl: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h = <-started
	if strings.Contains(strings.Join(h.Cmd.Args, " "), "secret") || !slices.Contains(h.Cmd.Env, "MCP_TIMEOUT=30000") {
		t.Errorf("args %v env %v", h.Cmd.Args, h.Cmd.Env)
	}
	if fakeproc.Str(setReq, "request", "servers", "remote", "headers", "Authorization") != "Bearer secret" ||
		fakeproc.Get(setReq, "request", "servers", "remote", "timeout") != float64(3900000) {
		t.Errorf("mcp_set_servers = %v", setReq)
	}
	log := record(sess)
	log.wait(t, "MCP failure notice", func(ev agent.Event) bool {
		n, ok := ev.(*agent.Notice)
		return ok && n.Code == "mcp_server_failed" && strings.Contains(n.Message, "spawn failed")
	})
}

// writeTranscript writes a session transcript under configDir for cwd.
func writeTranscript(t *testing.T, configDir, cwd, sessionID string, entries []map[string]any) string {
	t.Helper()
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	dir := filepath.Join(configDir, "projects", string(b))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range entries {
		data, _ := json.Marshal(e)
		sb.Write(data)
		sb.WriteByte('\n')
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func entry(typ, uuid, parent string, content any) map[string]any {
	e := map[string]any{"type": typ, "uuid": uuid, "parentUuid": nil, "sessionId": "s", "message": map[string]any{"role": typ, "content": content}}
	if parent != "" {
		e["parentUuid"] = parent
	}
	return e
}

func text(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

// sampleTranscript has two turns and a rewound branch: u1 a1 att1 u2 a2,
// then u3 continuing from att1.
func sampleTranscript() []map[string]any {
	att := map[string]any{"type": "attachment", "uuid": "att1", "parentUuid": "a1"}
	tr := map[string]any{"type": "user", "uuid": "r1", "parentUuid": "a2t", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu", "content": "ok"}}}}
	return []map[string]any{
		entry("user", "u1", "", "first"),
		entry("assistant", "a1", "u1", text("one")),
		att,
		entry("user", "u2", "att1", "second"),
		entry("assistant", "a2t", "u2", []any{map[string]any{"type": "tool_use", "id": "tu", "name": "Bash", "input": map[string]any{"command": "ls"}}}),
		tr,
		entry("assistant", "a2", "r1", text("two")),
		entry("user", "u3", "att1", "third"),
		{"type": "user", "uuid": "sum", "parentUuid": "u3", "isCompactSummary": true, "message": map[string]any{"role": "user", "content": "summary"}},
		entry("assistant", "a3", "sum", text("three")),
	}
}

func TestForkAtTurnAndHistoryBranches(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, id, sampleTranscript())
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	p := claude.New(claude.Options{Spawner: spawner, Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	for turn, want := range map[string]string{"u1": "att1", "u2": "a2", "u3": "a3", "a1": "a1"} {
		sess, err := p.StartSession(ctx, agent.Config{Cwd: cwd, Resume: id, Fork: true, ForkAtTurn: turn})
		if err != nil {
			t.Fatal(err)
		}
		h := <-started
		if !slices.Contains(h.Cmd.Args, "--resume-session-at="+want) || !slices.Contains(h.Cmd.Args, "--fork-session") {
			t.Errorf("ForkAtTurn %s: args = %v, want resume at %s", turn, h.Cmd.Args, want)
		}
		sess.Close()
	}
	if _, err := p.StartSession(ctx, agent.Config{Cwd: cwd, Resume: id, Fork: true, ForkAtTurn: "nope"}); err == nil {
		t.Error("unknown ForkAtTurn accepted")
	}

	items, err := p.ReadHistory(ctx, id, agent.ReadHistoryOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range items {
		texts = append(texts, it.Text)
	}
	if strings.Join(texts, ",") != "first,one,third,three" {
		t.Errorf("history = %q", texts)
	}
}

func TestReadSubagentHistory(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	path := writeTranscript(t, configDir, cwd, id, []map[string]any{entry("user", "u1", "", "go")})
	sub := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	e1, e2 := entry("user", "s1", "", "look"), entry("assistant", "s2", "s1", text("found"))
	e1["isSidechain"], e2["isSidechain"] = true, true
	d1, _ := json.Marshal(e1)
	d2, _ := json.Marshal(e2)
	if err := os.WriteFile(filepath.Join(sub, "agent-ag1.jsonl"), []byte(string(d1)+"\n"+string(d2)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	items, err := p.ReadHistory(context.Background(), id, agent.ReadHistoryOptions{Cwd: cwd, SubagentID: "ag1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Text != "look" || items[1].Text != "found" {
		t.Errorf("items = %+v", items)
	}
	if _, err := p.ReadHistory(context.Background(), id, agent.ReadHistoryOptions{Cwd: cwd, SubagentID: "missing"}); err == nil {
		t.Error("missing subagent transcript read without error")
	}
}

func TestRewindRestartsInPlace(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, id, sampleTranscript()[:7])
	var mu sync.Mutex
	spawns := 0
	release := make(chan struct{})
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		mu.Lock()
		spawns++
		n := spawns
		mu.Unlock()
		if handshake(h) == nil {
			return
		}
		if n == 2 {
			uuid := startTurn(h)
			<-release
			h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		}
		if n < 3 {
			refuseNativeRewind(h)
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Cwd: cwd, Resume: id, Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	<-started
	log := record(sess)
	if !sess.Capabilities().ForkAtTurn {
		t.Error("capabilities do not report ForkAtTurn")
	}

	if err := sess.Rewind(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	h := <-started
	if !slices.Contains(h.Cmd.Args, "--resume="+id) || !slices.Contains(h.Cmd.Args, "--resume-session-at=att1") ||
		!slices.Contains(h.Cmd.Args, "--resume-drops-turn=u2") || sess.ID() != id {
		t.Errorf("rewind args = %v, id %s", h.Cmd.Args, sess.ID())
	}
	log.wait(t, "SessionUpdated", func(ev agent.Event) bool { _, ok := ev.(*agent.SessionUpdated); return ok })

	turn, err := sess.Send(ctx, agent.Text("again"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Rewind(ctx, "u1"); err == nil || !strings.Contains(err.Error(), "turn runs") {
		t.Errorf("Rewind during a turn = %v", err)
	}
	close(release)
	if res, err := turn.Wait(ctx); err != nil || res.Status != agent.TurnSucceeded {
		t.Fatalf("turn after rewind = %+v, %v", res, err)
	}

	if err := sess.Rewind(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	h = <-started
	newID := sess.ID()
	if newID == id || !slices.Contains(h.Cmd.Args, "--session-id="+newID) || slices.ContainsFunc(h.Cmd.Args, func(a string) bool { return strings.HasPrefix(a, "--resume") }) {
		t.Errorf("first-turn rewind: id %s args %v", newID, h.Cmd.Args)
	}
	if sess.Info().ID != newID {
		t.Errorf("info id = %s", sess.Info().ID)
	}
	select {
	case <-sess.Done():
		t.Fatalf("session ended after rewind: %v", sess.Err())
	default:
	}
}

func TestTranslatorParity(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		stream(h, map[string]any{"type": "message_start", "message": map[string]any{"id": "m1"}})
		stream(h, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "Let me"}})
		h.Send(assistantMsg("m1", "w-a1", nil, map[string]any{"type": "thinking", "thinking": "Let me think"}))
		// Bash with structured output and presentation metadata.
		bash := assistantMsg("m2", "w-a2", nil, map[string]any{"type": "tool_use", "id": "tub", "name": "Bash", "input": map[string]any{"command": "make"}})
		bash["tool_use_meta"] = []any{map[string]any{"id": "tub", "display_name": "Run make", "server_display_name": "Shell", "icon_url": "https://i"}}
		h.Send(bash)
		res := toolResult("w-u1", "tub", "x", false)
		res["tool_use_result"] = map[string]any{"stdout": "out", "stderr": "err", "interrupted": false, "exitCode": 2}
		h.Send(res)
		// A call the user cancelled.
		h.Send(assistantMsg("m3", "w-a3", nil, map[string]any{"type": "tool_use", "id": "tuc", "name": "mcp__s__t", "input": map[string]any{}}))
		res = toolResult("w-u2", "tuc", "rejected", true)
		res["tool_result_meta"] = []any{map[string]any{"id": "tuc", "non_execution_kind": "cancelled"}}
		h.Send(res)
		// Server tool results.
		h.Send(assistantMsg("m4", "w-a4", nil,
			map[string]any{"type": "server_tool_use", "id": "srv1", "name": "web_search", "input": map[string]any{"query": "q"}},
			map[string]any{"type": "web_search_tool_result", "tool_use_id": "srv1", "content": map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}},
			map[string]any{"type": "server_tool_use", "id": "adv1", "name": "advisor", "input": map[string]any{}},
			map[string]any{"type": "advisor_tool_result", "tool_use_id": "adv1", "content": map[string]any{"type": "advisor_result", "text": "tip"}}))
		// A background subagent whose first frame precedes task_started.
		h.Send(assistantMsg("m5", "w-a5", nil, map[string]any{"type": "tool_use", "id": "sp1", "name": "Agent",
			"input": map[string]any{"description": "Find", "prompt": "look", "subagent_type": "Explore", "model": "sonnet", "effort": "high"}}))
		early := assistantMsg("m6", "w-a6", "sp1", map[string]any{"type": "tool_use", "id": "subt", "name": "Read", "input": map[string]any{"file_path": "/a"}})
		early["message"].(map[string]any)["model"] = "claude-haiku-x"
		h.Send(early)
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "ag1", "tool_use_id": "sp1", "description": "Find",
			"subagent_type": "Explore", "task_type": "local_agent", "is_backgrounded": true})
		res = toolResult("w-u3", "sp1", "Async agent launched successfully.", false)
		res["tool_use_result"] = map[string]any{"status": "async_launched", "agentId": "ag1"}
		h.Send(res)
		h.Send(map[string]any{"type": "system", "subtype": "task_progress", "task_id": "ag1", "description": "Reading files"})
		// A workflow task.
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "wf1", "task_type": "local_workflow", "description": "wf"})
		h.Send(map[string]any{"type": "system", "subtype": "task_progress", "task_id": "wf1", "workflow_progress": []any{
			map[string]any{"type": "workflow_phase", "index": 0, "title": "Plan"},
			map[string]any{"type": "workflow_agent", "index": 0, "label": "a", "state": "start", "phaseIndex": 0, "attempt": 1, "tokens": 10, "agentId": "x1"},
		}})
		// Refusal fallback retracts a reply; supersedes evicts another.
		h.Send(assistantMsg("m7", "w-a7", nil, map[string]any{"type": "text", "text": "bad"}))
		h.Send(map[string]any{"type": "system", "subtype": "model_refusal_fallback", "scope": "session", "original_model": "m1", "fallback_model": "fb-model",
			"content": "Switched to fb-model", "retracted_message_uuids": []any{"w-a7"}})
		h.Send(assistantMsg("m8", "w-a8", nil, map[string]any{"type": "text", "text": "x"}))
		sup := assistantMsg("m9", "w-a9", nil, map[string]any{"type": "text", "text": "y"})
		sup["supersedes"] = []any{"w-a8"}
		h.Send(sup)
		h.Send(map[string]any{"type": "system", "subtype": "api_retry", "attempt": 1, "max_retries": 3, "retry_delay_ms": 500, "error": "overloaded", "error_status": 529})
		h.Send(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": "five_hour", "utilization": 1,
			"resetsAt": 1800000000, "overageStatus": "allowed", "isUsingOverage": true}})
		h.Send(map[string]any{"type": "system", "subtype": "commands_changed", "commands": []any{map[string]any{"name": "clear", "description": "", "argumentHint": "", "builtin": true}}})
		h.Send(map[string]any{"type": "assistant", "uuid": "w-err", "parent_tool_use_id": nil, "error": "rate_limit",
			"message": map[string]any{"id": "m10", "model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "limit"}}}})
		h.Send(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "", "api_error_status": 429,
			"errors": []any{"[ede_diagnostic] trace", "limit hit"}, "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.Send(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "ag1", "tool_use_id": "sp1", "status": "completed", "summary": "done"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	res, err := sess.Run(ctx, agent.Text("go"))
	if res == nil {
		t.Fatal(err)
	}
	var te *agent.TurnError
	if res.Status != agent.TurnFailed || !errors.As(res.Err, &te) || te.HTTPStatus != 429 || !te.Retryable ||
		!te.ResetsAt.Equal(time.Unix(1800000000, 0)) || te.Message != "limit hit" {
		t.Errorf("result = %+v err %+v", res, res.Err)
	}
	spawn := log.wait(t, "Agent call completion", completedItem("sp1")).(*agent.ItemCompleted).Item
	if spawn.Status != agent.ItemDone || spawn.Tool.Output != "done" {
		t.Errorf("spawn call = %+v", spawn)
	}

	var reasoning []string
	var removed [][]string
	var retry *agent.Retry
	var notices []string
	var subUpdated bool
	var task, workflow agent.Task
	var info agent.SessionInfo
	items := map[string]agent.Item{}
	var overage bool
	for _, ev := range log.all() {
		switch ev := ev.(type) {
		case *agent.ReasoningDelta:
			reasoning = append(reasoning, ev.Delta)
		case *agent.ItemsRemoved:
			removed = append(removed, ev.IDs)
		case *agent.Retry:
			retry = ev
		case *agent.Notice:
			notices = append(notices, ev.Code+":"+string(ev.Level)+":"+ev.Message)
		case *agent.ItemUpdated:
			if ev.Item.ID == "subt" && ev.Item.SubagentID == "ag1" {
				subUpdated = true
			}
		case *agent.ItemCompleted:
			items[ev.Item.ID] = ev.Item
		case *agent.TaskUpdated:
			switch ev.Task.ID {
			case "ag1":
				if ev.Task.Status == agent.TaskRunning {
					task = ev.Task
				}
			case "wf1":
				workflow = ev.Task
			}
		case *agent.SessionUpdated:
			info = ev.Info
		case *agent.RateLimitUpdated:
			overage = len(ev.Limits) == 1 && ev.Limits[0].Overage
		}
	}
	if strings.Join(reasoning, "") != "Let me" {
		t.Errorf("reasoning deltas = %q", reasoning)
	}
	if b := items["tub"].Tool; b == nil || b.Output != "out\nerr" || b.ExitCode == nil || *b.ExitCode != 2 || b.DisplayName != "Run make" ||
		b.ServerDisplayName != "Shell" || b.IconURL != "https://i" {
		t.Errorf("bash = %+v", items["tub"].Tool)
	}
	if items["tuc"].Status != agent.ItemCancelled {
		t.Errorf("cancelled call = %+v", items["tuc"])
	}
	if items["srv1"].Status != agent.ItemFailed || !items["srv1"].Tool.IsError || items["adv1"].Status != agent.ItemDone {
		t.Errorf("server tools = %+v / %+v", items["srv1"], items["adv1"])
	}
	if !subUpdated {
		t.Error("early subagent item not back-filled with its SubagentID")
	}
	if task.Description != "Find" || task.Summary != "Reading files" || task.Subagent == nil || task.Subagent.Model != "claude-haiku-x" ||
		task.Subagent.Effort != agent.EffortHigh {
		t.Errorf("subagent task = %+v %+v", task, task.Subagent)
	}
	if w := workflow.Workflow; w == nil || len(w.Phases) != 1 || w.Phases[0].Title != "Plan" || len(w.Agents) != 1 || w.Agents[0].AgentID != "x1" || w.Agents[0].Tokens != 10 {
		t.Errorf("workflow = %+v", workflow.Workflow)
	}
	if len(removed) != 2 || !slices.Equal(removed[0], []string{"m7/0"}) || !slices.Equal(removed[1], []string{"m8/0"}) {
		t.Errorf("removed = %v", removed)
	}
	if info.Model != "fb-model" || len(info.Commands) != 1 || info.Commands[0].Scope != "builtin" {
		t.Errorf("info = %+v", info)
	}
	if retry == nil || retry.Code != "overloaded" || retry.HTTPStatus != 529 {
		t.Errorf("retry = %+v", retry)
	}
	want := []string{"model_refusal_fallback:warning:Switched to fb-model", "rate_limit_overage:warning:rate limit reached (five_hour); using overage", "rate_limit:error:limit"}
	if !slices.Equal(notices, want) || !overage {
		t.Errorf("notices = %q overage %v", notices, overage)
	}
}

func TestPermissionAllowSessionWithoutSuggestions(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.Send(map[string]any{"type": "control_request", "request_id": "p1", "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "WebFetch", "input": map[string]any{"url": "https://x"}, "tool_use_id": "tw"}})
		resp := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "p1" })
		if fakeproc.Str(resp, "response", "response", "updatedPermissions", 0, "type") != "addRules" ||
			fakeproc.Str(resp, "response", "response", "updatedPermissions", 0, "rules", 0, "toolName") != "WebFetch" ||
			fakeproc.Str(resp, "response", "response", "updatedPermissions", 0, "destination") != "session" {
			t.Errorf("answer = %v", resp)
		}
		h.WaitClosed()
	})
	var decisions []agent.PermissionDecision
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{Handlers: agent.Handlers{
		Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			decisions = req.Decisions
			return agent.PermissionResponse{Decision: agent.DecisionAllowSession}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	log := record(sess)
	log.wait(t, "RequestResolved", func(ev agent.Event) bool { _, ok := ev.(*agent.RequestResolved); return ok })
	sess.Close()
	if !slices.Contains(decisions, agent.DecisionAllowSession) {
		t.Errorf("decisions = %v", decisions)
	}
}

func TestSkillPartIsLastBlock(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		user := h.RecvWhere(isType("user"))
		content := fakeproc.Get(user, "message", "content").([]any)
		if len(content) != 2 || fakeproc.Str(content[0], "type") != "image" || fakeproc.Str(content[1], "text") != "/review fix the bug\n\nquickly" {
			t.Errorf("content = %v", content)
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	in := agent.Prompt(agent.TextPart("fix the bug"), agent.SkillPart("review", ""), agent.ImageData("image/png", []byte{1}), agent.TextPart("quickly"))
	if _, err := sess.Send(ctx, in); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimitsFromGetUsage(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		req := h.RecvWhere(isControl("get_usage"))
		h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"rate_limits_available": true, "rate_limits": map[string]any{
			"five_hour":    map[string]any{"utilization": 42.5, "resets_at": "2026-10-17T12:00:00Z"},
			"seven_day":    map[string]any{"utilization": 100, "resets_at": nil},
			"model_scoped": []any{map[string]any{"display_name": "Opus 4.5", "utilization": 10, "resets_at": nil}},
		}}))
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	limits, err := sess.RateLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 3 || limits[0].Name != "five_hour" || limits[0].UsedPercent != 42.5 || limits[0].Window != 5*time.Hour ||
		limits[0].ResetsAt.IsZero() || limits[1].Status != agent.RateLimitExceeded || limits[2].Name != "seven_day_opus_4_5" || limits[2].LimitID != "Opus 4.5" {
		t.Errorf("limits = %+v", limits)
	}
}

func TestCheck(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if slices.Contains(h.Cmd.Args, "--version") {
			h.SendRaw("2.1.301 (Claude Code)")
			return
		}
		if !slices.Contains(h.Cmd.Args, "--tools=") || !slices.Contains(h.Cmd.Args, "--no-session-persistence") {
			t.Errorf("check args = %v", h.Cmd.Args)
		}
		init := h.RecvWhere(isControl("initialize"))
		h.Send(controlResponse(fakeproc.Str(init, "request_id"), map[string]any{"commands": []any{}, "models": []any{},
			"account": map[string]any{"email": "a@example.com", "subscriptionType": "max", "tokenSource": "claude.ai"}}))
		h.WaitClosed()
	})
	st, err := claude.New(claude.Options{Spawner: spawner}).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Version != "2.1.301" || st.Err != nil || st.Auth == nil || !st.Auth.Authenticated ||
		st.Auth.Method != "oauth" || st.Auth.Email != "a@example.com" || st.Auth.Plan != "max" {
		t.Errorf("status = %+v auth %+v", st, st.Auth)
	}
	st, err = claude.New(claude.Options{Path: filepath.Join(t.TempDir(), "missing")}).Check(context.Background())
	if err != nil || st.Installed || st.Err == nil {
		t.Errorf("missing executable: %+v, %v", st, err)
	}
}

func TestTailShellOutput(t *testing.T) {
	tmp := t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	dir := filepath.Join(tmp, "claude-"+strconv.Itoa(os.Getuid()), "-proj", id, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "t1.output"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	finish := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		h.Send(assistantMsg("m1", "w1", nil, map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "echo hello"}}))
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "t1", "tool_use_id": "tu1", "task_type": "local_bash", "description": "echo"})
		<-finish
		h.Send(toolResult("w2", "tu1", "hello", false))
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		SessionID: id, Env: map[string]string{"CLAUDE_CODE_TMPDIR": tmp}, ProviderOptions: &claude.SessionOptions{TailShellOutput: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	turn, err := sess.Send(ctx, agent.Text("run"))
	if err != nil {
		t.Fatal(err)
	}
	log.wait(t, "ToolOutputDelta", func(ev agent.Event) bool {
		d, ok := ev.(*agent.ToolOutputDelta)
		return ok && d.ItemID == "tu1" && d.Delta == "hello\n"
	})
	close(finish)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "t1.output"), []byte("hello\nlate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	done := false
	for _, ev := range log.all() {
		switch ev := ev.(type) {
		case *agent.ItemCompleted:
			done = done || ev.Item.ID == "tu1"
		case *agent.ToolOutputDelta:
			if done {
				t.Errorf("delta %q after the call completed", ev.Delta)
			}
		}
	}
}
