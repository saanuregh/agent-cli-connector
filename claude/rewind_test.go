package claude_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// Regression tests for defects found in review.
func TestRewindDoesNotAnswerOldRequestsOnNewProcess(t *testing.T) {
	configDir, cwd := t.TempDir(), t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	writeTranscript(t, configDir, cwd, id, sampleTranscript()[:7])
	var mu sync.Mutex
	spawns := 0
	got := make(chan map[string]any, 1)
	dialogSent := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		mu.Lock()
		spawns++
		n := spawns
		mu.Unlock()
		if handshake(h) == nil {
			return
		}
		if n == 1 {
			h.Send(map[string]any{"type": "control_request", "request_id": "old_1", "request": map[string]any{"subtype": "request_user_dialog", "dialog_kind": "x"}})
			close(dialogSent)
			refuseNativeRewind(h)
			h.WaitClosed()
			return
		}
		m := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Str(m, "response", "request_id") == "old_1" })
		if m != nil {
			got <- m
		}
		h.WaitClosed()
	})
	release := make(chan struct{})
	opts := &claude.SessionOptions{OnUserDialog: func(ctx context.Context, d claude.UserDialog) (json.RawMessage, error) {
		<-release // a handler that does not watch ctx
		return json.RawMessage(`{}`), nil
	}}
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{
		Cwd: cwd, Resume: id, Env: map[string]string{"CLAUDE_CONFIG_DIR": configDir}, ProviderOptions: opts,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() {
		for range sess.Events() {
		}
	}()
	<-dialogSent
	time.Sleep(50 * time.Millisecond)
	if err := sess.Rewind(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case m := <-got:
		t.Errorf("answer to the old process's request old_1 was written to the new process: %v", m)
	case <-time.After(time.Second):
	}
}

func TestRetractedItemsLeaveTurnResult(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		user := h.RecvWhere(isType("user"))
		uuid := fakeproc.Str(user, "uuid")
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
		h.Send(map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"id": "msg1", "content": []any{map[string]any{"type": "text", "text": "refused"}}}, "parent_tool_use_id": nil})
		h.Send(map[string]any{"type": "assistant", "uuid": "a2", "supersedes": []any{"a1"}, "message": map[string]any{"id": "msg2", "content": []any{map[string]any{"type": "text", "text": "ok"}}}, "parent_tool_use_id": nil})
		h.Send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() {
		for range sess.Events() {
		}
	}()
	res, err := sess.Run(ctx, agent.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Text == "refused" {
			t.Errorf("retracted item %s still in TurnResult.Items", it.ID)
		}
	}
}
