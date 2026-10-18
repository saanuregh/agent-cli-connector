package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

const thread = "th-1"

// startThread answers initialize and thread/start, sending a warning for
// the thread before thread/start returns.
func startThread(t *testing.T, h *fakeproc.Harness) map[string]any {
	init := h.RecvWhere(fakeproc.Method("initialize"))
	if init == nil {
		return nil
	}
	if fakeproc.Get(init, "params", "capabilities", "experimentalApi") != true {
		t.Errorf("initialize = %v", init)
	}
	h.Reply(init, map[string]any{"userAgent": "fake", "codexHome": "/h", "platformFamily": "unix", "platformOs": "linux"})
	h.RecvWhere(fakeproc.Method("initialized"))
	start := h.RecvWhere(fakeproc.Method("thread/start"))
	h.Notify("warning", map[string]any{"threadId": thread, "message": "early warning"})
	h.Reply(start, map[string]any{
		"thread": map[string]any{"id": thread, "cwd": "/w", "cliVersion": "1.2.3", "name": nil},
		"model":  "gpt-test", "modelProvider": "openai", "cwd": "/w", "reasoningEffort": "low",
	})
	return start
}

func turnStarted(h *fakeproc.Harness, id string) {
	h.Notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": id, "status": "inProgress", "items": []any{}}})
}

func turnCompleted(h *fakeproc.Harness, id, status string, turnErr any) {
	h.Notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": id, "status": status, "items": []any{}, "error": turnErr, "durationMs": 1200}})
}

func item(h *fakeproc.Harness, method, turn string, it map[string]any) {
	h.Notify(method, map[string]any{"threadId": thread, "turnId": turn, "item": it})
}

