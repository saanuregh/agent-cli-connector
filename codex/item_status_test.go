package codex_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// Items without a native status start running, an interrupted subagent
// call ends cancelled, and the items a stopped subagent turn left running
// end like the main thread's.
func TestItemStatusesOfStartedAndStoppedItems(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		startTurn(h, "t1")
		item(h, "item/completed", "t1", map[string]any{"type": "subAgentActivity", "id": "act1", "kind": "started", "agentThreadId": child, "agentPath": "/root/w"})
		item(h, "item/started", "t1", map[string]any{"type": "webSearch", "id": "ws1", "query": "go"})
		item(h, "item/completed", "t1", map[string]any{"type": "webSearch", "id": "ws1", "query": "go"})

		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "status": "inProgress", "items": []any{}}})
		childNotify(h, "item/started", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "cc1", "command": "sleep", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
		childNotify(h, "item/started", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "webSearch", "id": "cw1", "query": "q"}})
		childNotify(h, "item/started", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "cdone", "command": "ls", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
		childNotify(h, "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "cdone", "command": "ls", "cwd": "/w", "status": "completed", "commandActions": []any{}}})
		childNotify(h, "turn/completed", map[string]any{"turn": map[string]any{"id": "ct1", "status": "interrupted", "items": []any{}}})

		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct2", "status": "inProgress", "items": []any{}}})
		childNotify(h, "item/started", map[string]any{"turnId": "ct2", "item": map[string]any{"type": "commandExecution", "id": "cc2", "command": "make", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
		childNotify(h, "turn/completed", map[string]any{"turn": map[string]any{"id": "ct2", "status": "failed", "items": []any{}, "error": map[string]any{"message": "boom"}}})

		item(h, "item/completed", "t1", map[string]any{"type": "collabAgentToolCall", "id": "co1", "tool": "wait", "status": "interrupted", "senderThreadId": thread, "receiverThreadIds": []any{}})
		turnCompleted(h, "t1", "completed", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Send(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	started := map[string]agent.Item{}
	done := map[string][]agent.Item{}
	collect(t, sess, func(ev agent.Event) bool {
		switch ev := ev.(type) {
		case *agent.ItemStarted:
			started[ev.Item.ID] = ev.Item
		case *agent.ItemCompleted:
			done[ev.Item.ID] = append(done[ev.Item.ID], ev.Item)
		}
		return isTurnCompleted(ev)
	})
	for _, id := range []string{"ws1", "cw1", "cc1"} {
		if started[id].Status != agent.ItemRunning {
			t.Errorf("started %s status = %s", id, started[id].Status)
		}
	}
	end := func(id string) agent.Item {
		t.Helper()
		if len(done[id]) != 1 {
			t.Fatalf("%s completed %d times", id, len(done[id]))
		}
		return done[id][0]
	}
	for _, id := range []string{"cc1", "cw1"} {
		if it := end(id); it.Status != agent.ItemCancelled || it.SubagentID != child || it.ParentID != "act1" || it.Tool.IsError {
			t.Errorf("%s = %+v", id, it)
		}
	}
	if it := end("cdone"); it.Status != agent.ItemDone {
		t.Errorf("cdone = %+v", it)
	}
	if it := end("cc2"); it.Status != agent.ItemFailed || !it.Tool.IsError || it.TurnID != "ct2" {
		t.Errorf("cc2 = %+v", it)
	}
	if it := end("co1"); it.Status != agent.ItemCancelled {
		t.Errorf("co1 = %+v", it)
	}
}

// Rewind reports the items of the removed turns; an unknown turn removes
// every turn the session saw.
func TestRewindReportsRemovedItems(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		for _, id := range []string{"t1", "t2"} {
			startTurn(h, id)
			item(h, "item/completed", id, map[string]any{"type": "userMessage", "id": "u-" + id, "content": []any{}})
			item(h, "item/started", id, map[string]any{"type": "commandExecution", "id": "c-" + id, "command": "ls", "cwd": "/w", "status": "inProgress", "commandActions": []any{}})
			item(h, "item/completed", id, map[string]any{"type": "commandExecution", "id": "c-" + id, "command": "ls", "cwd": "/w", "status": "completed", "commandActions": []any{}})
			turnCompleted(h, id, "completed", nil)
		}
		for range 2 {
			h.Reply(h.RecvWhere(fakeproc.Method("thread/read")), map[string]any{"thread": map[string]any{"id": thread, "historyMode": "paginated"}})
			h.Reply(h.RecvWhere(fakeproc.Method("thread/revert")), map[string]any{"thread": map[string]any{"id": thread}})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, text := range []string{"one", "two"} {
		if _, err := sess.Send(ctx, agent.Text(text)); err != nil {
			t.Fatal(err)
		}
		collect(t, sess, isTurnCompleted)
	}
	removed := func() []string {
		t.Helper()
		evs := collect(t, sess, func(ev agent.Event) bool { _, ok := ev.(*agent.ItemsRemoved); return ok })
		return evs[len(evs)-1].(*agent.ItemsRemoved).IDs
	}
	if err := sess.Rewind(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if ids := removed(); !slices.Equal(ids, []string{"u-t2", "c-t2"}) {
		t.Errorf("removed = %v", ids)
	}
	if err := sess.Rewind(ctx, "t0"); err != nil {
		t.Fatal(err)
	}
	if ids := removed(); !slices.Equal(ids, []string{"u-t1", "c-t1"}) {
		t.Errorf("removed for an unknown turn = %v", ids)
	}
}

// History ends the items an interrupted or failed turn left in progress,
// as the live session does.
func TestHistoryEndsItemsOfStoppedTurns(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		h.Reply(h.RecvWhere(fakeproc.Method("initialize")), map[string]any{"userAgent": "fake"})
		cmd := func(id, status string) map[string]any {
			return map[string]any{"type": "commandExecution", "id": id, "command": "ls", "cwd": "/w", "status": status, "commandActions": []any{}}
		}
		for m := h.Recv(); m != nil; m = h.Recv() {
			if fakeproc.Str(m, "method") != "thread/read" {
				if m["id"] != nil {
					h.Reply(m, map[string]any{})
				}
				continue
			}
			h.Reply(m, map[string]any{"thread": map[string]any{"id": thread, "turns": []any{
				map[string]any{"id": "t1", "status": "interrupted", "items": []any{cmd("a", "inProgress"), cmd("b", "completed")}},
				map[string]any{"id": "t2", "status": "failed", "items": []any{cmd("c", "inProgress")}},
				map[string]any{"id": "t3", "status": "inProgress", "items": []any{cmd("d", "inProgress")}},
			}}})
		}
	})
	p := codex.New(codex.Options{Spawner: spawner, SharedServer: true})
	defer p.Close()
	items, err := p.ReadHistory(context.Background(), thread, agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]agent.ItemStatus{"a": agent.ItemCancelled, "b": agent.ItemDone, "c": agent.ItemFailed, "d": agent.ItemRunning}
	if len(items) != len(want) {
		t.Fatalf("items = %+v", items)
	}
	for _, it := range items {
		if it.Status != want[it.ID] || it.Tool.IsError != (it.ID == "c") {
			t.Errorf("%s: status %s, error %v", it.ID, it.Status, it.Tool.IsError)
		}
	}
}

// UnsetEnv strips variables, such as secrets, that a shared app-server's
// environment would still hold, so it is refused rather than ignored.
func TestSharedServerRefusesUnsetEnv(t *testing.T) {
	spawner := agent.SpawnFunc(func(context.Context, agent.Command) (agent.Process, error) {
		t.Error("app-server started for a refused config")
		return nil, errors.New("refused")
	})
	p := codex.New(codex.Options{Spawner: spawner, SharedServer: true})
	defer p.Close()
	_, err := p.StartSession(context.Background(), agent.Config{UnsetEnv: []string{"SECRET_TOKEN"}})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("StartSession with UnsetEnv on a shared server = %v", err)
	}
}

// Plan mode promises no changes, so a thread starts in it read-only.
func TestPlanModeStartsReadOnly(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		start := startThread(t, h)
		if start == nil {
			return
		}
		if fakeproc.Str(start, "params", "sandbox") != "read-only" || fakeproc.Str(start, "params", "approvalPolicy") != "on-request" {
			t.Errorf("plan thread/start = %v", start["params"])
		}
		h.WaitClosed()
	})
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{PermissionMode: agent.PermissionPlan})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if sess.Info().PermissionMode != agent.PermissionPlan {
		t.Errorf("mode = %s", sess.Info().PermissionMode)
	}
}
