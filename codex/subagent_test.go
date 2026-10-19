package codex_test

import (
	"context"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

const child = "child-1"

func childNotify(h *fakeproc.Harness, method string, params map[string]any) {
	params["threadId"] = child
	h.Notify(method, params)
}

func TestSubagentThreadsAndSharedEvents(t *testing.T) {
	stopped := make(chan map[string]any, 1)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		h.Reply(ts, map[string]any{"turn": map[string]any{"id": "t1", "status": "inProgress", "items": []any{}}})
		turnStarted(h, "t1")
		// The child's first messages arrive before anything names it; its
		// turn chains to t1 through rootTurnId.
		childNotify(h, "thread/status/changed", map[string]any{"status": map[string]any{"type": "active", "activeFlags": []any{}}})
		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "rootTurnId": "t1", "status": "inProgress", "items": []any{}}})
		item(h, "item/completed", "t1", map[string]any{"type": "subAgentActivity", "id": "act1", "kind": "started", "agentThreadId": child,
			"agentPath": "/root/bug_hunter", "model": "gpt-mini", "reasoningEffort": "low"})
		childNotify(h, "item/started", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "cc1", "command": "grep", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
		childNotify(h, "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "commandExecution", "id": "cc1", "command": "grep", "cwd": "/w", "status": "completed", "commandActions": []any{}}})
		childNotify(h, "item/agentMessage/delta", map[string]any{"turnId": "ct1", "itemId": "cm1", "delta": "two"})
		childNotify(h, "item/completed", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "agentMessage", "id": "cm1", "text": "two bugs", "phase": "final_answer"}})

		stop := h.RecvWhere(fakeproc.Method("turn/interrupt"))
		stopped <- stop
		h.Reply(stop, map[string]any{})
		childNotify(h, "turn/completed", map[string]any{"turn": map[string]any{"id": "ct1", "status": "interrupted", "items": []any{}, "durationMs": 3000}})

		h.Notify("account/rateLimits/updated", map[string]any{"rateLimits": map[string]any{
			"primary": map[string]any{"usedPercent": 42, "windowDurationMins": 300, "resetsAt": 1800000000}}})
		h.Notify("hook/completed", map[string]any{"threadId": thread, "turnId": "t1", "run": map[string]any{"id": "r1", "eventName": "stop", "sourcePath": "/h.json", "status": "completed", "entries": []any{map[string]any{"kind": "context", "text": "ok"}}}})
		h.Notify("thread/goal/updated", map[string]any{"threadId": thread, "turnId": "t1", "goal": map[string]any{"objective": "ship", "status": "active", "tokensUsed": 10, "tokenBudget": 100}})
		turnCompleted(h, "t1", "completed", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	turn, err := sess.Send(ctx, agent.Text("hunt bugs"))
	if err != nil {
		t.Fatal(err)
	}

	var task agent.Task
	var childCmd, childMsg agent.Item
	var delta *agent.TextDelta
	var limits []agent.RateLimit
	var hook *agent.HookRun
	var goal *agent.Goal
	asked := false
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case ev := <-sess.Events():
			switch ev := ev.(type) {
			case *agent.TaskUpdated:
				task = ev.Task
				if task.Output == "two bugs" && !asked {
					asked = true
					go func() {
						if err := sess.StopTask(ctx, child); err != nil {
							t.Errorf("StopTask: %v", err)
						}
					}()
				}
			case *agent.ItemCompleted:
				if ev.Item.ID == "cc1" {
					childCmd = ev.Item
				}
				if ev.Item.ID == "cm1" {
					childMsg = ev.Item
				}
			case *agent.TextDelta:
				if ev.SubagentID != "" {
					delta = ev
				}
			case *agent.RateLimitUpdated:
				limits = ev.Limits
			case *agent.HookRun:
				hook = ev
			case *agent.GoalUpdated:
				goal = ev.Goal
			case *agent.TurnCompleted:
				done = true
			}
		case <-deadline:
			t.Fatalf("turn never completed; task %+v", task)
		}
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	stop := <-stopped
	if fakeproc.Str(stop, "params", "threadId") != child || fakeproc.Str(stop, "params", "turnId") != "ct1" {
		t.Errorf("stop = %v", stop)
	}
	sa := task.Subagent
	if task.ID != child || task.Status != agent.TaskStopped || task.ToolCallID != "act1" || task.Output != "two bugs" || task.ToolUses != 1 ||
		task.Duration != 3*time.Second || sa == nil || sa.Type != "bug_hunter" || sa.Model != "gpt-mini" || sa.Effort != agent.EffortLow {
		t.Errorf("task = %+v subagent %+v", task, sa)
	}
	if childCmd.SubagentID != child || childCmd.ParentID != "act1" || childMsg.SubagentID != child || delta == nil || delta.SubagentID != child {
		t.Errorf("child items: cmd %+v msg %+v delta %+v", childCmd, childMsg, delta)
	}
	if len(limits) != 1 || limits[0].UsedPercent != 42 || limits[0].Window != 5*time.Hour {
		t.Errorf("limits = %+v", limits)
	}
	if hook == nil || !hook.Done || hook.Event != "stop" || hook.Output != "ok" || hook.Outcome != agent.HookSucceeded {
		t.Errorf("hook = %+v", hook)
	}
	if goal == nil || goal.Objective != "ship" || goal.TokenBudget != 100 {
		t.Errorf("goal = %+v", goal)
	}
}

