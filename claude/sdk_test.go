package claude_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// Tests for the Agent SDK 0.3.302 features (phase 5).

func noopTool(name string, needsApproval bool) agent.Tool {
	return agent.Tool{Name: name, NeedsApproval: needsApproval, Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult("ok"), nil
	}}
}

func TestHostToolPolicyAndApprovedTools(t *testing.T) {
	inits := make(chan map[string]any, 1)
	sets := make(chan map[string]any, 1)
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := handshake(h)
		if init == nil {
			return
		}
		inits <- init
		if req := h.RecvWhere(isControl("mcp_set_servers")); req != nil {
			sets <- req
			h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"added": []any{}, "removed": []any{}, "errors": map[string]any{}}))
		}
		h.WaitClosed()
	})
	servers := map[string]agent.MCPServer{
		"remote": {URL: "https://mcp.example.com", ApprovedTools: []string{"search"}},
		"local":  {Command: "srv", ApprovedTools: []string{"run"}},
	}
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Tools:           []agent.Tool{noopTool("delete_all", true), noopTool("lookup", false)},
		MCPServers:      servers,
		ProviderOptions: &claude.SessionOptions{ToolTimeout: 90 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	init := <-inits
	if fakeproc.Str(init, "request", "sdkMcpServerConfigs", "host", "tools", 0, "name") != "delete_all" ||
		fakeproc.Str(init, "request", "sdkMcpServerConfigs", "host", "tools", 0, "permission_policy") != "always_ask" ||
		fakeproc.Get(init, "request", "sdkMcpServerConfigs", "host", "tools", 1) != nil ||
		fakeproc.Get(init, "request", "sdkMcpServerConfigs", "host", "timeout") != float64(90000) {
		t.Errorf("initialize sdkMcpServerConfigs = %v", fakeproc.Get(init, "request", "sdkMcpServerConfigs"))
	}
	allowed := ""
	for _, a := range h.Cmd.Args {
		if v, ok := strings.CutPrefix(a, "--allowedTools="); ok {
			allowed = v
		}
	}
	if !strings.Contains(allowed, "mcp__host__lookup") || strings.Contains(allowed, "delete_all") || !strings.Contains(allowed, "mcp__local__run") {
		t.Errorf("allowed tools = %q", allowed)
	}
	i := slices.Index(h.Cmd.Args, "--mcp-config")
	var mcp struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if i < 0 || json.Unmarshal([]byte(h.Cmd.Args[i+1]), &mcp) != nil ||
		fakeproc.Str(mcp.Servers["remote"], "tools", 0, "permission_policy") != "always_allow" ||
		fakeproc.Str(mcp.Servers["remote"], "tools", 0, "name") != "search" || mcp.Servers["local"]["tools"] != nil {
		t.Errorf("mcp config = %+v", mcp.Servers)
	}

	if _, err := sess.SetMCPServers(ctx, map[string]agent.MCPServer{"remote": servers["remote"]}); err != nil {
		t.Fatal(err)
	}
	set := <-sets
	if fakeproc.Str(set, "request", "servers", "host", "type") != "sdk" ||
		fakeproc.Str(set, "request", "servers", "host", "tools", 0, "permission_policy") != "always_ask" ||
		fakeproc.Get(set, "request", "servers", "host", "timeout") != float64(90000) ||
		fakeproc.Str(set, "request", "servers", "remote", "tools", 0, "permission_policy") != "always_allow" {
		t.Errorf("mcp_set_servers = %v", set)
	}
}

// twoTurns runs two turns on the fake CLI: prompts with assistant messages
// a1 and a2 (message IDs m1 and m2).
func twoTurns(h *fakeproc.Harness) (string, string) {
	var ids []string
	for _, n := range []string{"1", "2"} {
		uuid := startTurn(h)
		ids = append(ids, uuid)
		h.Send(assistantMsg("m"+n, "a"+n, nil, map[string]any{"type": "text", "text": "reply " + n}))
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "reply " + n, "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
	}
	return ids[0], ids[1]
}

