package claude_test

import (
	"context"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

func TestSubagentsPlansAndSharedEvents(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		user := h.RecvWhere(isType("user"))
		uuid := fakeproc.Str(user, "uuid")
		h.Send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
		// The main agent spawns a background subagent.
		h.Send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "m1",
			"content": []any{map[string]any{"type": "tool_use", "id": "spawn1", "name": "Agent", "input": map[string]any{"description": "Find bugs", "prompt": "look", "subagent_type": "Explore", "effort": "low"}}}}})
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "ag1", "tool_use_id": "spawn1", "description": "Find bugs",
			"subagent_type": "Explore", "prompt": "look", "is_backgrounded": true, "spawn_depth": 1, "task_type": "local_agent"})
		h.Send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "spawn1", "content": "launched"}}},
			"tool_use_result": map[string]any{"agentId": "ag1", "resolvedModel": "haiku-x", "status": "async_launched"}})
		// The subagent works; its frames carry agent_id.
		h.Send(map[string]any{"type": "assistant", "parent_tool_use_id": "spawn1", "agent_id": "ag1", "message": map[string]any{"id": "m2",
			"content": []any{map[string]any{"type": "tool_use", "id": "sub-bash", "name": "Bash", "input": map[string]any{"command": "grep x"}}}}})
		h.Send(map[string]any{"type": "system", "subtype": "task_progress", "task_id": "ag1", "tool_use_id": "spawn1", "description": "Finding",
			"usage": map[string]any{"total_tokens": 500, "tool_uses": 1, "duration_ms": 2000}, "last_tool_name": "Bash", "summary": "grepping"})
		// Task list tools feed the plan.
		h.Send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "m3",
			"content": []any{map[string]any{"type": "tool_use", "id": "tc1", "name": "TaskCreate", "input": map[string]any{"subject": "Write tests", "description": "d"}}}}})
		h.Send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tc1", "content": "ok"}}},
			"tool_use_result": map[string]any{"task": map[string]any{"id": "1", "subject": "Write tests"}}})
		h.Send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "m4",
			"content": []any{map[string]any{"type": "tool_use", "id": "tu1", "name": "TaskUpdate", "input": map[string]any{"taskId": "1", "status": "in_progress"}}}}})
		h.Send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "ok"}}},
			"tool_use_result": map[string]any{"success": true, "taskId": "1"}})
		h.Send(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed_warning", "rateLimitType": "five_hour",
			"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 0.9, "resetsAt": 1800000000}, "seven_day": map[string]any{"utilization": 0.1, "resetsAt": 1800500000}}}})
		h.Send(map[string]any{"type": "system", "subtype": "hook_response", "hook_id": "h1", "hook_name": "lint", "hook_event": "PostToolUse", "output": "clean", "outcome": "success"})
		h.Send(map[string]any{"type": "active_goal", "value": map[string]any{"condition": "all tests pass", "iterations": 2}})
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "spawned", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		// The background subagent finishes after the turn.
		h.Send(map[string]any{"type": "system", "subtype": "task_notification", "task_id": "ag1", "tool_use_id": "spawn1", "status": "completed",
			"summary": "found 2 bugs", "usage": map[string]any{"total_tokens": 900, "tool_uses": 3, "duration_ms": 5000}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	var final agent.Task
	var subItem agent.Item
	var plan []agent.PlanStep
	var limits []agent.RateLimit
	var hook *agent.HookRun
	var goal *agent.Goal
	timeout := time.After(5 * time.Second)
	for final.Status != agent.TaskCompleted {
		select {
		case ev := <-sess.Events():
			switch ev := ev.(type) {
			case *agent.TaskUpdated:
				final = ev.Task
			case *agent.ItemStarted:
				if ev.Item.ID == "sub-bash" {
					subItem = ev.Item
				}
			case *agent.PlanUpdated:
				plan = ev.Steps
			case *agent.RateLimitUpdated:
				limits = ev.Limits
			case *agent.HookRun:
				hook = ev
			case *agent.GoalUpdated:
				goal = ev.Goal
			}
		case <-timeout:
			t.Fatalf("subagent never completed; last task %+v", final)
		}
	}
	sa := final.Subagent
	if final.ID != "ag1" || final.Kind != agent.TaskSubagent || final.ToolCallID != "spawn1" || final.Output != "found 2 bugs" ||
		final.ToolUses != 3 || final.Duration != 5*time.Second || sa == nil || sa.Type != "Explore" || sa.Prompt != "look" ||
		!sa.Background || sa.Depth != 1 || sa.Model != "haiku-x" || sa.Effort != agent.EffortLow {
		t.Errorf("task = %+v subagent %+v", final, sa)
	}
	if subItem.SubagentID != "ag1" || subItem.ParentID != "spawn1" {
		t.Errorf("subagent item = %+v", subItem)
	}
	if len(plan) != 1 || plan[0].ID != "1" || plan[0].Status != agent.StepInProgress {
		t.Errorf("plan = %+v", plan)
	}
	if len(limits) != 2 || limits[0].Name != "five_hour" || limits[0].Status != agent.RateLimitWarning || limits[0].UsedPercent != 90 || limits[0].Window != 5*time.Hour {
		t.Errorf("limits = %+v", limits)
	}
	if hook == nil || !hook.Done || hook.Event != "PostToolUse" || hook.Output != "clean" || hook.Outcome != agent.HookSucceeded {
		t.Errorf("hook = %+v", hook)
	}
	if goal == nil || goal.Objective != "all tests pass" || goal.Iterations != 2 {
		t.Errorf("goal = %+v", goal)
	}
}

func TestSubagentTodoWriteLeavesPlan(t *testing.T) {
	todos := func(text string) map[string]any {
		return map[string]any{"todos": []any{map[string]any{"content": text, "status": "pending"}}}
	}
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		h.Send(assistantMsg("m1", "w1", nil, map[string]any{"type": "tool_use", "id": "spawn1", "name": "Agent", "input": map[string]any{"prompt": "go"}}))
		sub := assistantMsg("m2", "w2", "spawn1", map[string]any{"type": "tool_use", "id": "subtodo", "name": "TodoWrite", "input": todos("sub step")})
		sub["agent_id"] = "ag1"
		h.Send(sub)
		h.Send(assistantMsg("m3", "w3", nil, map[string]any{"type": "tool_use", "id": "maintodo", "name": "TodoWrite", "input": todos("main step")}))
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	log := record(sess)
	if _, err := sess.Run(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	log.wait(t, "main plan", func(ev agent.Event) bool { p, ok := ev.(*agent.PlanUpdated); return ok && p.ItemID == "maintodo" })
	for _, ev := range log.all() {
		if p, ok := ev.(*agent.PlanUpdated); ok && p.ItemID != "maintodo" {
			t.Errorf("plan from %s: %+v", p.ItemID, p.Steps)
		}
	}
}

func TestSubagentItemsBeforeTaskStartedCompleteOnce(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		h.Send(assistantMsg("m1", "w1", nil, map[string]any{"type": "tool_use", "id": "spawn1", "name": "Agent", "input": map[string]any{"prompt": "go"}}))
		// The subagent's first frames carry no agent_id and precede task_started.
		h.Send(assistantMsg("m2", "w2", "spawn1", map[string]any{"type": "text", "text": "looking"}))
		h.Send(assistantMsg("m3", "w3", "spawn1", map[string]any{"type": "tool_use", "id": "subls", "name": "Bash", "input": map[string]any{"command": "ls"}}))
		sres := toolResult("w4", "subls", "a.go", false)
		sres["parent_tool_use_id"] = "spawn1"
		h.Send(sres)
		h.Send(map[string]any{"type": "system", "subtype": "task_started", "task_id": "ag1", "tool_use_id": "spawn1", "task_type": "local_agent"})
		h.Send(toolResult("w5", "spawn1", "done", false))
		h.Send(map[string]any{"type": "result", "subtype": "success", "result": "ok", "user_message_uuids": []any{uuid}, "usage": map[string]any{}})
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
	if err != nil {
		t.Fatal(err)
	}
	log.wait(t, "spawn completed", completedItem("spawn1"))
	completed := map[string]bool{}
	for _, ev := range log.all() {
		switch ev := ev.(type) {
		case *agent.ItemCompleted:
			if ev.Item.ParentID == "spawn1" && ev.Item.SubagentID != "ag1" {
				t.Errorf("completed without subagent: %+v", ev.Item)
			}
			completed[ev.Item.ID] = true
		case *agent.ItemUpdated:
			if completed[ev.Item.ID] {
				t.Errorf("update after completion: %+v", ev.Item)
			}
		}
	}
	if !completed["m2/0"] || !completed["subls"] {
		t.Errorf("completed = %v", completed)
	}
	for _, it := range res.Items {
		if it.ParentID == "spawn1" && it.SubagentID != "ag1" {
			t.Errorf("turn result item without subagent: %+v", it)
		}
	}
}

func TestInterruptedTurnEndsOpenItems(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		uuid := startTurn(h)
		stream(h, map[string]any{"type": "message_start", "message": map[string]any{"id": "m1"}})
		stream(h, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		stream(h, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "partial"}})
		stream(h, map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash"}})
		h.Send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": "aborted_streaming",
			"user_message_uuids": []any{uuid}, "usage": map[string]any{}})
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
	res, err := sess.Run(ctx, agent.Text("go"))
	if res == nil || res.Status != agent.TurnInterrupted {
		t.Fatalf("result = %+v, %v", res, err)
	}
	status := map[string]agent.ItemStatus{}
	for _, it := range res.Items {
		status[it.ID] = it.Status
	}
	if status["m1/0"] != agent.ItemCancelled || status["tu1"] != agent.ItemCancelled {
		t.Errorf("turn items = %+v", res.Items)
	}
}