func TestTurnApprovalsToolsQueueAndSteer(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		start := startThread(t, h)
		if start == nil {
			return
		}
		p := start["params"].(map[string]any)
		if p["approvalPolicy"] != "on-request" || p["sandbox"] != "workspace-write" || fakeproc.Str(p, "dynamicTools", 0, "name") != "add" ||
			fakeproc.Str(p, "config", "model_reasoning_effort") != "low" || !strings.HasPrefix(fakeproc.Str(p, "cwd"), "/") {
			t.Errorf("thread/start params = %v", p)
		}
		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		if fakeproc.Str(ts, "params", "input", 0, "text") != "first" || fakeproc.Str(ts, "params", "effort") != "low" {
			t.Errorf("turn/start = %v", ts)
		}
		h.Reply(ts, map[string]any{"turn": map[string]any{"id": "t1", "status": "inProgress", "items": []any{}}})
		turnStarted(h, "t1")
		item(h, "item/started", "t1", map[string]any{"type": "agentMessage", "id": "m1", "text": "", "phase": "final_answer"})
		h.Notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": "t1", "itemId": "m1", "delta": "Do"})
		h.Notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": "t1", "itemId": "m1", "delta": "ne"})

		item(h, "item/started", "t1", map[string]any{"type": "commandExecution", "id": "c1", "command": "rm x", "cwd": "/w", "status": "inProgress", "commandActions": []any{}})
		h.Send(map[string]any{"id": 7, "method": "item/commandExecution/requestApproval", "params": map[string]any{
			"threadId": thread, "turnId": "t1", "itemId": "c1", "command": "rm x", "cwd": "/w", "reason": "deletes a file",
			"proposedExecpolicyAmendment": []any{"rm", "x"},
			"availableDecisions":          []any{"accept", map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []any{"rm", "x"}}}, "cancel"},
		}})
		ans := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(7) })
		if fakeproc.Get(ans, "result", "decision", "acceptWithExecpolicyAmendment") == nil {
			t.Errorf("approval answer = %v", ans)
		}
		item(h, "item/completed", "t1", map[string]any{"type": "commandExecution", "id": "c1", "command": "rm x", "cwd": "/w", "status": "completed", "aggregatedOutput": "", "exitCode": 0, "commandActions": []any{}})

		h.Send(map[string]any{"id": 8, "method": "item/tool/call", "params": map[string]any{"threadId": thread, "turnId": "t1", "callId": "d1", "tool": "add", "arguments": map[string]any{"a": 2, "b": 3}}})
		tool := h.RecvWhere(func(m map[string]any) bool { return fakeproc.Get(m, "id") == float64(8) })
		if fakeproc.Str(tool, "result", "contentItems", 0, "text") != "5" || fakeproc.Get(tool, "result", "success") != true {
			t.Errorf("tool answer = %v", tool)
		}

		steer := h.RecvWhere(fakeproc.Method("turn/steer"))
		if fakeproc.Str(steer, "params", "expectedTurnId") != "t1" {
			t.Errorf("turn/steer = %v", steer)
		}
		h.Reply(steer, map[string]any{"turnId": "t1"})

		h.Notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": "t1", "tokenUsage": map[string]any{
			"total": map[string]any{"totalTokens": 120, "inputTokens": 100, "cachedInputTokens": 40, "outputTokens": 20},
			"last":  map[string]any{"totalTokens": 120, "inputTokens": 100, "outputTokens": 20}, "modelContextWindow": 5000}})
		item(h, "item/completed", "t1", map[string]any{"type": "agentMessage", "id": "m1", "text": "Done", "phase": "final_answer"})
		turnCompleted(h, "t1", "completed", nil)

		next := h.RecvWhere(fakeproc.Method("turn/start"))
		if fakeproc.Str(next, "params", "input", 0, "text") != "second" {
			t.Errorf("queued turn/start = %v", next)
		}
		h.Reply(next, map[string]any{"turn": map[string]any{"id": "t2", "status": "inProgress", "items": []any{}}})
		turnStarted(h, "t2")
		turnCompleted(h, "t2", "failed", map[string]any{"message": "too long", "codexErrorInfo": "contextWindowExceeded"})
		h.WaitClosed()
	})
	ctx := context.Background()
	cfg := agent.Config{
		Cwd:            "/w",
		Effort:         agent.EffortLow,
		PermissionMode: agent.PermissionDefault,
		Tools: []agent.Tool{{Name: "add", Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
			var in struct{ A, B int }
			_ = json.Unmarshal(call.Arguments, &in)
			return agent.TextResult(strings.TrimSpace(string(rune('0' + in.A + in.B)))), nil
		}}},
		Handlers: agent.Handlers{Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			if req.Command != "rm x" || req.Reason != "deletes a file" || !slices.Contains(req.Decisions, agent.DecisionAllowAlways) {
				t.Errorf("permission request = %+v", req)
			}
			return agent.PermissionResponse{Decision: agent.DecisionAllowAlways}
		}},
	}
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if sess.ID() != thread || sess.Info().Model != "gpt-test" || sess.Info().Version != "1.2.3" {
		t.Errorf("info = %+v", sess.Info())
	}
	events := make(chan agent.Event, 256)
	go func() {
		for ev := range sess.Events() {
			events <- ev
		}
		close(events)
	}()

	first, err := sess.Send(ctx, agent.Text("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.Send(ctx, agent.Text("second"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer}); err != nil {
		t.Fatal(err)
	}
	res, err := first.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != agent.TurnSucceeded || res.Text != "Done" || res.Usage.InputTokens != 60 || res.Usage.CacheReadTokens != 40 || res.Usage.ContextWindow != 5000 {
		t.Errorf("first result = %+v", res)
	}
	res, err = second.Wait(ctx)
	if res == nil {
		t.Fatal(err)
	}
	var terr *agent.TurnError
	if res.Status != agent.TurnFailed || !errors.As(res.Err, &terr) || terr.Code != "contextWindowExceeded" || second.ID() != "t2" {
		t.Errorf("second result = %+v id %s", res, second.ID())
	}

	var order []string
	timeout := time.After(5 * time.Second)
	for len(order) < 3 {
		select {
		case ev := <-events:
			switch ev := ev.(type) {
			case *agent.SessionStarted:
				order = append(order, "started")
			case *agent.Notice:
				if ev.Message == "early warning" {
					order = append(order, "warning")
				}
			case *agent.ItemStarted:
				if ev.Item.ID == "m1" {
					order = append(order, "message")
					if ev.Item.Status != agent.ItemRunning {
						t.Errorf("started message status = %s", ev.Item.Status)
					}
				}
			}
		case <-timeout:
			t.Fatalf("events so far: %v", order)
		}
	}
	if strings.Join(order, ",") != "started,warning,message" {
		t.Errorf("event order = %v", order)
	}
}