func TestNativeRewind(t *testing.T) {
	type seen struct{ first, second string }
	turns := make(chan seen, 1)
	rewinds := make(chan map[string]any, 4)
	var mu sync.Mutex
	spawns := 0
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		mu.Lock()
		spawns++
		mu.Unlock()
		if handshake(h) == nil {
			return
		}
		first, second := twoTurns(h)
		turns <- seen{first, second}
		req := h.RecvWhere(isControl("rewind_conversation"))
		rewinds <- req
		h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"rewound": false, "prefillText": nil, "precedingAssistantUuid": nil,
			"error": "commands queued", "reason": "commands_queued"}))
		req = h.RecvWhere(isControl("rewind_conversation"))
		rewinds <- req
		h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"rewound": true, "targetMessageUuid": second,
			"prefillText": "second prompt", "precedingAssistantUuid": "a1"}))
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	id := sess.ID()
	if _, err := sess.Run(ctx, agent.Text("first prompt")); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Run(ctx, agent.Text("second prompt")); err != nil {
		t.Fatal(err)
	}
	ids := <-turns

	var refused *claude.RewindRefusedError
	if _, err := sess.RewindWithResult(ctx, ids.second); !errors.As(err, &refused) || refused.Reason != "commands_queued" {
		t.Fatalf("refused rewind = %v", err)
	}
	if req := <-rewinds; fakeproc.Str(req, "request", "target_message_uuid") != ids.second {
		t.Errorf("rewind request = %v", req)
	}
	// An item ID of the turn resolves to its prompt.
	res, err := sess.RewindWithResult(ctx, "m2/0")
	if err != nil {
		t.Fatal(err)
	}
	if req := <-rewinds; fakeproc.Str(req, "request", "target_message_uuid") != ids.second {
		t.Errorf("rewind by item = %v", req)
	}
	if !res.Native || res.PrefillText != "second prompt" || res.PrecedingAssistantID != "a1" || res.SessionID != id || sess.ID() != id {
		t.Errorf("result = %+v", res)
	}
	removed := log.wait(t, "ItemsRemoved", func(ev agent.Event) bool { _, ok := ev.(*agent.ItemsRemoved); return ok }).(*agent.ItemsRemoved)
	if !slices.Contains(removed.IDs, ids.second) || !slices.Contains(removed.IDs, "m2/0") ||
		slices.Contains(removed.IDs, ids.first) || slices.Contains(removed.IDs, "m1/0") {
		t.Errorf("removed = %v (turns %v)", removed.IDs, ids)
	}
	var tr agent.Transcript
	for _, ev := range log.all() {
		tr.Apply(ev)
	}
	var texts []string
	for _, it := range tr.Items() {
		texts = append(texts, it.Text)
	}
	// Send emits the prompt after writing it, so the reply can come first.
	slices.Sort(texts)
	if strings.Join(texts, "|") != "first prompt|reply 1" {
		t.Errorf("transcript after rewind = %q", texts)
	}
	mu.Lock()
	defer mu.Unlock()
	if spawns != 1 {
		t.Errorf("native rewind restarted the CLI: %d spawns", spawns)
	}
}

func TestRewindRestartRefusedByDropsTurnResumesPlainly(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, id, sampleTranscript()[:7])
	var mu sync.Mutex
	spawns := 0
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		mu.Lock()
		spawns++
		n := spawns
		mu.Unlock()
		if n == 2 {
			// The truncated resume is refused before initialize is answered.
			h.RecvWhere(isControl("initialize"))
			h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "usage": map[string]any{},
				"errors": []any{"Resume rejected by --resume-drops-turn: resuming at att1 would discard entries not attributable to turn u2"}})
			h.Stderr("Resume rejected by --resume-drops-turn: resuming at att1 would discard entries not attributable to turn u2")
			return
		}
		if handshake(h) == nil {
			return
		}
		if n == 1 {
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
	var refused *claude.RewindRefusedError
	if err := sess.Rewind(ctx, "u2"); !errors.As(err, &refused) || refused.Reason != "resume_drops_turn_rejected" {
		t.Fatalf("Rewind = %v", err)
	}
	if h := <-started; !slices.Contains(h.Cmd.Args, "--resume-drops-turn=u2") {
		t.Errorf("restart args = %v", h.Cmd.Args)
	}
	h := <-started
	if !slices.Contains(h.Cmd.Args, "--resume="+id) || slices.ContainsFunc(h.Cmd.Args, func(a string) bool {
		return strings.HasPrefix(a, "--resume-session-at") || strings.HasPrefix(a, "--resume-drops-turn")
	}) {
		t.Errorf("plain resume args = %v", h.Cmd.Args)
	}
	if sess.ID() != id {
		t.Errorf("session ID changed to %s", sess.ID())
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-sess.Done():
		t.Fatalf("session ended: %v", sess.Err())
	default:
	}
	for _, ev := range log.all() {
		if _, ok := ev.(*agent.ItemsRemoved); ok {
			t.Errorf("ItemsRemoved after a refused rewind: %+v", ev)
		}
		if tc, ok := ev.(*agent.TurnCompleted); ok {
			t.Errorf("the refusal produced a turn: %+v", tc.Result)
		}
	}
}

