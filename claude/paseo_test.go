package claude_test

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

func TestInterruptSparesTasksAndQueued(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := handshake(h)
		if init == nil {
			return
		}
		if fakeproc.Get(init, "request", "perTaskStopAffordance") != true {
			t.Errorf("perTaskStopAffordance not declared: %v", init)
		}
		// An idle Interrupt sends nothing, so the prompt comes first.
		if m := h.Recv(); fakeproc.Str(m, "type") != "user" {
			t.Errorf("first message after idle Interrupt = %v", m)
		}
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": "11111111-1111-4111-8111-111111111111", "state": "started"})
		queued := h.RecvWhere(isType("user"))
		interrupt := h.RecvWhere(isControl("interrupt"))
		if fakeproc.Get(interrupt, "request", "cancel_queued") != nil {
			t.Errorf("interrupt cancels the queue: %v", interrupt)
		}
		h.Send(controlResponse(fakeproc.Str(interrupt, "request_id"), map[string]any{"still_queued": []any{fakeproc.Str(queued, "uuid")}, "cancelled": []any{}}))
		h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": "aborted_streaming",
			"errors": []any{}, "user_message_uuids": []any{"11111111-1111-4111-8111-111111111111"}, "usage": map[string]any{}})
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": fakeproc.Str(queued, "uuid"), "state": "started"})
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "done", "user_message_uuids": []any{fakeproc.Str(queued, "uuid")}, "usage": map[string]any{}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("work")}, ClientID: "11111111-1111-4111-8111-111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Send(ctx, agent.Text("later"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if res, err := second.Wait(waitCtx); err != nil || res.Text != "done" {
		t.Errorf("queued turn = %+v, %v; want it to run after the interrupt", res, err)
	}
	if res, err := first.Wait(waitCtx); !errors.Is(err, agent.ErrInterrupted) || res.Status != agent.TurnInterrupted {
		t.Errorf("running turn = %+v, %v", res, err)
	}

	spawner, _ = fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if init := handshake(h); init != nil && fakeproc.Get(init, "request", "perTaskStopAffordance") != nil {
			t.Errorf("perTaskStopAffordance sent despite NoPerTaskStopAffordance: %v", init)
		}
		h.WaitClosed()
	})
	off, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{ProviderOptions: &claude.SessionOptions{NoPerTaskStopAffordance: true}})
	if err != nil {
		t.Fatal(err)
	}
	off.Close()
}

