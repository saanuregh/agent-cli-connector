package pi_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestSubagentEventsFromBridge(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		token := bridgeToken(t, h)
		forward := func(event string, payload map[string]any) {
			data, _ := json.Marshal(map[string]any{"event": event, "payload": payload})
			h.Send(map[string]any{"type": "extension_ui_request", "id": "s-" + event, "method": "setStatus", "statusKey": token + ":event", "statusText": string(data)})
		}
		forward("created", map[string]any{"id": "sa1", "type": "explore", "description": "Map the code", "isBackground": true})
		forward("completed", map[string]any{"id": "sa1", "type": "explore", "description": "Map the code", "result": "mapped", "status": "completed",
			"toolUses": 4, "durationMs": 1500, "usage": map[string]any{"input": 10, "output": 5, "reasoning": 3, "totalTokens": 15, "cost": map[string]any{"total": 0.002}}})
		h.WaitClosed()
	})
	cfg := agent.Config{Tools: []agent.Tool{{Name: "add", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult("5"), nil
	}}}}
	sess, err := pi.New(pi.Options{Spawner: spawner, BridgePath: "/bridge.ts"}).StartSession(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var tasks []agent.Task
	deadline := time.After(5 * time.Second)
	for len(tasks) < 2 {
		select {
		case ev := <-sess.Events():
			if tu, ok := ev.(*agent.TaskUpdated); ok {
				tasks = append(tasks, tu.Task)
			}
			if _, ok := ev.(*pi.UIUpdate); ok {
				t.Error("forwarded subagent event leaked as a UI update")
			}
		case <-deadline:
			t.Fatalf("tasks = %+v", tasks)
		}
	}
	first, last := tasks[0], tasks[1]
	if first.Status != agent.TaskRunning || first.Subagent == nil || first.Subagent.Type != "explore" || !first.Subagent.Background {
		t.Errorf("created task = %+v", first)
	}
	if last.Status != agent.TaskCompleted || last.Output != "mapped" || last.ToolUses != 4 || last.Duration != 1500*time.Millisecond ||
		last.Usage == nil || last.Usage.CostUSD != 0.002 || last.Usage.ReasoningTokens != 3 || last.Kind != agent.TaskSubagent {
		t.Errorf("completed task = %+v", last)
	}
}

func TestSubagentToolResults(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "c1", "toolName": "subagent", "args": map[string]any{"agent": "scout", "task": "look"}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "c1", "toolName": "subagent", "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "found it"}},
			"details": map[string]any{"mode": "single", "results": []any{map[string]any{
				"agent": "scout", "task": "look", "exitCode": 0, "model": "m1", "stopReason": "stop",
				"usage":    map[string]any{"input": 10, "output": 2, "cacheRead": 0, "cacheWrite": 0, "cost": 0.5},
				"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "found it"}}}},
			}}}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var task *agent.Task
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range sess.Events() {
			if tu, ok := ev.(*agent.TaskUpdated); ok {
				task = &tu.Task
			}
		}
	}()
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	<-drained
	if task == nil || task.ID != "c1:subagent:0" || task.Status != agent.TaskCompleted || task.Output != "found it" ||
		task.Subagent.Type != "scout" || task.Subagent.Model != "m1" || task.ToolCallID != "c1" || task.Usage.CostUSD != 0.5 {
		t.Errorf("task %+v", task)
	}
}
