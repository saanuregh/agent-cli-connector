package claude

import (
	"context"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// TestTurnEndDropsPerMessageState runs turns whose frames arrive before
// message_delta sets stop_reason, as Claude Code 2.1.301 sends them, and
// checks that no per-message or per-thread state outlives them.
func TestTurnEndDropsPerMessageState(t *testing.T) {
	isControl := func(subtype string) func(map[string]any) bool {
		return func(m map[string]any) bool { return fakeproc.Str(m, "request", "subtype") == subtype }
	}
	reply := func(h *fakeproc.Harness, req map[string]any, resp any) {
		h.Send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": fakeproc.Str(req, "request_id"), "response": resp}})
	}
	stream := func(h *fakeproc.Harness, parent any, event map[string]any) {
		h.Send(map[string]any{"type": "stream_event", "event": event, "parent_tool_use_id": parent})
	}
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := h.RecvWhere(isControl("initialize"))
		if init == nil {
			return
		}
		reply(h, init, map[string]any{"commands": []any{}})
		reply(h, h.RecvWhere(isControl("get_settings")), map[string]any{})
		for turn := range 2 {
			user := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "type") == "user" })
			uuid := fakeproc.Str(user, "uuid")
			h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
			msg := []string{"m1", "m2"}[turn]
			stream(h, nil, map[string]any{"type": "message_start", "message": map[string]any{"id": msg}})
			stream(h, nil, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "spawn" + msg, "name": "Agent"}})
			h.Send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": msg, "stop_reason": nil,
				"content": []any{map[string]any{"type": "tool_use", "id": "spawn" + msg, "name": "Agent", "input": map[string]any{"prompt": "go"}}}}})
			stream(h, nil, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 1}})
			stream(h, nil, map[string]any{"type": "message_stop"})
			// A foreground subagent works and finishes inside the turn.
			sub := "s" + msg
			stream(h, "spawn"+msg, map[string]any{"type": "message_start", "message": map[string]any{"id": sub}})
			h.Send(map[string]any{"type": "assistant", "parent_tool_use_id": "spawn" + msg, "agent_id": "ag" + msg, "message": map[string]any{"id": sub, "model": "haiku", "stop_reason": nil,
				"content": []any{map[string]any{"type": "text", "text": "done"}}}})
			h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "ag" + msg, "tool_use_id": "spawn" + msg, "task_type": "local_agent"})
			h.Send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "spawn" + msg, "content": "done"}}}, "tool_use_result": map[string]any{"agentId": "ag" + msg, "status": "completed"}})
			h.Send(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "ag" + msg, "tool_use_id": "spawn" + msg, "status": "completed"})
			h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := New(Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() {
		for range sess.Events() {
		}
	}()
	for range 2 {
		if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
			t.Fatal(err)
		}
	}
	tr := sess.translator()
	if len(tr.frames) != 0 || len(tr.blocks) != 0 || len(tr.agents) != 0 || len(tr.tools) != 0 || len(tr.toolTurn) != 0 ||
		len(tr.pendingSubagentItems) != 0 || len(tr.subagentModels) != 0 || len(tr.streamMessage) > 1 || len(tr.tasks) != 0 {
		t.Errorf("state left: frames %v blocks %v agents %v tools %v toolTurn %v pending %v models %v streams %v tasks %v",
			tr.frames, tr.blocks, tr.agents, tr.tools, tr.toolTurn, tr.pendingSubagentItems, tr.subagentModels, tr.streamMessage, tr.tasks)
	}
}