func TestSessionIDFollowsStream(t *testing.T) {
	const next = "22222222-2222-4222-8222-222222222222"
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.Send(map[string]any{"type": "system", "subtype": "task_progress", "task_id": "x", "session_id": "sub", "parent_tool_use_id": "tu"})
		h.Send(map[string]any{"type": "system", "subtype": "init", "session_id": next, "model": "m1"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	ev := log.wait(t, "SessionUpdated with the new ID", func(ev agent.Event) bool {
		u, ok := ev.(*agent.SessionUpdated)
		return ok && u.Info.ID == next
	})
	if sess.ID() != next || sess.Info().ID != next || ev.Meta().SessionID != next {
		t.Errorf("ID = %q, info %q, event %q", sess.ID(), sess.Info().ID, ev.Meta().SessionID)
	}
	for _, ev := range log.all() {
		if u, ok := ev.(*agent.SessionUpdated); ok && u.Info.ID == "sub" {
			t.Error("a subagent frame changed the session ID")
		}
	}
}

func TestAllowBypassPermissions(t *testing.T) {
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		for {
			m := h.RecvWhere(isControl("set_permission_mode"))
			if m == nil {
				return
			}
			h.Send(controlResponse(fakeproc.Str(m, "request_id"), map[string]any{}))
		}
	})
	ctx := context.Background()
	p := claude.New(claude.Options{Spawner: spawner})
	plain, err := p.StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if h := <-started; slices.Contains(h.Cmd.Args, "--allow-dangerously-skip-permissions") {
		t.Errorf("flag without option: %v", h.Cmd.Args)
	}
	if err := plain.SetPermissionMode(ctx, agent.PermissionBypass); err == nil || !strings.Contains(err.Error(), "AllowBypassPermissions") {
		t.Errorf("bypass without option = %v", err)
	}
	plain.Close()

	allowed, err := p.StartSession(ctx, agent.Config{ProviderOptions: &claude.SessionOptions{AllowBypassPermissions: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Close()
	if h := <-started; !slices.Contains(h.Cmd.Args, "--allow-dangerously-skip-permissions") {
		t.Errorf("flag missing: %v", h.Cmd.Args)
	}
	if err := allowed.SetPermissionMode(ctx, agent.PermissionBypass); err != nil || allowed.Info().PermissionMode != agent.PermissionBypass {
		t.Errorf("bypass with option = %v, mode %q", err, allowed.Info().PermissionMode)
	}
}

func TestResumeMissingSession(t *testing.T) {
	missing := "No conversation found with session ID: 33333333-3333-4333-8333-333333333333"
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if slices.ContainsFunc(h.Cmd.Args, func(a string) bool { return strings.HasPrefix(a, "--resume=") }) {
			// The CLI reports the missing session and exits without
			// answering initialize.
			h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{missing}, "usage": map[string]any{}})
			h.Stderr(missing)
			return
		}
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{missing},
			"user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.WaitClosed()
	})
	ctx := context.Background()
	p := claude.New(claude.Options{Spawner: spawner})
	if _, err := p.StartSession(ctx, agent.Config{Resume: "33333333-3333-4333-8333-333333333333"}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("Start = %v, want ErrSessionNotFound", err)
	}
	sess, err := p.StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.Run(ctx, agent.Text("hi"))
	if res == nil {
		t.Fatal(err)
	}
	var te *agent.TurnError
	if res.Status != agent.TurnFailed || !errors.Is(res.Err, agent.ErrSessionNotFound) || !errors.As(res.Err, &te) || te.Code != "error_during_execution" {
		t.Errorf("result = %+v (%v)", res, res.Err)
	}
}