func TestPermissionModeMapping(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		start := startThread(t, h)
		if start == nil {
			return
		}
		if fakeproc.Str(start, "params", "approvalPolicy") != "never" || fakeproc.Str(start, "params", "sandbox") != "danger-full-access" ||
			fakeproc.Str(start, "params", "approvalsReviewer") != "user" {
			t.Errorf("bypass mapping = %v", start["params"])
		}
		upd := h.RecvWhere(fakeproc.Method("thread/settings/update"))
		if fakeproc.Str(upd, "params", "sandboxPolicy", "type") != "readOnly" || fakeproc.Get(upd, "params", "collaborationMode") != nil {
			t.Errorf("readOnly settings = %v", upd)
		}
		h.Reply(upd, map[string]any{})
		// Workspace-write without a known policy reads the configured one.
		cfg := h.RecvWhere(fakeproc.Method("config/read"))
		if fakeproc.Str(cfg, "params", "cwd") != "/w" {
			t.Errorf("config/read = %v", cfg)
		}
		h.Reply(cfg, map[string]any{"config": map[string]any{"sandbox_workspace_write": map[string]any{"writable_roots": []any{"/extra"}, "network_access": true}}, "origins": map[string]any{}})
		upd = h.RecvWhere(fakeproc.Method("thread/settings/update"))
		if fakeproc.Str(upd, "params", "collaborationMode", "mode") != "plan" || fakeproc.Str(upd, "params", "collaborationMode", "settings", "model") != "gpt-test" ||
			fakeproc.Get(upd, "params", "collaborationMode", "settings", "developer_instructions") != nil ||
			fakeproc.Str(upd, "params", "sandboxPolicy", "writableRoots", 0) != "/extra" || fakeproc.Get(upd, "params", "sandboxPolicy", "networkAccess") != true {
			t.Errorf("plan settings = %v", upd)
		}
		h.Reply(upd, map[string]any{})
		upd = h.RecvWhere(fakeproc.Method("thread/settings/update"))
		if fakeproc.Str(upd, "params", "collaborationMode", "mode") != "default" || fakeproc.Str(upd, "params", "sandboxPolicy", "writableRoots", 0) != "/extra" {
			t.Errorf("leaving plan settings = %v", upd)
		}
		h.Reply(upd, map[string]any{})
		upd = h.RecvWhere(fakeproc.Method("thread/settings/update"))
		if fakeproc.Get(upd, "params", "collaborationMode") != nil || fakeproc.Get(upd, "params", "sandboxPolicy", "networkAccess") != true {
			t.Errorf("acceptEdits settings = %v", upd)
		}
		h.Reply(upd, map[string]any{})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{PermissionMode: agent.PermissionBypass, AppendSystemPrompt: "be terse"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, mode := range []agent.PermissionMode{agent.PermissionReadOnly, agent.PermissionPlan, agent.PermissionDefault, agent.PermissionAcceptEdits} {
		if err := sess.SetPermissionMode(ctx, mode); err != nil {
			t.Fatal(err)
		}
	}
	if sess.Info().PermissionMode != agent.PermissionAcceptEdits {
		t.Errorf("mode = %s", sess.Info().PermissionMode)
	}
}

func TestTurnOptionsOfAnotherTypeRefused(t *testing.T) {
	release := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		<-release
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer close(release)
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, ProviderOptions: "options"})
	if err == nil || !strings.Contains(err.Error(), "Input.ProviderOptions holds string; want *codex.TurnOptions") {
		t.Errorf("Send = %v", err)
	}
	_, err = sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: "Steer"})
	if err == nil || !strings.Contains(err.Error(), `unknown Input.Delivery "Steer"`) {
		t.Errorf("Send with bad delivery = %v", err)
	}
}

func TestHostToolsNeedExperimentalAPI(t *testing.T) {
	p := codex.New(codex.Options{DisableExperimentalAPI: true})
	if p.Capabilities().HostTools {
		t.Error("HostTools without the experimental API")
	}
	tool := agent.Tool{Name: "add", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult(""), nil
	}}
	_, err := p.Start(context.Background(), agent.Config{Tools: []agent.Tool{tool}, Strict: true})
	if !errors.Is(err, agent.ErrUnsupported) || !strings.Contains(err.Error(), "experimental API") {
		t.Errorf("Strict start with tools = %v", err)
	}
}

