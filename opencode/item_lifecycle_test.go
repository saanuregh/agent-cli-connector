package opencode_test

import (
	"context"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// completions returns the ItemCompleted events for item id seen so far.
func completions(e *events, id string) []agent.Item {
	var out []agent.Item
	for _, ev := range e.all() {
		if c, ok := ev.(*agent.ItemCompleted); ok && c.Item.ID == id {
			out = append(out, c.Item)
		}
	}
	return out
}

// settle emits a rename and waits for it, so every event emitted before
// it has been handled.
func settle(t *testing.T, f *fakeServer, e *events, title string) {
	t.Helper()
	f.emit("session.renamed", map[string]any{"title": title})
	e.wait(t, func(ev agent.Event) bool { u, ok := ev.(*agent.SessionUpdated); return ok && u.Info.Title == title })
}

// A tool call still open when its turn succeeds is cancelled, and a late
// end for it adds no second completion.
func TestTurnEndCancelsOpenToolsAndIgnoresLateEnds(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("go"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call_1", "name": "read"})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	f.emit("session.tool.success", map[string]any{"assistantMessageID": "m", "id": "call_1", "content": []any{map[string]any{"type": "text", "text": "late"}}})
	settle(t, f, ev, "after")
	got := completions(ev, "call_1")
	if len(got) != 1 || got[0].Status != agent.ItemCancelled || got[0].Tool == nil || got[0].Tool.Name != "read" {
		t.Fatalf("completions of call_1 = %+v", got)
	}
}

// Text an interrupt cut off is cancelled.
func TestInterruptCancelsOpenText(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("go"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.text.started", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0})
	f.emit("session.text.delta", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0, "delta": "half"})
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("turn = %s", res.Status)
	}
	settle(t, f, ev, "after")
	got := completions(ev, "msg_a:text:0")
	if len(got) != 1 || got[0].Status != agent.ItemCancelled || got[0].Text != "half" {
		t.Fatalf("completions = %+v", got)
	}
}

// A background subagent's open items outlive the turn that started it and
// close when the subagent stops.
func TestStoppedBackgroundSubagentClosesItems(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("delegate"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call_sub", "name": "subagent"})
	f.emit("session.tool.called", map[string]any{"assistantMessageID": "m", "id": "call_sub", "input": map[string]any{"agent": "general", "prompt": "look", "background": true}})
	f.emit("session.created", map[string]any{"sessionID": "ses_bg", "parentID": fakeSession, "agent": "general"})
	f.emit("session.tool.progress", map[string]any{"assistantMessageID": "m", "id": "call_sub", "metadata": map[string]any{"sessionID": "ses_bg", "status": "running"}})
	f.emit("session.text.started", map[string]any{"sessionID": "ses_bg", "assistantMessageID": "msg_b", "ordinal": 0})
	f.emit("session.tool.input.started", map[string]any{"sessionID": "ses_bg", "assistantMessageID": "msg_b", "id": "call_b", "name": "read"})
	f.emit("session.tool.success", map[string]any{"assistantMessageID": "m", "id": "call_sub", "metadata": map[string]any{"sessionID": "ses_bg", "status": "running"}})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	settle(t, f, ev, "turn ended")
	if n := len(completions(ev, "ses_bg/call_b")) + len(completions(ev, "msg_b:text:0")); n != 0 {
		t.Fatalf("the turn's end completed %d items of the running background subagent", n)
	}
	f.emit("session.inbox.enqueued", map[string]any{"inboxID": "inb_1", "item": map[string]any{"type": "synthetic",
		"payload": map[string]any{"text": "stopped", "metadata": map[string]any{"source": "subagent", "childID": "ses_bg", "state": "cancelled"}}}})
	settle(t, f, ev, "stopped")
	for _, id := range []string{"ses_bg/call_b", "msg_b:text:0"} {
		if got := completions(ev, id); len(got) != 1 || got[0].Status != agent.ItemCancelled || got[0].SubagentID != "ses_bg" {
			t.Errorf("completions of %s = %+v", id, got)
		}
	}
}

// A subagent call whose turn ended before it named its session is
// forgotten: a late progress report starts no task.
func TestTurnEndForgetsUnlinkedSubagentCall(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("delegate"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call_sub", "name": "subagent"})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	f.emit("session.tool.progress", map[string]any{"assistantMessageID": "m", "id": "call_sub", "metadata": map[string]any{"sessionID": "ses_late", "status": "running"}})
	settle(t, f, ev, "after")
	for _, e := range ev.all() {
		if u, ok := e.(*agent.TaskUpdated); ok && u.Task.ID == "ses_late" {
			t.Fatalf("a late progress report started task %+v", u.Task)
		}
	}
}

// A compaction without an input takes the ID OpenCode gives its message.
func TestCompactionItemTakesMessageID(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("go"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.compaction.started", map[string]any{"reason": "auto", "recent": ""})
	f.emit("session.compaction.ended", map[string]any{"reason": "auto", "text": "summary", "recent": ""})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	settle(t, f, ev, "after")
	// The fake's event ID is "evt_<type>"; OpenCode's message ID swaps the
	// prefix for "msg_".
	if got := completions(ev, "msg_session.compaction.started"); len(got) != 1 || got[0].Kind != agent.ItemCompaction {
		t.Fatalf("compaction completions = %+v", got)
	}
}

// History: a shell command without an exit code has none, a failed step
// adds no error item, and a subagent's items carry the IDs and links a
// live session gives them.
func TestHistoryMatchesLiveItems(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/message", func(r *fakeRequest) (int, any) {
		return pageMessages(r, []any{
			map[string]any{"id": "msg_u", "type": "user", "text": "go"},
			map[string]any{"id": "msg_sh", "type": "shell", "command": "sleep 9", "status": "killed", "exit": nil},
			map[string]any{"id": "msg_a", "type": "assistant", "error": map[string]any{"type": "provider", "message": "boom"}, "content": []any{
				map[string]any{"type": "tool", "id": "call_sub", "name": "subagent", "state": map[string]any{"status": "completed", "input": map[string]any{}, "metadata": map[string]any{"sessionID": "ses_kid"}}},
			}},
			map[string]any{"id": "msg_i", "type": "idle", "outcome": "failed"},
		})
	})
	f.on("GET /api/session/ses_kid", func(*fakeRequest) (int, any) {
		info := sessionInfo("ses_kid")
		info["parentID"] = fakeSession
		return 200, data(info)
	})
	f.on("GET /api/session/ses_kid/message", func(r *fakeRequest) (int, any) {
		return pageMessages(r, []any{
			map[string]any{"id": "msg_ku", "type": "user", "text": "look"},
			map[string]any{"id": "msg_ka", "type": "assistant", "content": []any{
				map[string]any{"type": "tool", "id": "call_k", "name": "read", "state": map[string]any{"status": "completed", "input": map[string]any{}}},
			}},
			map[string]any{"id": "msg_ki", "type": "idle", "outcome": "succeeded"},
		})
	})
	p := f.provider()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	items, err := p.ReadHistory(ctx, fakeSession, agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == agent.ItemError {
			t.Errorf("error item %+v", it)
		}
		if it.ID == "msg_sh" && it.Tool.ExitCode != nil {
			t.Errorf("shell exit code = %d, want none", *it.Tool.ExitCode)
		}
	}
	kid, err := p.ReadHistory(ctx, fakeSession, agent.ReadHistoryOptions{SubagentID: "ses_kid"})
	if err != nil {
		t.Fatal(err)
	}
	var tool *agent.Item
	for i := range kid {
		if kid[i].SubagentID != "ses_kid" || kid[i].ParentID != "call_sub" {
			t.Errorf("subagent item %s: SubagentID %q, ParentID %q", kid[i].ID, kid[i].SubagentID, kid[i].ParentID)
		}
		if kid[i].Kind == agent.ItemToolCall {
			tool = &kid[i]
		}
	}
	if tool == nil || tool.ID != "ses_kid/call_k" {
		t.Fatalf("subagent tool item = %+v", tool)
	}
}