func TestSDKFramesMapped(t *testing.T) {
	const newID = "99999999-2222-4333-8444-555555555555"
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.Send(map[string]any{"type": "control_request", "request_id": "e1", "request": map[string]any{"subtype": "elicitation", "mcp_server_name": "srv",
			"message": "Sign in", "mode": "url", "url": "https://example.com", "elicitation_id": "el1", "title": "Sign in to Srv", "description": "Opens a browser"}})
		uuid := startTurn(h)
		h.Send(map[string]any{"type": "system", "subtype": "init", "model": "m1", "capabilities": []any{"interrupt_cancel_queued_v1"},
			"plugin_errors": []any{map[string]any{"plugin": "p@m", "type": "path-not-found", "message": "missing dir"}}, "user_message_uuids": []any{uuid}})
		h.Send(map[string]any{"type": "system", "subtype": "init", "model": "m1", "capabilities": []any{"interrupt_cancel_queued_v1"},
			"plugin_errors": []any{map[string]any{"plugin": "p@m", "type": "path-not-found", "message": "missing dir"}}})
		h.Send(map[string]any{"type": "system", "subtype": "informational", "content": "Hook blocked", "level": "suggestion", "tag": "hook_feedback"})
		h.Send(map[string]any{"type": "system", "subtype": "informational", "content": "fyi", "level": "notice"})
		h.Send(map[string]any{"type": "system", "subtype": "model_refusal_no_fallback", "content": "refused", "original_model": "m1"})
		h.Send(map[string]any{"type": "system", "subtype": "plugin_install", "status": "failed", "name": "p", "error": "network"})
		h.Send(map[string]any{"type": "system", "subtype": "elicitation_complete", "mcp_server_name": "srv", "elicitation_id": "el1"})
		h.Send(map[string]any{"type": "system", "subtype": "elicitation_complete", "mcp_server_name": "srv", "elicitation_id": "other"})
		h.Send(assistantMsg("m1", "a1", nil,
			map[string]any{"type": "tool_use", "id": "t1", "name": "Task", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "t2", "name": "ReadMcpResource", "input": map[string]any{"server": "docs", "uri": "x://y"}},
			map[string]any{"type": "tool_use", "id": "t3", "name": "KillShell", "input": map[string]any{}}))
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "", "user_message_uuids": []any{uuid}, "usage": map[string]any{},
			"origin": map[string]any{"kind": "human"}, "terminal_reason": "tool_deferred",
			"deferred_tool_use": map[string]any{"id": "t9", "name": "Bash", "input": map[string]any{"command": "ls"}}})
		uuid = startTurn(h)
		h.Send(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "You've hit your limit · resets 5pm", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		uuid = startTurn(h)
		h.Send(map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": "This service is disabled for your org", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.Send(map[string]any{"type": "conversation_reset", "new_conversation_id": newID, "trigger": "clear", "uuid": "r1", "session_id": newID})
		h.WaitClosed()
	})
	ctx := context.Background()
	elicitations := make(chan *agent.ElicitationRequest, 1)
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	go func() {
		ev := log.wait(t, "ElicitationRequest", func(ev agent.Event) bool { _, ok := ev.(*agent.ElicitationRequest); return ok })
		elicitations <- ev.(*agent.ElicitationRequest)
	}()
	el := <-elicitations
	if el.Title != "Sign in to Srv" || el.Description != "Opens a browser" {
		t.Errorf("elicitation = %+v", el)
	}
	res, err := sess.Run(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Origin != "human" || res.StopReason != "tool_deferred" {
		t.Errorf("result = %+v", res)
	}
	if el.Outcome() != agent.RequestAnswered {
		t.Errorf("completed elicitation outcome = %q", el.Outcome())
	}
	var te *agent.TurnError
	if res, err := sess.Run(ctx, agent.Text("again")); res == nil || !errors.As(err, &te) || te.Code != "usage_limit" || !te.Retryable {
		t.Errorf("usage limit result = %+v, %v", res, err)
	}
	if res, err := sess.Run(ctx, agent.Text("again")); res == nil || !errors.As(err, &te) || te.Code != "org_policy_limit" || te.Retryable {
		t.Errorf("org policy result = %+v, %v", res, err)
	}
	log.wait(t, "ConversationReset", func(ev agent.Event) bool {
		r, ok := ev.(*agent.ConversationReset)
		return ok && r.Trigger == "clear" && r.NewSessionID == newID
	})
	if sess.ID() != newID {
		t.Errorf("session ID after reset = %s", sess.ID())
	}
	if caps := sess.CLICapabilities(); !slices.Equal(caps, []string{"interrupt_cancel_queued_v1"}) {
		t.Errorf("capabilities = %v", caps)
	}
	notices := map[string]agent.NoticeLevel{}
	pluginErrors := 0
	kinds := map[string]*agent.ToolCall{}
	var deferred *claude.DeferredToolUse
	for _, ev := range log.all() {
		switch ev := ev.(type) {
		case *agent.Notice:
			notices[ev.Code+"|"+ev.Message] = ev.Level
			if ev.Code == "plugin_error" {
				pluginErrors++
			}
		case *agent.ItemUpdated:
			if ev.Item.Tool != nil {
				kinds[ev.Item.ID] = ev.Item.Tool
			}
		case *claude.DeferredToolUse:
			deferred = ev
		}
	}
	for key, level := range map[string]agent.NoticeLevel{
		"hook_feedback|Hook blocked":       agent.NoticeWarning,
		"|fyi":                             agent.NoticeInfo,
		"model_refusal|refused":            agent.NoticeError,
		"plugin_install_failed|p: network": agent.NoticeError,
		"plugin_error|p@m: missing dir":    agent.NoticeWarning,
		"elicitation_complete|srv: elicitation other completed": agent.NoticeInfo,
	} {
		if got, ok := notices[key]; !ok || got != level {
			t.Errorf("notice %q = %q, %v (all %v)", key, got, ok, notices)
		}
	}
	if pluginErrors != 1 {
		t.Errorf("plugin errors reported %d times", pluginErrors)
	}
	if k := kinds["t1"]; k == nil || k.Kind != agent.ToolSubagent {
		t.Errorf("Task kind = %+v", k)
	}
	if k := kinds["t2"]; k == nil || k.Kind != agent.ToolMCP || k.Server != "docs" {
		t.Errorf("ReadMcpResource = %+v", k)
	}
	if k := kinds["t3"]; k == nil || k.Kind != agent.ToolSubagent {
		t.Errorf("KillShell kind = %+v", k)
	}
	if deferred == nil || deferred.ToolUseID != "t9" || deferred.ToolName != "Bash" || !strings.Contains(string(deferred.Input), "ls") {
		t.Errorf("deferred = %+v", deferred)
	}
}

