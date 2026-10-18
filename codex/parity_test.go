package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// collect reads events until stop returns true or the deadline passes.
func collect(t *testing.T, sess agent.Session, stop func(agent.Event) bool) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if stop(ev) {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out; %d events", len(out))
		}
	}
}

func isTurnCompleted(ev agent.Event) bool {
	_, ok := ev.(*agent.TurnCompleted)
	return ok
}

func startTurn(h *fakeproc.Harness, id string) map[string]any {
	ts := h.RecvWhere(fakeproc.Method("turn/start"))
	h.Reply(ts, map[string]any{"turn": map[string]any{"id": id, "status": "inProgress", "items": []any{}}})
	turnStarted(h, id)
	return ts
}

// waitFor waits until the harness script closed done.
func waitFor(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("harness script did not finish")
	}
}

func TestInterruptEndsRunningItemsAndStopsSubagents(t *testing.T) {
	scripted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		startTurn(h, "t1")
		h.Notify("item/plan/delta", map[string]any{"threadId": thread, "turnId": "t1", "itemId": "p1", "delta": "1. look"})
		item(h, "item/started", "t1", map[string]any{"type": "commandExecution", "id": "c1", "command": "sleep 9", "cwd": "/w", "status": "inProgress", "processId": "pty-7", "commandActions": []any{}})
		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "rootTurnId": "t1", "status": "inProgress", "items": []any{}}})
		item(h, "item/completed", "t1", map[string]any{"type": "subAgentActivity", "id": "act1", "kind": "started", "agentThreadId": child, "agentPath": "/root/w"})

		stops := map[string]string{}
		for len(stops) < 2 {
			m := h.RecvWhere(fakeproc.Method("turn/interrupt"))
			if m == nil {
				return
			}
			stops[fakeproc.Str(m, "params", "threadId")] = fakeproc.Str(m, "params", "turnId")
			h.Reply(m, map[string]any{})
		}
		if stops[thread] != "t1" || stops[child] != "ct1" {
			t.Errorf("interrupts = %v", stops)
		}
		childNotify(h, "turn/completed", map[string]any{"turn": map[string]any{"id": "ct1", "status": "interrupted", "items": []any{}}})
		// Codex finished the turn while the interrupt was in flight.
		turnCompleted(h, "t1", "completed", nil)
		// A subagent turn that starts after its parent turn stopped.
		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct2", "rootTurnId": "t1", "status": "inProgress", "items": []any{}}})
		late := h.RecvWhere(fakeproc.Method("turn/interrupt"))
		if fakeproc.Str(late, "params", "threadId") != child || fakeproc.Str(late, "params", "turnId") != "ct2" {
			t.Errorf("late interrupt = %v", late)
		}
		h.Reply(late, map[string]any{})
		close(scripted)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	turn, err := sess.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	var plan *agent.PlanDelta
	var ended agent.Item
	sawTask := false
	events := collect(t, sess, func(ev agent.Event) bool {
		switch ev := ev.(type) {
		case *agent.PlanDelta:
			plan = ev
		case *agent.TextDelta:
			t.Errorf("plan delta arrived as text: %+v", ev)
		case *agent.TaskUpdated:
			if ev.Task.ID == child && !sawTask {
				sawTask = true
				go func() {
					if err := sess.Interrupt(ctx); err != nil {
						t.Errorf("Interrupt: %v", err)
					}
				}()
			}
		case *agent.ItemCompleted:
			if ev.Item.ID == "c1" {
				ended = ev.Item
			}
		}
		return isTurnCompleted(ev)
	})
	res, err := turn.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnInterrupted {
		t.Errorf("status = %s", res.Status)
	}
	if plan == nil || plan.ItemID != "p1" || plan.Delta != "1. look" {
		t.Errorf("plan delta = %+v", plan)
	}
	if ended.Status != agent.ItemCancelled || !strings.Contains(string(ended.Tool.Input), `"processId":"pty-7"`) {
		t.Errorf("dangling item = %+v", ended)
	}
	found := false
	for _, it := range res.Items {
		found = found || it.ID == "c1"
	}
	if !found || len(events) == 0 {
		t.Errorf("result items = %+v", res.Items)
	}
	waitFor(t, scripted)
}