func TestSubagentHandbackHiddenTasksAndEffort(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := handshake(h)
		if init == nil {
			return
		}
		if n := len(fakeproc.Get(init, "request", "hooks", "PreToolUse").([]any)); n != 2 {
			t.Errorf("PreToolUse matchers = %d, want the user's and the effort hook", n)
		}
		effortHook := fakeproc.Str(init, "request", "hooks", "SubagentStop", 0, "hookCallbackIds", 0)
		startTurn(h)
		h.Send(assistantMsg("m1", "a1", nil, map[string]any{"type": "tool_use", "id": "tu_a", "name": "Agent",
			"input": map[string]any{"description": "Scout", "prompt": "look", "subagent_type": "Explore", "name": "scout"}}))
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "ag1", "tool_use_id": "tu_a", "task_type": "local_agent", "subagent_type": "Explore"})
		h.Send(map[string]any{"type": "control_request", "request_id": "hk1", "request": map[string]any{"subtype": "hook_callback", "callback_id": effortHook,
			"input": map[string]any{"hook_event_name": "SubagentStop", "agent_id": "ag1", "effort": map[string]any{"level": "low"}}}})
		if r := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "hk1" }); fakeproc.Str(r, "response", "subtype") != "success" {
			t.Errorf("effort hook answer = %v", r)
		}
		h.Send(map[string]any{"type": "stream_event", "parent_tool_use_id": "tu_a", "event": map[string]any{"type": "message_start", "message": map[string]any{"id": "m2"}}})
		h.Send(map[string]any{"type": "stream_event", "parent_tool_use_id": "tu_a", "event": map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "hb1", "name": "SubagentHandback"}}})
		h.Send(map[string]any{"type": "stream_event", "parent_tool_use_id": "tu_a", "event": map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{"}}})
		h.Send(assistantMsg("m2", "a2", "tu_a", map[string]any{"type": "tool_use", "id": "hb1", "name": "SubagentHandback", "input": map[string]any{"message": "found it"}}))
		ack := toolResult("u2", "hb1", "ok", false)
		ack["parent_tool_use_id"] = "tu_a"
		h.Send(ack)
		h.Send(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "ag1", "status": "completed", "summary": "short",
			"handback_report": map[string]any{"text": "full report", "warning": "unverified"}})
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "amb", "task_type": "local_agent", "skip_transcript": true})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{ProviderOptions: &claude.SessionOptions{
		TrackSubagentEffort: true,
		Hooks: map[claude.HookEvent][]claude.HookMatcher{claude.HookPreToolUse: {{Hooks: []claude.HookFunc{
			func(context.Context, claude.HookInput) (claude.HookOutput, error) { return claude.HookOutput{}, nil },
		}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	if _, err := sess.Send(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	report := log.wait(t, "handback message", completedItem("hb1")).(*agent.ItemCompleted).Item
	if report.Kind != agent.ItemAssistantMessage || report.Text != "found it" || report.SubagentID != "ag1" || report.ParentID != "tu_a" {
		t.Errorf("handback item = %+v", report)
	}
	done := log.wait(t, "completed subagent", func(ev agent.Event) bool {
		u, ok := ev.(*agent.TaskUpdated)
		return ok && u.Task.ID == "ag1" && u.Task.Status == agent.TaskCompleted
	}).(*agent.TaskUpdated).Task
	if done.Output != "unverified\n\nfull report" || done.Subagent.Name != "scout" || done.Subagent.Effort != "low" {
		t.Errorf("task = %+v subagent %+v", done, done.Subagent)
	}
	ambient := log.wait(t, "ambient task", func(ev agent.Event) bool {
		u, ok := ev.(*agent.TaskUpdated)
		return ok && u.Task.ID == "amb"
	}).(*agent.TaskUpdated).Task
	if !ambient.Hidden || done.Hidden {
		t.Errorf("hidden: ambient %v, subagent %v", ambient.Hidden, done.Hidden)
	}
	for _, ev := range log.all() {
		switch ev := ev.(type) {
		case *agent.ItemStarted:
			if ev.Item.ID == "hb1" {
				t.Errorf("handback call started as an item: %+v", ev.Item)
			}
		case *agent.ToolInputDelta:
			if ev.ItemID == "hb1" {
				t.Error("handback input streamed")
			}
		case *agent.ItemCompleted:
			if ev.Item.ID == "hb1" && ev.Item.Kind != agent.ItemAssistantMessage {
				t.Errorf("handback completed as %v", ev.Item.Kind)
			}
		}
	}
}

func TestContextAccountingAndCompaction(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		stream(h, map[string]any{"type": "message_start", "message": map[string]any{"id": "m1", "model": "claude-main",
			"usage": map[string]any{"input_tokens": 100, "cache_read_input_tokens": 50, "cache_creation_input_tokens": 0}}})
		stream(h, map[string]any{"type": "message_delta", "delta": map[string]any{}, "usage": map[string]any{"input_tokens": 120, "output_tokens": 10}})
		main := assistantMsg("m1", "a1", nil, map[string]any{"type": "text", "text": "hi"})
		main["message"].(map[string]any)["model"] = "claude-main"
		h.Send(main)
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "hi", "user_message_uuids": []any{uuid}, "usage": map[string]any{},
			"modelUsage": map[string]any{"claude-main": map[string]any{"contextWindow": 200000}, "claude-sub": map[string]any{"contextWindow": 1000000}}})
		uuid = startTurn(h)
		h.Send(map[string]any{"type": "system", "subtype": "status", "status": "compacting"})
		h.Send(map[string]any{"type": "system", "subtype": "status", "status": "compacting"})
		h.Send(map[string]any{"type": "user", "isCompactSummary": true, "parent_tool_use_id": nil, "uuid": "sum", "message": map[string]any{"role": "user", "content": "summary"}})
		h.Send(map[string]any{"type": "system", "subtype": "status", "status": nil})
		h.Send(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": "cb", "compact_metadata": map[string]any{"trigger": "manual", "pre_tokens": 180, "post_tokens": 40}})
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "", "user_message_uuids": []any{uuid}, "usage": map[string]any{},
			"modelUsage": map[string]any{"claude-main": map[string]any{"contextWindow": 200000}}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	res, err := sess.Run(ctx, agent.Text("one"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.ContextWindow != 200000 || res.Usage.ContextTokens != 180 {
		t.Errorf("first turn usage = %+v", res.Usage)
	}
	// Events reach the log asynchronously, so wait instead of scanning.
	log.wait(t, "context seeded from message_start", func(ev agent.Event) bool {
		u, ok := ev.(*agent.UsageUpdated)
		return ok && u.Usage.ContextTokens == 150
	})
	res, err = sess.Run(ctx, agent.Text("/compact"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.ContextTokens != 40 {
		t.Errorf("context after compaction = %d, want 40", res.Usage.ContextTokens)
	}
	log.wait(t, "turn completed", func(ev agent.Event) bool {
		c, ok := ev.(*agent.TurnCompleted)
		return ok && c.Result.TurnID == res.TurnID
	})
	starts := 0
	for _, ev := range log.all() {
		switch ev := ev.(type) {
		case *agent.Compaction:
			if !ev.Done {
				starts++
			}
		case *agent.ItemCompleted:
			if ev.Item.ID == "sum" {
				t.Error("compact summary shown as a user message")
			}
		}
	}
	if starts != 1 {
		t.Errorf("compaction starts = %d, want 1", starts)
	}
}

func TestToolImagesSyntheticPromptsAndFailedTasks(t *testing.T) {
	png := []byte("png-bytes")
	exit := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		startTurn(h)
		h.Send(assistantMsg("m1", "a1", nil, map[string]any{"type": "tool_use", "id": "shot", "name": "mcp__b__screenshot", "input": map[string]any{}}))
		h.Send(toolResult("u1", "shot", []any{map[string]any{"type": "text", "text": "captured"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(png)}}}, false))
		h.Send(map[string]any{"type": "user", "uuid": "n1", "parent_tool_use_id": nil, "message": map[string]any{"role": "user",
			"content": "<task-notification><task-id>b1</task-id></task-notification>"}})
		h.Send(map[string]any{"type": "user", "uuid": "n2", "parent_tool_use_id": nil, "isSynthetic": true, "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "text", "text": "injected"}}}})
		h.Send(map[string]any{"type": "user", "uuid": "n3", "parent_tool_use_id": nil, "isSynthetic": true, "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "sh1", "task_type": "local_bash", "description": "watch", "is_backgrounded": true})
		<-exit
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	if _, err := sess.Send(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	shot := log.wait(t, "screenshot", completedItem("shot")).(*agent.ItemCompleted).Item
	if shot.Tool.Output != "captured" || len(shot.Tool.Images) != 1 || string(shot.Tool.Images[0].Data) != string(png) || shot.Tool.Images[0].MediaType != "image/png" {
		t.Errorf("tool = %+v", shot.Tool)
	}
	for _, id := range []string{"n1", "n2"} {
		if it := log.wait(t, id, completedItem(id)).(*agent.ItemCompleted).Item; !it.Synthetic || it.Kind != agent.ItemUserMessage {
			t.Errorf("%s = %+v", id, it)
		}
	}
	log.wait(t, "shell task", func(ev agent.Event) bool { u, ok := ev.(*agent.TaskUpdated); return ok && u.Task.ID == "sh1" })
	close(exit)
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
	log.wait(t, "failed task", func(ev agent.Event) bool {
		u, ok := ev.(*agent.TaskUpdated)
		return ok && u.Task.ID == "sh1" && u.Task.Status == agent.TaskFailed
	})
	for _, ev := range log.all() {
		if c, ok := ev.(*agent.ItemCompleted); ok && c.Item.ID == "n3" {
			t.Error("interrupt placeholder shown")
		}
	}
}

func TestLaunchEnvMCPAndInputLayout(t *testing.T) {
	var content []any
	got := make(chan struct{})
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		user := h.RecvWhere(isType("user"))
		content, _ = fakeproc.Get(user, "message", "content").([]any)
		close(got)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		UnsetEnv:   []string{"FOO"},
		MCPServers: map[string]agent.MCPServer{"srv": {Command: "srv", AlwaysLoad: true, ApprovedTools: []string{"t1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	for _, name := range []string{"CLAUDECODE", "CLAUDE_CODE_SSE_PORT", "FOO"} {
		if !slices.Contains(h.Cmd.UnsetEnv, name) {
			t.Errorf("UnsetEnv %v lacks %s", h.Cmd.UnsetEnv, name)
		}
	}
	mcp := h.Cmd.Args[slices.Index(h.Cmd.Args, "--mcp-config")+1]
	if !strings.Contains(mcp, `"alwaysLoad":true`) || !slices.ContainsFunc(h.Cmd.Args, func(a string) bool {
		return strings.HasPrefix(a, "--allowedTools=") && strings.Contains(a, "mcp__srv__t1")
	}) {
		t.Errorf("args = %v", h.Cmd.Args)
	}
	if _, err := sess.Send(ctx, agent.Prompt(agent.TextPart("hi"), agent.ImageData("image/bmp", []byte("x")))); err == nil || !strings.Contains(err.Error(), "image/bmp") {
		t.Errorf("bmp image = %v", err)
	}
	if _, err := sess.Send(ctx, agent.Prompt(agent.TextPart("/review main.go"), agent.ImageData("image/png", []byte("x")))); err != nil {
		t.Fatal(err)
	}
	<-got
	if len(content) != 2 || fakeproc.Str(content, 0, "type") != "image" || fakeproc.Str(content, 1, "text") != "/review main.go" {
		t.Errorf("content = %v", content)
	}
}

func TestListModelsReadsUserSettings(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !slices.Contains(h.Cmd.Args, "--setting-sources=user") || !slices.Contains(h.Cmd.Args, "--tools=") ||
			!slices.ContainsFunc(h.Cmd.Args, func(a string) bool { return strings.Contains(a, "disableAllHooks") }) {
			t.Errorf("ListModels args = %v", h.Cmd.Args)
		}
		if handshake(h) == nil {
			return
		}
		h.WaitClosed()
	})
	models, err := claude.New(claude.Options{Spawner: spawner}).ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "m1" {
		t.Errorf("models = %+v, %v", models, err)
	}
}

func TestListModelsResolvedID(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := h.RecvWhere(isControl("initialize"))
		h.Send(controlResponse(fakeproc.Str(init, "request_id"), map[string]any{"commands": []any{}, "models": []any{
			map[string]any{"value": "default", "resolvedModel": "claude-opus-9"},
			map[string]any{"value": "sonnet", "resolvedModel": "claude-sonnet-9"},
			map[string]any{"value": "claude-haiku-9", "resolvedModel": "claude-haiku-9"},
			map[string]any{"value": "custom"},
		}}))
		h.WaitClosed()
	})
	models, err := claude.New(claude.Options{Spawner: spawner}).ListModels(context.Background())
	if err != nil || len(models) != 4 {
		t.Fatalf("models = %+v, %v", models, err)
	}
	for i, want := range []string{"claude-opus-9", "claude-sonnet-9", "", ""} {
		if models[i].ResolvedID != want {
			t.Errorf("models[%d].ResolvedID = %q, want %q", i, models[i].ResolvedID, want)
		}
	}
	if !models[0].IsDefault {
		t.Errorf("default entry = %+v", models[0])
	}
}

func TestWorkflowOutputFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "wf.json")
	if err := os.WriteFile(out, []byte(`{"result":{"answer":"42"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "wf1", "task_type": "local_workflow", "description": "wf"})
		h.Send(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "wf1", "status": "completed", "summary": "done", "output_file": out})
		h.WaitClosed()
	})
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	task := record(sess).wait(t, "workflow result", func(ev agent.Event) bool {
		u, ok := ev.(*agent.TaskUpdated)
		return ok && u.Task.ID == "wf1" && u.Task.Status == agent.TaskCompleted
	}).(*agent.TaskUpdated).Task
	if task.Output != "42" {
		t.Errorf("workflow output = %q", task.Output)
	}
}