func TestTailShellOutputViaControl(t *testing.T) {
	finish := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		h.Send(assistantMsg("m1", "w1", nil, map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "make"}}))
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "t1", "tool_use_id": "tu1", "task_type": "local_bash", "description": "make"})
		for _, out := range []string{"hel", "hello\nwor"} {
			req := h.RecvWhere(isControl("get_task_output"))
			if fakeproc.Str(req, "request", "task_id") != "t1" {
				t.Errorf("get_task_output = %v", req)
			}
			h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"output": out, "total_bytes": len(out), "truncated": false}))
		}
		<-finish
		h.Send(toolResult("w2", "tu1", "hello\nworld", false))
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		// Later polls are answered until the tail stops.
		for {
			req := h.RecvWhere(func(m map[string]any) bool { return isControl("get_task_output")(m) || m == nil })
			if req == nil {
				return
			}
			h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"output": "hello\nworld", "total_bytes": 11}))
		}
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Env: map[string]string{"CLAUDE_CODE_TMPDIR": t.TempDir()}, ProviderOptions: &claude.SessionOptions{TailShellOutput: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	turn, err := sess.Send(ctx, agent.Text("build"))
	if err != nil {
		t.Fatal(err)
	}
	log.wait(t, "second delta", func(ev agent.Event) bool {
		d, ok := ev.(*agent.ToolOutputDelta)
		return ok && d.Delta == "lo\nwor"
	})
	close(finish)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for _, ev := range log.all() {
		if d, ok := ev.(*agent.ToolOutputDelta); ok && d.ItemID == "tu1" {
			deltas = append(deltas, d.Delta)
		}
	}
	if strings.Join(deltas, "") != "hello\nwor" {
		t.Errorf("deltas = %q", deltas)
	}
}