func TestFailureRateLimitsRetriesAndUsage(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		rl := h.RecvWhere(fakeproc.Method("account/rateLimits/read"))
		h.Reply(rl, map[string]any{
			"rateLimits": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 10}},
			"rateLimitsByLimitId": map[string]any{
				"codex": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300}},
				"spark": map[string]any{"limitId": "spark", "primary": map[string]any{"usedPercent": 50}},
			},
		})
		startTurn(h, "t1")
		h.Notify("account/rateLimits/updated", map[string]any{"rateLimits": map[string]any{"limitId": "codex",
			"secondary": map[string]any{"usedPercent": 100, "resetsAt": 1800000000}}})
		h.Notify("account/rateLimits/updated", map[string]any{"rateLimits": map[string]any{"limitId": "spark", "limitName": "Spark",
			"primary": map[string]any{"usedPercent": 60}}})
		h.Notify("error", map[string]any{"threadId": thread, "turnId": "t1", "willRetry": true, "error": map[string]any{
			"message": "Reconnecting... 2/5", "codexErrorInfo": map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": 502}}}})
		h.Notify("error", map[string]any{"threadId": thread, "turnId": "t1", "willRetry": false, "error": map[string]any{
			"message": "stream broke", "additionalDetails": "proxy reset"}})
		h.Notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": "old", "tokenUsage": map[string]any{
			"total": map[string]any{"totalTokens": 1000, "inputTokens": 1000}, "last": map[string]any{"inputTokens": 1000}}})
		h.Notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": "t1", "tokenUsage": map[string]any{
			"total": map[string]any{"totalTokens": 1150, "inputTokens": 1100, "outputTokens": 50}, "last": map[string]any{"inputTokens": 100, "outputTokens": 50}}})
		// A revert lowered the total: the request's own usage counts.
		h.Notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": "t1", "tokenUsage": map[string]any{
			"total": map[string]any{"totalTokens": 500, "inputTokens": 480, "outputTokens": 20}, "last": map[string]any{"inputTokens": 7, "outputTokens": 3}}})
		item(h, "item/started", "t1", map[string]any{"type": "mcpToolCall", "id": "m1", "server": "s", "tool": "x", "status": "inProgress", "arguments": map[string]any{}})
		turnCompleted(h, "t1", "failed", map[string]any{"message": "limit", "codexErrorInfo": "usageLimitExceeded"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	limits, err := sess.RateLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(limits) != 2 || limits[0].LimitID != "codex" || limits[0].Window != 5*time.Hour || limits[1].LimitID != "spark" {
		t.Errorf("RateLimits = %+v", limits)
	}
	turn, err := sess.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	var main []agent.RateLimit
	var other *codex.OtherRateLimitUpdated
	var retry *agent.Retry
	var notice *agent.Notice
	var usage []agent.Usage
	var failed agent.Item
	collect(t, sess, func(ev agent.Event) bool {
		switch ev := ev.(type) {
		case *agent.RateLimitUpdated:
			main = ev.Limits
		case *codex.OtherRateLimitUpdated:
			other = ev
		case *agent.Retry:
			retry = ev
		case *agent.Notice:
			if ev.Level == agent.NoticeError {
				notice = ev
			}
		case *agent.UsageUpdated:
			usage = append(usage, ev.Usage)
		case *agent.ItemCompleted:
			if ev.Item.ID == "m1" {
				failed = ev.Item
			}
		}
		return isTurnCompleted(ev)
	})
	res, err := turn.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	if len(main) != 2 || main[0].UsedPercent != 10 || main[1].Status != agent.RateLimitExceeded || main[1].LimitID != "codex" {
		t.Errorf("merged limits = %+v", main)
	}
	if other == nil || other.LimitID != "spark" || other.LimitName != "Spark" || len(other.Limits) != 1 || other.Limits[0].UsedPercent != 60 {
		t.Errorf("other limits = %+v", other)
	}
	if retry == nil || retry.Attempt != 2 || retry.MaxAttempts != 5 || retry.Code != "responseStreamDisconnected" || retry.HTTPStatus != 502 {
		t.Errorf("retry = %+v", retry)
	}
	if notice == nil || notice.Message != "stream broke: proxy reset" {
		t.Errorf("notice = %+v", notice)
	}
	if len(usage) != 2 || usage[0].OutputTokens != 50 || usage[1].OutputTokens != 53 || res.Usage.OutputTokens != 53 || res.Usage.InputTokens != 107 {
		t.Errorf("usage = %+v result %+v", usage, res.Usage)
	}
	var terr *agent.TurnError
	if !errors.As(res.Err, &terr) || terr.Code != "usageLimitExceeded" || !terr.Retryable || !terr.ResetsAt.Equal(time.Unix(1800000000, 0)) {
		t.Errorf("turn error = %+v", res.Err)
	}
	if failed.Status != agent.ItemFailed || !failed.Tool.IsError {
		t.Errorf("failed item = %+v", failed)
	}
}

func TestTurnOptionsAsyncQuestionsAndContextRestore(t *testing.T) {
	scripted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		ts := startTurn(h, "t1")
		p := ts["params"].(map[string]any)
		if p["approvalPolicy"] != "never" || fakeproc.Str(p, "sandboxPolicy", "type") != "readOnly" || p["approvalsReviewer"] != "user" ||
			p["cwd"] != "/x" || p["serviceTier"] != "fast" || p["serviceTierForTurn"] != nil || fakeproc.Str(p, "additionalContext", "repo", "value") != "v" {
			t.Errorf("turn/start params = %v", p)
		}
		item(h, "item/completed", "t1", map[string]any{"type": "agentMessage", "id": "a1", "text": "Which?", "delivery": "async",
			"questions": []any{map[string]any{"title": "Pick a color", "options": []any{"red", "blue"}}}})
		turnCompleted(h, "t1", "completed", nil)

		answer := startTurn(h, "t2")
		if fakeproc.Str(answer, "params", "input", 0, "text") != "blue" {
			t.Errorf("answer turn = %v", answer)
		}
		turnCompleted(h, "t2", "completed", nil)

		compact := h.RecvWhere(fakeproc.Method("thread/compact/start"))
		h.Reply(compact, map[string]any{})
		item(h, "item/started", "t3", map[string]any{"type": "contextCompaction", "id": "k1"})
		item(h, "item/completed", "t3", map[string]any{"type": "contextCompaction", "id": "k1"})
		inject := h.RecvWhere(fakeproc.Method("thread/inject_items"))
		if fakeproc.Str(inject, "params", "items", 0, "role") != "developer" || fakeproc.Str(inject, "params", "items", 0, "content", 0, "text") != "<repo>v</repo>" ||
			fakeproc.Str(inject, "params", "items", 1, "role") != "user" || fakeproc.Str(inject, "params", "items", 1, "content", 0, "text") != "<external_web>w</external_web>" {
			t.Errorf("inject = %v", inject)
		}
		h.Reply(inject, map[string]any{})
		close(scripted)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("go")}, ProviderOptions: codex.TurnOptions{
		ApprovalPolicy: "never", SandboxPolicy: "read-only", ApprovalsReviewer: "user", Cwd: "/x", StickyServiceTier: "fast",
		AdditionalContext: map[string]any{
			"repo": map[string]any{"kind": "application", "value": "v"},
			"web":  map[string]any{"kind": "untrusted", "value": "w"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var ask *agent.UserInputRequest
	collect(t, sess, func(ev agent.Event) bool {
		if r, ok := ev.(*agent.UserInputRequest); ok {
			ask = r
		}
		return isTurnCompleted(ev)
	})
	if ask == nil || !ask.Async || len(ask.Questions) != 1 || ask.Questions[0].Text != "Pick a color" || len(ask.Questions[0].Options) != 2 {
		t.Fatalf("async question = %+v", ask)
	}
	if err := ask.Answer(ctx, map[string][]string{ask.Questions[0].ID: {"blue"}}); err != nil {
		t.Fatal(err)
	}
	collect(t, sess, isTurnCompleted)
	if err := sess.Compact(ctx, ""); err != nil {
		t.Fatal(err)
	}
	var triggers []string
	collect(t, sess, func(ev agent.Event) bool {
		if c, ok := ev.(*agent.Compaction); ok {
			triggers = append(triggers, c.Trigger)
			return c.Done
		}
		return false
	})
	if strings.Join(triggers, ",") != "manual,manual" {
		t.Errorf("compaction triggers = %v", triggers)
	}
	waitFor(t, scripted)
}

func TestThreadLifecycleAndTypedMethods(t *testing.T) {
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := h.RecvWhere(fakeproc.Method("initialize"))
		if fakeproc.Str(init, "params", "clientInfo", "title") != "My Host" || fakeproc.Get(init, "params", "capabilities", "extensions", "x") != true {
			t.Errorf("initialize = %v", init)
		}
		h.Reply(init, map[string]any{"userAgent": "my-host/0.163.0 (Linux; x86_64)"})
		resume := h.RecvWhere(fakeproc.Method("thread/resume"))
		if fakeproc.Get(resume, "params", "excludeTurns") != true || fakeproc.Str(resume, "params", "personality") != "calm" ||
			fakeproc.Get(resume, "params", "config", "mcp_servers.slow", "tool_timeout_sec") != float64(3900) ||
			fakeproc.Get(resume, "params", "config", "mcp_servers.slow", "startup_timeout_sec") != 2.5 {
			t.Errorf("resume = %v", resume)
		}
		h.Send(map[string]any{"id": resume["id"], "error": map[string]any{"code": -32600, "message": "session th-1 is archived"}})
		un := h.RecvWhere(fakeproc.Method("thread/unarchive"))
		h.Reply(un, map[string]any{})
		resume = h.RecvWhere(fakeproc.Method("thread/resume"))
		h.Reply(resume, map[string]any{"thread": map[string]any{"id": thread, "cwd": "/w"}, "model": "m", "cwd": "/w"})
		skills := h.RecvWhere(fakeproc.Method("skills/list"))
		h.Reply(skills, map[string]any{"data": []any{map[string]any{"cwd": "/w", "errors": []any{}, "skills": []any{
			map[string]any{"name": "lint", "description": "long", "enabled": true, "path": "/s/lint/SKILL.md", "scope": "repo", "interface": map[string]any{"shortDescription": "Lint"}}}}}})

		fork := h.RecvWhere(fakeproc.Method("thread/fork"))
		if fakeproc.Str(fork, "params", "lastTurnId") != "t5" || fakeproc.Get(fork, "params", "personality") != nil || fakeproc.Get(fork, "params", "excludeTurns") != true {
			t.Errorf("fork = %v", fork)
		}
		h.Reply(fork, map[string]any{"thread": map[string]any{"id": "th-2"}, "model": "m"})

		answers := map[string]any{
			"thread/read":                          map[string]any{"thread": map[string]any{"id": thread, "historyMode": "paginated"}},
			"thread/revert":                        map[string]any{"thread": map[string]any{"id": thread}},
			"thread/goal/set":                      map[string]any{},
			"mcpServer/tool/call":                  map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "isError": false},
			"mcpServer/resource/read":              map[string]any{"contents": []any{map[string]any{"uri": "ui://a"}}},
			"thread/backgroundTerminals/terminate": map[string]any{"terminated": true},
			"feedback/upload":                      map[string]any{"threadId": thread},
			"account/rateLimitResetCredit/consume": map[string]any{"outcome": "reset"},
		}
		pages := 0
		for {
			m := h.Recv()
			if m == nil {
				return
			}
			method, params := fakeproc.Str(m, "method"), m["params"]
			switch method {
			case "skills/list":
				h.Reply(m, map[string]any{"data": []any{}})
			case "thread/revert":
				if fakeproc.Str(params, "beforeTurnId") != "t3" {
					t.Errorf("revert = %v", m)
				}
			case "thread/goal/set":
				if fakeproc.Str(params, "status") != "paused" || fakeproc.Get(params, "objective") != nil {
					t.Errorf("goal set = %v", m)
				}
			case "thread/turns/list":
				if fakeproc.Str(params, "sortDirection") != "desc" || fakeproc.Get(params, "limit") != float64(2) {
					t.Errorf("turns list = %v", m)
				}
				h.Reply(m, map[string]any{"data": []any{map[string]any{"id": "t3", "status": "completed", "items": []any{
					map[string]any{"type": "agentMessage", "id": "a", "text": "hi"}}, "startedAt": 1700000000, "durationMs": 1500}}, "nextCursor": "c2"})
				continue
			case "mcpServer/tool/call":
				if fakeproc.Str(params, "tool") != "t" || fakeproc.Get(params, "arguments", "a") != float64(1) || fakeproc.Get(params, "_meta", "k") != "v" {
					t.Errorf("tool call = %v", m)
				}
			case "mcpServerStatus/list":
				if fakeproc.Str(params, "serverName") != "s" || fakeproc.Str(params, "detail") != "toolsAndAuthOnly" || fakeproc.Str(params, "threadId") != thread {
					t.Errorf("status = %v", m)
				}
				pages++
				if pages == 1 {
					h.Reply(m, map[string]any{"data": []any{map[string]any{"name": "s"}}, "nextCursor": "n"})
				} else {
					if fakeproc.Str(params, "cursor") != "n" {
						t.Errorf("status page 2 = %v", m)
					}
					h.Reply(m, map[string]any{"data": []any{map[string]any{"name": "s2"}}})
				}
				continue
			case "thread/backgroundTerminals/list":
				h.Reply(m, map[string]any{"data": []any{map[string]any{"processId": "pty-1", "itemId": "c1", "command": "sleep", "cwd": "/w"}}})
				continue
			case "feedback/upload":
				if fakeproc.Str(params, "classification") != "bug" || fakeproc.Get(params, "includeLogs") != true || fakeproc.Str(params, "reason") != "broke" {
					t.Errorf("feedback = %v", m)
				}
			case "thread/unsubscribe", "turn/interrupt":
				h.Reply(m, map[string]any{})
				continue
			}
			if a, ok := answers[method]; ok {
				h.Reply(m, a)
			} else if m["id"] != nil {
				h.Reply(m, map[string]any{})
			}
		}
	})
	ctx := context.Background()
	cfg := agent.Config{Resume: thread, MCPServers: map[string]agent.MCPServer{
		"slow": {URL: "http://x", ToolTimeout: 3900 * time.Second, StartupTimeout: 2500 * time.Millisecond}},
		ProviderOptions: &codex.SessionOptions{Personality: "calm"}}
	p := codex.New(codex.Options{Spawner: spawner, ClientName: "my-host", ClientTitle: "My Host",
		InitializeCapabilities: map[string]any{"extensions": map[string]any{"x": true}}})
	sess, err := p.StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	h := <-started
	if !slices.Contains(h.Cmd.Env, "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=my-host") {
		t.Errorf("env = %v", h.Cmd.Env)
	}
	if sess.Info().Version != "0.163.0" {
		t.Errorf("version = %q", sess.Info().Version)
	}
	collect(t, sess, func(ev agent.Event) bool {
		u, ok := ev.(*agent.SessionUpdated)
		return ok && len(u.Info.Commands) > 0
	})
	if cmds := sess.Info().Commands; len(cmds) != 1 || cmds[0].Path != "/s/lint/SKILL.md" || cmds[0].Scope != "repo" || cmds[0].Description != "Lint" {
		t.Errorf("commands = %+v", cmds)
	}
	if !sess.Capabilities().ForkAtTurn {
		t.Error("capabilities lack ForkAtTurn")
	}
	forked, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: "t5"})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()
	if forked.ID() != "th-2" {
		t.Errorf("fork id = %s", forked.ID())
	}

	if err := sess.Rewind(ctx, "t3"); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetGoalStatus(ctx, "paused"); err != nil {
		t.Fatal(err)
	}
	page, err := sess.ListTurns(ctx, codex.ListTurnsOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "c2" || len(page.Turns) != 1 || page.Turns[0].ID != "t3" || page.Turns[0].Duration != 1500*time.Millisecond ||
		len(page.Turns[0].Items) != 1 || page.Turns[0].Items[0].Text != "hi" || page.Turns[0].StartedAt.Unix() != 1700000000 {
		t.Errorf("turns = %+v", page)
	}
	res, err := sess.CallMCPTool(ctx, "s", "t", map[string]any{"a": 1}, map[string]any{"k": "v"})
	if err != nil || len(res.Content) != 1 {
		t.Errorf("CallMCPTool = %+v, %v", res, err)
	}
	if raw, err := sess.ReadMCPResource(ctx, "s", "ui://a"); err != nil || !strings.Contains(string(raw), "ui://a") {
		t.Errorf("ReadMCPResource = %s, %v", raw, err)
	}
	if st, err := sess.MCPServerStatus(ctx, codex.MCPServerStatusOptions{ServerName: "s", Detail: "toolsAndAuthOnly"}); err != nil || len(st) != 2 {
		t.Errorf("MCPServerStatus = %s, %v", st, err)
	}
	if terms, err := sess.BackgroundTerminals(ctx); err != nil || len(terms) != 1 || terms[0].ProcessID != "pty-1" {
		t.Errorf("BackgroundTerminals = %+v, %v", terms, err)
	}
	if ok, err := sess.TerminateBackgroundTerminal(ctx, "pty-1"); err != nil || !ok {
		t.Errorf("TerminateBackgroundTerminal = %v, %v", ok, err)
	}
	if err := sess.UploadFeedback(ctx, "bug", "broke", true); err != nil {
		t.Fatal(err)
	}
	if out, err := sess.Client().ConsumeRateLimitResetCredit(ctx, "key-1"); err != nil || out != "reset" {
		t.Errorf("ConsumeRateLimitResetCredit = %q, %v", out, err)
	}
}