func TestSubagentRequestsNameTheSubagent(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		h.Reply(ts, map[string]any{"turn": map[string]any{"id": "t1", "status": "inProgress", "items": []any{}}})
		turnStarted(h, "t1")
		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "rootTurnId": "t1", "status": "inProgress", "items": []any{}}})
		h.Send(map[string]any{"id": 9, "method": "item/commandExecution/requestApproval", "params": map[string]any{
			"threadId": child, "turnId": "ct1", "itemId": "cc1", "command": "rm x", "cwd": "/w"}})
		h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(9) })
		h.Send(map[string]any{"id": 10, "method": "item/tool/requestUserInput", "params": map[string]any{
			"threadId": child, "turnId": "ct1", "itemId": "cq1", "questions": []any{map[string]any{"id": "q", "header": "h", "question": "which?"}}}})
		h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(10) })
		h.Send(map[string]any{"id": 11, "method": "item/commandExecution/requestApproval", "params": map[string]any{
			"threadId": thread, "turnId": "t1", "itemId": "c1", "command": "ls", "cwd": "/w"}})
		h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(11) })
		turnCompleted(h, "t1", "completed", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	var subagents []string
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{Handlers: agent.Handlers{
		Permission: func(_ context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			subagents = append(subagents, "permission:"+req.SubagentID)
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		},
		UserInput: func(_ context.Context, req *agent.UserInputRequest) agent.UserInputResponse {
			subagents = append(subagents, "question:"+req.SubagentID)
			return agent.UserInputResponse{Answers: map[string][]string{"q": {"a"}}}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(subagents, ","), "permission:"+child+",question:"+child+",permission:"; got != want {
		t.Errorf("request subagents = %s, want %s", got, want)
	}
	if err := sess.StopTask(ctx, "no-such-task"); err == nil {
		t.Error("StopTask of an unknown task succeeded")
	}
}

func TestSubagentFileChangeApprovalCarriesChanges(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		h.Reply(ts, map[string]any{"turn": map[string]any{"id": "t1", "status": "inProgress", "items": []any{}}})
		turnStarted(h, "t1")
		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "rootTurnId": "t1", "status": "inProgress", "items": []any{}}})
		childNotify(h, "item/started", map[string]any{"turnId": "ct1", "item": map[string]any{"type": "fileChange", "id": "cf1", "status": "inProgress",
			"changes": []any{map[string]any{"path": "/w/a.go", "diff": "+x", "kind": map[string]any{"type": "add"}}}}})
		h.Send(map[string]any{"id": 9, "method": "item/fileChange/requestApproval", "params": map[string]any{
			"threadId": child, "turnId": "ct1", "itemId": "cf1"}})
		h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(9) })
		turnCompleted(h, "t1", "completed", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	var got *agent.PermissionRequest
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{Handlers: agent.Handlers{
		Permission: func(_ context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			got = req
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.SubagentID != child || len(got.Changes) != 1 || got.Changes[0].Path != "/w/a.go" || got.Changes[0].Kind != agent.FileAdded {
		t.Errorf("request = %+v", got)
	}
}