func TestInterruptTurnDropsQueuedAndStopsRunning(t *testing.T) {
	interrupted := make(chan map[string]any, 1)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		startTurn(h, "t1")
		stop := h.RecvWhere(fakeproc.Method("turn/interrupt"))
		interrupted <- stop
		h.Reply(stop, map[string]any{})
		turnCompleted(h, "t1", "interrupted", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var _ agent.TurnInterrupter = sess
	first, err := sess.Send(ctx, agent.Text("work"))
	if err != nil {
		t.Fatal(err)
	}
	collect(t, sess, func(ev agent.Event) bool { s, ok := ev.(*agent.TurnStarted); return ok && s.TurnID == "t1" })
	queued, err := sess.Send(ctx, agent.Text("later"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.InterruptTurn(ctx, queued.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := queued.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Errorf("queued turn = %v, want ErrCancelled", err)
	}
	select {
	case <-first.Done():
		t.Fatal("dropping the queued input ended the running turn")
	default:
	}
	if err := sess.InterruptTurn(ctx, first.ID()); err != nil {
		t.Fatal(err)
	}
	if stop := <-interrupted; fakeproc.Str(stop, "params", "turnId") != "t1" {
		t.Errorf("turn/interrupt = %v", stop)
	}
	if _, err := first.Wait(ctx); !errors.Is(err, agent.ErrInterrupted) {
		t.Errorf("running turn = %v, want ErrInterrupted", err)
	}
	if err := sess.InterruptTurn(ctx, first.ID()); err != nil {
		t.Errorf("InterruptTurn on an ended turn = %v", err)
	}
}

func TestCompactWaitsForCompaction(t *testing.T) {
	finish := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		compact := h.RecvWhere(fakeproc.Method("thread/compact/start"))
		if fakeproc.Get(compact, "params", "instructions") != nil {
			t.Errorf("instructions sent: %v", compact)
		}
		h.Reply(compact, map[string]any{})
		item(h, "item/started", "tc", map[string]any{"type": "contextCompaction", "id": "k1"})
		<-finish
		item(h, "item/completed", "tc", map[string]any{"type": "contextCompaction", "id": "k1"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	done := make(chan error, 1)
	go func() { done <- sess.Compact(ctx, "ignored") }()
	select {
	case err := <-done:
		t.Fatalf("Compact returned before the compaction finished: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(finish)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Compact did not return after the compaction finished")
	}
}

func TestConfigOutputSchemaAppliesToTurnsWithoutTheirOwn(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if startThread(t, h) == nil {
			return
		}
		ts := startTurn(h, "t1")
		if schema := fakeproc.Get(ts, "params", "outputSchema"); fakeproc.Get(schema, "additionalProperties") != false || fakeproc.Str(schema, "required", 0) != "a" {
			t.Errorf("session schema = %v", schema)
		}
		item(h, "item/completed", "t1", map[string]any{"type": "agentMessage", "id": "m1", "text": `{"a":1}`, "phase": "final_answer"})
		turnCompleted(h, "t1", "completed", nil)

		ts = startTurn(h, "t2")
		if fakeproc.Get(ts, "params", "outputSchema", "properties", "z") == nil {
			t.Errorf("input schema = %v", fakeproc.Get(ts, "params", "outputSchema"))
		}
		steer := h.RecvWhere(fakeproc.Method("turn/steer"))
		if _, ok := steer["params"].(map[string]any)["outputSchema"]; ok {
			t.Errorf("steer = %v", steer)
		}
		h.Reply(steer, map[string]any{"turnId": "t2"})
		turnCompleted(h, "t2", "completed", nil)

		ts = startTurn(h, "t3")
		if fakeproc.Str(ts, "params", "outputSchema", "required", 0) != "a" {
			t.Errorf("queued turn schema = %v", fakeproc.Get(ts, "params", "outputSchema"))
		}
		turnCompleted(h, "t3", "completed", nil)
		h.WaitClosed()
	})
	ctx := context.Background()
	cfg := agent.Config{OutputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`)}
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.Run(ctx, agent.Text("one"))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.StructuredOutput) != `{"a":1}` {
		t.Errorf("structured output = %s", res.StructuredOutput)
	}
	own := agent.Text("two")
	own.OutputSchema = json.RawMessage(`{"type":"object","properties":{"z":{"type":"string"}}}`)
	second, err := sess.Send(ctx, own)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, sess, func(ev agent.Event) bool { s, ok := ev.(*agent.TurnStarted); return ok && s.TurnID == "t2" })
	if _, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("more")}, Delivery: agent.DeliverSteer}); err != nil {
		t.Fatal(err)
	}
	third, err := sess.Send(ctx, agent.Text("three"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := third.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidConfigOutputSchemaFailsStart(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		init := h.RecvWhere(fakeproc.Method("initialize"))
		h.Reply(init, map[string]any{"userAgent": "fake", "codexHome": "/h", "platformFamily": "unix", "platformOs": "linux"})
		if m := h.RecvWhere(fakeproc.Method("thread/start")); m != nil {
			t.Errorf("thread/start sent for an invalid schema: %v", m)
		}
	})
	cfg := agent.Config{OutputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`)}
	_, err := codex.New(codex.Options{Spawner: spawner}).StartSession(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "must set additionalProperties to false") {
		t.Errorf("StartSession = %v", err)
	}
}

func TestNativeApprovalPolicyPassesThrough(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		start := startThread(t, h)
		if fakeproc.Str(start, "params", "approvalPolicy") != "untrusted" {
			t.Errorf("native policy = %v", start["params"])
		}
		h.WaitClosed()
	})
	sess, err := codex.New(codex.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{PermissionMode: "untrusted"})
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()
}