func TestElicitationMetaGoalTimeAndItemPresentation(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		startTurn(h, "t1")
		h.Send(map[string]any{"id": 9, "method": "mcpServer/elicitation/request", "params": map[string]any{"threadId": thread, "turnId": "t1",
			"serverName": "s", "mode": "form", "message": "ok?", "requestedSchema": map[string]any{"type": "object"}, "_meta": map[string]any{"persist": []any{"session"}}}})
		ans := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(9) })
		if fakeproc.Str(ans, "result", "_meta", "persist") != "session" {
			t.Errorf("elicitation answer = %v", ans)
		}
		h.Notify("thread/goal/updated", map[string]any{"threadId": thread, "goal": map[string]any{"objective": "o", "status": "paused", "tokensUsed": 1, "timeUsedSeconds": 90}})
		item(h, "item/completed", "t1", map[string]any{"type": "webSearch", "id": "w1", "query": "", "action": map[string]any{"type": "openPage", "url": "https://x"}})
		item(h, "item/completed", "t1", map[string]any{"type": "webSearch", "id": "w2", "query": "", "action": map[string]any{"type": "search", "queries": []any{"a", "b"}}})
		item(h, "item/completed", "t1", map[string]any{"type": "dynamicToolCall", "id": "d1", "namespace": "files", "tool": "read", "status": "completed", "arguments": map[string]any{}})
		item(h, "item/completed", "t1", map[string]any{"type": "mcpToolCall", "id": "m1", "server": "s", "tool": "x", "status": "completed", "arguments": map[string]any{},
			"appContext": map[string]any{"connectorId": "c", "actionName": "Create issue", "appName": "Tracker"}})
		childNotify(h, "turn/started", map[string]any{"turn": map[string]any{"id": "ct1", "rootTurnId": "t1", "status": "inProgress", "items": []any{}}})
		childNotify(h, "thread/settings/updated", map[string]any{"threadSettings": map[string]any{"model": "gpt-mini", "effort": "high"}})
		item(h, "item/completed", "t1", map[string]any{"type": "collabAgentToolCall", "id": "s1", "tool": "wait", "status": "completed", "receiverThreadIds": []any{child},
			"agentsStates": map[string]any{child: map[string]any{"status": "notFound"}}})
		turnCompleted(h, "t1", "completed", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{Handlers: agent.Handlers{
		Elicitation: func(ctx context.Context, req *agent.ElicitationRequest) agent.ElicitationResponse {
			if !strings.Contains(string(req.MCPMeta), "persist") {
				t.Errorf("request meta = %s", req.MCPMeta)
			}
			return agent.ElicitationResponse{Action: agent.ElicitationAccept, Content: json.RawMessage(`{}`), MCPMeta: json.RawMessage(`{"persist":"session"}`)}
		}}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Send(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	var goal *agent.Goal
	items := map[string]agent.Item{}
	var task agent.Task
	collect(t, sess, func(ev agent.Event) bool {
		switch ev := ev.(type) {
		case *agent.GoalUpdated:
			goal = ev.Goal
		case *agent.ItemCompleted:
			items[ev.Item.ID] = ev.Item
		case *agent.TaskUpdated:
			task = ev.Task
		}
		return isTurnCompleted(ev)
	})
	if goal == nil || goal.TimeUsed != 90*time.Second || goal.Status != "paused" {
		t.Errorf("goal = %+v", goal)
	}
	if items["w1"].Tool.URL != "https://x" || items["w2"].Tool.Query != "a\nb" || items["d1"].Tool.Name != "files.read" ||
		items["m1"].Tool.DisplayName != "Create issue" || items["m1"].Tool.ServerDisplayName != "Tracker" {
		t.Errorf("items = %+v %+v %+v %+v", items["w1"].Tool, items["w2"].Tool, items["d1"].Tool, items["m1"].Tool)
	}
	if task.Status != agent.TaskFailed || task.Subagent.Model != "gpt-mini" || task.Subagent.Effort != agent.EffortHigh {
		t.Errorf("task = %+v subagent %+v", task, task.Subagent)
	}
}

func TestCheck(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if len(h.Cmd.Args) > 0 && h.Cmd.Args[0] == "--version" {
			h.SendRaw("codex-cli 0.163.0")
			return
		}
		init := h.RecvWhere(fakeproc.Method("initialize"))
		h.Reply(init, map[string]any{"userAgent": "x/0.163.0"})
		acct := h.RecvWhere(fakeproc.Method("account/read"))
		h.Reply(acct, map[string]any{"account": map[string]any{"type": "chatgpt", "email": "a@b.c", "planType": "plus"}, "requiresOpenaiAuth": true})
		h.WaitClosed()
	})
	ctx := context.Background()
	st, err := codex.New(codex.Options{Spawner: spawner}).Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Version != "0.163.0" || st.Err != nil || st.Auth == nil || !st.Auth.Authenticated ||
		st.Auth.Method != "chatgpt" || st.Auth.Plan != "plus" || st.Auth.Email != "a@b.c" {
		t.Errorf("status = %+v auth %+v", st, st.Auth)
	}

	missing := agent.SpawnFunc(func(ctx context.Context, cmd agent.Command) (agent.Process, error) { return nil, exec.ErrNotFound })
	st, err = codex.New(codex.Options{Spawner: missing}).Check(ctx)
	if err != nil || st.Installed || st.Err == nil {
		t.Errorf("missing status = %+v, %v", st, err)
	}
}