func TestOnControlRequestAndPendingRedelivery(t *testing.T) {
	answers := make(chan map[string]any, 2)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := h.RecvWhere(isControl("initialize"))
		if init == nil {
			return
		}
		h.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": fakeproc.Str(init, "request_id"),
			"response": map[string]any{"claude_code_version": "9.9.9", "capabilities": []any{"mcp_read_resource_v1"}},
			"pending_permission_requests": []any{map[string]any{"type": "control_request", "request_id": "p1",
				"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}, "tool_use_id": "tu1"}}},
		}})
		settings := h.RecvWhere(isControl("get_settings"))
		h.Send(controlResponse(fakeproc.Str(settings, "request_id"), map[string]any{}))
		answers <- h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "p1" })
		h.Send(map[string]any{"type": "control_request", "request_id": "c1", "request": map[string]any{"subtype": "host_auth_token_refresh", "reason": "expired"}})
		answers <- h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "c1" })
		h.WaitClosed()
	})
	var gotSubtype string
	var gotReq json.RawMessage
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Handlers: agent.Handlers{Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		}},
		ProviderOptions: &claude.SessionOptions{OnControlRequest: func(ctx context.Context, subtype string, req json.RawMessage) (any, error) {
			gotSubtype, gotReq = subtype, req
			return map[string]any{"token": "t"}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if caps := sess.CLICapabilities(); !slices.Equal(caps, []string{"mcp_read_resource_v1"}) {
		t.Errorf("capabilities = %v", caps)
	}
	p1 := <-answers
	if fakeproc.Str(p1, "response", "response", "behavior") != "allow" {
		t.Errorf("redelivered permission answer = %v", p1)
	}
	c1 := <-answers
	if fakeproc.Str(c1, "response", "subtype") != "success" || fakeproc.Str(c1, "response", "response", "token") != "t" ||
		gotSubtype != "host_auth_token_refresh" || !strings.Contains(string(gotReq), "expired") {
		t.Errorf("custom control answer = %v (subtype %q)", c1, gotSubtype)
	}
}

func TestPrewarmAndClaim(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const claimedID = "77777777-2222-4333-8444-555555555555"
	claims := make(chan map[string]any, 1)
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		req := h.RecvWhere(func(m map[string]any) bool { return isControl("claim_session")(m) || m == nil })
		if req == nil {
			return
		}
		claims <- req
		h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"status": "ok", "cwd": cwd, "session_id": claimedID, "parked_ms": 5,
			"initialize": map[string]any{"claude_code_version": "1.2.3", "capabilities": []any{"cap_a"}, "commands": []any{}}}))
		if m := h.RecvWhere(isControl("set_model")); m != nil {
			h.Send(controlResponse(fakeproc.Str(m, "request_id"), map[string]any{}))
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	p := claude.New(claude.Options{Spawner: spawner})
	base := agent.Config{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}, Tools: []agent.Tool{noopTool("lookup", false)}}

	spare, err := p.Prewarm(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	h := <-started
	spareDir := h.Cmd.Dir
	if !slices.Contains(h.Cmd.Args, "--await-claim") || !strings.HasPrefix(filepath.Base(spareDir), "claude-spare-") || filepath.Dir(spareDir) != filepath.Clean(os.TempDir()) {
		t.Errorf("spare launch: dir %s args %v", spareDir, h.Cmd.Args)
	}
	sess, err := spare.Claim(ctx, agent.Config{Cwd: cwd, Model: "m2", PermissionMode: agent.PermissionAcceptEdits, Title: "T"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	claim := <-claims
	if fakeproc.Str(claim, "request", "cwd") != cwd || fakeproc.Str(claim, "request", "permission_mode") != "acceptEdits" ||
		fakeproc.Str(claim, "request", "sdk_mcp_servers", 0) != "host" || fakeproc.Get(claim, "request", "include_initialize") != true ||
		fakeproc.Str(claim, "request", "title") != "T" {
		t.Errorf("claim = %v", claim)
	}
	if sess.ID() != claimedID || sess.Info().Cwd != cwd || sess.Info().Version != "1.2.3" || sess.Info().Model != "m2" ||
		!slices.Equal(sess.CLICapabilities(), []string{"cap_a"}) {
		t.Errorf("claimed session id %s info %+v", sess.ID(), sess.Info())
	}
	if first := <-sess.Events(); first == nil {
		t.Error("no events")
	} else if st, ok := first.(*agent.SessionStarted); !ok || st.Info.ID != claimedID {
		t.Errorf("first event = %#v", first)
	}
	if _, err := os.Stat(spareDir); !os.IsNotExist(err) {
		t.Errorf("spare directory left behind: %v", err)
	}
	if _, err := spare.Claim(ctx, agent.Config{Cwd: cwd}); err == nil {
		t.Error("second claim succeeded")
	}

	other, err := p.Prewarm(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	h = <-started
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-other.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closed spare did not exit")
	}
	if err := other.Err(); err != nil {
		t.Errorf("closed spare Err = %v", err)
	}
	if _, err := os.Stat(h.Cmd.Dir); !os.IsNotExist(err) {
		t.Errorf("closed spare left its directory: %v", err)
	}
	if _, err := p.Prewarm(ctx, agent.Config{Resume: claimedID}); err == nil {
		t.Error("Prewarm accepted Resume")
	}
}

func TestOfflineSessionActions(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	path := writeTranscript(t, configDir, cwd, id, sampleTranscript()[:7])
	sub := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := claude.New(claude.Options{Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}})
	info, err := p.SessionInfo(ctx, id, cwd)
	if err != nil || info.ID != id || info.Title != "first" || info.Path != path {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if err := p.RenameSession(ctx, id, "", "  Renamed  "); err != nil {
		t.Fatal(err)
	}
	if info, err := p.SessionInfo(ctx, id, ""); err != nil || info.Title != "Renamed" {
		t.Errorf("after rename = %+v, %v", info, err)
	}
	if err := p.RenameSession(ctx, id, cwd, " "); err == nil {
		t.Error("empty title accepted")
	}
	if err := p.DeleteSession(ctx, "not-a-uuid", cwd); err == nil {
		t.Error("invalid ID accepted")
	}
	if err := p.DeleteSession(ctx, id, cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("transcript still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(sub)); !os.IsNotExist(err) {
		t.Errorf("session folder still there: %v", err)
	}
	if _, err := p.SessionInfo(ctx, id, cwd); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("info after delete = %v", err)
	}
}

func TestPlanSuggestionsOriginAndLaunchEnv(t *testing.T) {
	users := make(chan map[string]any, 1)
	inits := make(chan map[string]any, 1)
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := handshake(h)
		if init == nil {
			return
		}
		inits <- init
		req := h.RecvWhere(isControl("get_plan"))
		h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"exists": true, "content": "# Plan", "path": "/p/plan.md"}))
		req = h.RecvWhere(isControl("file_suggestions"))
		if fakeproc.Str(req, "request", "query") != "src/ma" {
			t.Errorf("file_suggestions = %v", req)
		}
		h.Send(controlResponse(fakeproc.Str(req, "request_id"), map[string]any{"suggestions": []any{map[string]any{"path": "src/main.go"}}, "cwd": "/w"}))
		users <- h.RecvWhere(isType("user"))
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{ProviderOptions: &claude.SessionOptions{
		Agents: map[string]claude.AgentDefinition{"rev": {Description: "d", Prompt: "p", AutoCompactWindow: 50000, NumericEffort: 3, Observer: "watch"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	for _, name := range []string{"NODE_OPTIONS", "DEBUG", "CLAUDE_CODE_AGENT_PROGRESS_SUMMARIES", "CLAUDECODE"} {
		if !slices.Contains(h.Cmd.UnsetEnv, name) {
			t.Errorf("UnsetEnv %v lacks %s", h.Cmd.UnsetEnv, name)
		}
	}
	init := <-inits
	if fakeproc.Get(init, "request", "agents", "rev", "autoCompactWindow") != float64(50000) ||
		fakeproc.Get(init, "request", "agents", "rev", "effort") != float64(3) || fakeproc.Str(init, "request", "agents", "rev", "observer") != "watch" {
		t.Errorf("agents = %v", fakeproc.Get(init, "request", "agents"))
	}
	plan, err := sess.Plan(ctx)
	if err != nil || !plan.Exists || plan.Content != "# Plan" || plan.Path != "/p/plan.md" {
		t.Errorf("plan = %+v, %v", plan, err)
	}
	paths, err := sess.FileSuggestions(ctx, "src/ma")
	if err != nil || !slices.Equal(paths, []string{"src/main.go"}) {
		t.Errorf("suggestions = %v, %v", paths, err)
	}
	if _, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("hi")}, ProviderOptions: claude.TurnOptions{Origin: &claude.MessageOrigin{Kind: "human"}}}); err != nil {
		t.Fatal(err)
	}
	if u := <-users; fakeproc.Str(u, "origin", "kind") != "human" || fakeproc.Get(u, "origin", "server") != nil {
		t.Errorf("user message = %v", u)
	}
}
