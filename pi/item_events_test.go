package pi_test

import (
	"context"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// itemEventsSession runs one turn and one Bash command against a fake Pi
// and returns every event the session emitted.
func itemEventsSession(t *testing.T) []agent.Event {
	t.Helper()
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		respond(h, prompt, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "message_start", "message": map[string]any{"role": "assistant", "content": []any{}}})
		update(h, map[string]any{"type": "text_start", "contentIndex": 0})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "toolUse",
			"content": []any{map[string]any{"type": "text", "text": "hi", "textSignature": "sig"}}}})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "c1", "toolName": "subagent", "args": map[string]any{"agent": "scout", "task": "look"}})
		h.Send(map[string]any{"type": "tool_execution_update", "toolCallId": "c1", "toolName": "subagent", "partialResult": map[string]any{
			"content": []any{}, "details": map[string]any{"results": []any{map[string]any{"agent": "scout", "task": "look"}}}}})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "c1", "toolName": "subagent", "isError": true,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "boom"}}}})
		h.Send(map[string]any{"type": "tool_execution_start", "toolCallId": "c2", "toolName": "screenshot"})
		h.Send(map[string]any{"type": "tool_execution_end", "toolCallId": "c2", "toolName": "screenshot",
			"result": map[string]any{"content": []any{map[string]any{"type": "image", "data": "cG5n", "mimeType": "image/png"}}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})

		bash := h.RecvWhere(isCommand("bash"))
		h.Send(map[string]any{"type": "bash_execution_update", "id": bash["id"], "delta": "nope\n"})
		respond(h, bash, map[string]any{"output": "nope\n", "exitCode": 1, "cancelled": false, "truncated": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.Event
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range sess.Events() {
			events = append(events, ev)
		}
	}()
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	r, err := sess.Bash(ctx, "false", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ItemID, "bash-") {
		t.Errorf("Bash item ID = %q", r.ItemID)
	}
	sess.Close()
	<-drained
	return events
}

func TestStreamedItemsCompleteAndKeepNativeData(t *testing.T) {
	events := itemEventsSession(t)
	var transcript agent.Transcript
	var task *agent.Task
	var text agent.Item
	for _, ev := range events {
		transcript.Apply(ev)
		switch ev := ev.(type) {
		case *agent.TaskUpdated:
			task = &ev.Task
		case *agent.ItemCompleted:
			if ev.Item.Kind == agent.ItemAssistantMessage {
				text = ev.Item
			}
		}
	}
	for _, it := range transcript.Items() {
		if it.Status == agent.ItemRunning {
			t.Errorf("item %s (%s) still running", it.ID, it.Kind)
		}
	}
	if task == nil || task.Status != agent.TaskFailed || task.Error != "boom" {
		t.Errorf("subagent task = %+v", task)
	}
	if !strings.Contains(string(text.Raw), `"textSignature":"sig"`) || strings.Contains(string(text.Raw), "arguments") {
		t.Errorf("text item Raw = %s", text.Raw)
	}
	shot, ok := transcript.Item("c2")
	if !ok || len(shot.Tool.Images) != 1 || string(shot.Tool.Images[0].Data) != "png" || shot.Tool.Images[0].MediaType != "image/png" {
		t.Errorf("screenshot item = %+v", shot.Tool)
	}
	var bash []agent.Item
	for _, it := range transcript.Items() {
		if it.Tool != nil && it.Tool.Command == "false" {
			bash = append(bash, it)
		}
	}
	if len(bash) != 1 || bash[0].Status != agent.ItemDone || !bash[0].Tool.IsError || bash[0].Tool.Output != "nope\n" ||
		bash[0].Tool.ExitCode == nil || *bash[0].Tool.ExitCode != 1 {
		t.Errorf("bash items = %+v", bash)
	}
}

func TestMessageItemIDsDifferAcrossSessions(t *testing.T) {
	first := func(events []agent.Event) string {
		for _, ev := range events {
			if s, ok := ev.(*agent.ItemStarted); ok && s.Item.Kind == agent.ItemAssistantMessage {
				return s.Item.ID
			}
		}
		return ""
	}
	a, b := first(itemEventsSession(t)), first(itemEventsSession(t))
	if a == "" || a == b {
		t.Errorf("message item IDs %q and %q", a, b)
	}
}
