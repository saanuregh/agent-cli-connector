package opencode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// TestCapabilitiesMatchInterfaces checks that the RateLimits flag matches
// the RateLimitReader interface *opencode.Session implements.
func TestCapabilitiesMatchInterfaces(t *testing.T) {
	caps := opencode.New(opencode.Options{}).Capabilities()
	var sess any = (*opencode.Session)(nil)
	check := func(name string, flag, implemented bool) {
		if flag != implemented {
			t.Errorf("Capabilities.%s = %v, implements = %v", name, flag, implemented)
		}
	}
	_, limits := sess.(agent.RateLimitReader)
	check("RateLimits", caps.RateLimits, limits)
	if opencode.New(opencode.Options{ServerURL: "http://127.0.0.1:1"}).Capabilities().HostTools {
		t.Error("an attached server without AllowHostToolsOnAttachedServer reports host tools")
	}
}

func TestTurnLifecycle(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	if info := sess.Info(); info.Model != "opencode/test-model" || info.Version != "2.0.32" || len(info.Commands) != 2 {
		t.Fatalf("info = %+v", info)
	}
	turn := send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("hello")}, ClientID: "c1"})
	req := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	id, _ := req.Body["id"].(string)
	if !strings.HasPrefix(id, "msg_") || req.Body["text"] != "hello" || fakeproc.Str(req.Body, "metadata", "clientID") != "c1" {
		t.Fatalf("prompt body = %v", req.Body)
	}
	if turn.ID() != id {
		t.Fatalf("turn ID %q, prompt ID %q", turn.ID(), id)
	}
	f.emit("session.inbox.enqueued", map[string]any{"inboxID": id, "item": map[string]any{"type": "user", "payload": map[string]any{"text": "hello", "metadata": map[string]any{"clientID": "c1"}}}})
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.inbox.delivered", map[string]any{"inboxID": id})
	f.emit("session.step.started", map[string]any{"assistantMessageID": "msg_a", "model": map[string]any{"providerID": "opencode", "id": "test-model", "variant": "default"}})
	f.emit("session.reasoning.started", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0})
	f.emit("session.reasoning.delta", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0, "delta": "think"})
	f.emit("session.reasoning.ended", map[string]any{"assistantMessageID": "msg_a", "ordinal": 0, "text": "think"})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "msg_a", "id": "call_1", "name": "shell"})
	f.emit("session.tool.called", map[string]any{"assistantMessageID": "msg_a", "id": "call_1", "input": map[string]any{"command": "ls"}, "executed": false})
	f.emit("session.tool.success", map[string]any{"assistantMessageID": "msg_a", "id": "call_1", "content": []any{map[string]any{"type": "text", "text": "a.txt\n"}}, "metadata": map[string]any{"exit": 0}})
	f.emit("session.step.ended", map[string]any{"assistantMessageID": "msg_a", "finish": "tool-calls", "cost": 0.25, "tokens": map[string]any{"input": 10, "output": 5, "reasoning": 2, "cache": map[string]any{"read": 3, "write": 1}}})
	f.emit("session.text.started", map[string]any{"assistantMessageID": "msg_b", "ordinal": 0})
	f.emit("session.text.delta", map[string]any{"assistantMessageID": "msg_b", "ordinal": 0, "delta": "hi "})
	f.emit("session.text.delta", map[string]any{"assistantMessageID": "msg_b", "ordinal": 0, "delta": "there"})
	f.emit("session.text.ended", map[string]any{"assistantMessageID": "msg_b", "ordinal": 0, "text": "hi there"})
	f.emit("session.step.ended", map[string]any{"assistantMessageID": "msg_b", "finish": "stop", "cost": 0.5, "tokens": map[string]any{"input": 20, "output": 3, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}})
	f.emit("session.execution.succeeded", map[string]any{})
	res := waitTurn(t, turn)
	if res.Status != agent.TurnSucceeded || res.Text != "hi there" || res.StopReason != "stop" || res.Steps != 2 {
		t.Fatalf("result = %+v", res)
	}
	u := res.Usage
	if u.InputTokens != 30 || u.OutputTokens != 10 || u.ReasoningTokens != 2 || u.CacheReadTokens != 3 || u.CacheWriteTokens != 1 || u.CostUSD != 0.75 || u.ContextWindow != 1000 {
		t.Fatalf("usage = %+v", u)
	}
	var kinds []agent.ItemKind
	for _, it := range res.Items {
		kinds = append(kinds, it.Kind)
		if it.Kind == agent.ItemUserMessage && (it.ClientID != "c1" || it.ID != id) {
			t.Errorf("user item = %+v", it)
		}
		if it.Kind == agent.ItemToolCall && (it.Tool.Kind != agent.ToolShell || it.Tool.Command != "ls" || it.Tool.Output != "a.txt\n" || it.Tool.ExitCode == nil) {
			t.Errorf("tool item = %+v %+v", it, it.Tool)
		}
	}
	want := []agent.ItemKind{agent.ItemUserMessage, agent.ItemReasoning, agent.ItemToolCall, agent.ItemAssistantMessage}
	if !slices.Equal(kinds, want) {
		t.Fatalf("items = %v, want %v", kinds, want)
	}
	waitFor[*agent.TurnCompleted](t, ev)
	if countOf[*agent.TextDelta](ev) != 2 || countOf[*agent.ReasoningDelta](ev) != 1 || countOf[*agent.UsageUpdated](ev) != 2 || countOf[*agent.TurnStarted](ev) != 1 {
		t.Errorf("deltas %d, reasoning %d, usage %d, turn starts %d", countOf[*agent.TextDelta](ev), countOf[*agent.ReasoningDelta](ev), countOf[*agent.UsageUpdated](ev), countOf[*agent.TurnStarted](ev))
	}
	for _, e := range ev.all() {
		switch e.(type) {
		case *agent.TextDelta, *agent.ItemCompleted, *agent.UsageUpdated:
			if len(e.Meta().Raw) == 0 {
				t.Errorf("%T has no Raw", e)
			}
		}
	}
	f.emit("session.renamed", map[string]any{"title": "New title"})
	ev.wait(t, func(e agent.Event) bool {
		u, ok := e.(*agent.SessionUpdated)
		return ok && u.Info.Title == "New title"
	})
	f.emit("vcs.branch.updated", map[string]any{"sessionID": "", "branch": "main"})
	f.emit("session.status", map[string]any{"status": map[string]any{"type": "idle"}})
	raw := waitFor[*agent.RawEvent](t, ev)
	if raw.Type == "" || len(raw.Raw) == 0 {
		t.Errorf("raw event = %+v", raw)
	}
}

func TestPermissionRoundTrip(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	created := f.next("POST", "/api/session")
	// The build agent's deny follows the mode's rules; the last rule keeps
	// other sessions' host tools out on an attached server.
	if rules := created.Body["permissions"].([]any); len(rules) != 5 || fakeproc.Str(rules[0], "action") != "shell" || fakeproc.Str(rules[0], "effect") != "ask" ||
		fakeproc.Str(rules[3], "action") != "browser" || fakeproc.Str(rules[4], "action") != "agenthost-*" {
		t.Fatalf("create rules = %v", created.Body["permissions"])
	}
	turn := send(t, sess, agent.Text("run it"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call_1", "name": "shell"})
	ask := func(id string) *agent.PermissionRequest {
		f.emit("permission.asked", map[string]any{"id": id, "action": "shell", "resources": []any{"echo hi"}, "save": []any{"echo *"},
			"source": map[string]any{"type": "tool", "messageID": "m", "id": "call_1"}})
		return ev.wait(t, func(e agent.Event) bool {
			r, ok := e.(*agent.PermissionRequest)
			return ok && r.ID == id
		}).(*agent.PermissionRequest)
	}
	ctx := context.Background()
	req := ask("per_1")
	if req.Kind != agent.PermissionCommand || req.Command != "echo hi" || req.ToolCallID != "call_1" || req.ToolName != "shell" {
		t.Fatalf("request = %+v", req)
	}
	if len(sess.PendingRequests()) != 1 {
		t.Fatalf("pending = %d", len(sess.PendingRequests()))
	}
	if err := req.AllowForSession(ctx); err != nil {
		t.Fatal(err)
	}
	// The grant goes right after the ask it answers.
	rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	if i := slices.Index(rules, "shell:echo *=allow"); i < 1 || rules[i-1] != "shell:*=ask" {
		t.Fatalf("grant rules = %v", rules)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/permission/per_1/reply"); r.Body["decision"] != "once" {
		t.Fatalf("reply = %v", r.Body)
	}
	if err := ask("per_2").Deny(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/permission/per_2/reply"); r.Body["decision"] != "reject" || r.Body["message"] == "" {
		t.Fatalf("deny = %v", r.Body)
	}
	if err := ask("per_3").Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/permission/per_3/reply"); r.Body["decision"] != "reject" || r.Body["message"] != nil {
		t.Fatalf("cancel = %v", r.Body)
	}
	if err := ask("per_4").Respond(ctx, agent.PermissionResponse{Decision: agent.DecisionAllowAlways}); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/permission/per_4/reply"); r.Body["decision"] != "always" {
		t.Fatalf("always = %v", r.Body)
	}
	// A request answered elsewhere is withdrawn.
	elsewhere := ask("per_5")
	f.emit("permission.replied", map[string]any{"requestID": "per_5", "reply": "once"})
	select {
	case <-elsewhere.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("request not withdrawn")
	}
	if elsewhere.Outcome() != agent.RequestWithdrawn {
		t.Fatalf("outcome = %q", elsewhere.Outcome())
	}
	// The turn's end withdraws what is still pending.
	open := ask("per_6")
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	select {
	case <-open.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("request not withdrawn at the turn's end")
	}
}

func TestQuestionRoundTrip(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("ask me"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	form := func(id string) *agent.UserInputRequest {
		f.emit("form.created", map[string]any{"sessionID": "", "form": map[string]any{
			"id": id, "sessionID": fakeSession, "title": "Questions",
			"metadata": map[string]any{"kind": "question", "tool": map[string]any{"messageID": "m", "id": "call_q"}},
			"fields": []any{
				map[string]any{"key": "q0", "title": "Color", "description": "Pick one", "type": "string", "custom": true,
					"options": []any{map[string]any{"value": "red", "label": "Red"}, map[string]any{"value": "blue", "label": "Blue"}}},
				map[string]any{"key": "q1", "title": "Sizes", "type": "multiselect",
					"options": []any{map[string]any{"value": "s", "label": "Small"}, map[string]any{"value": "l", "label": "Large"}}},
				map[string]any{"key": "q2", "title": "Sure", "type": "boolean"},
			},
		}})
		return ev.wait(t, func(e agent.Event) bool {
			r, ok := e.(*agent.UserInputRequest)
			return ok && r.ID == id
		}).(*agent.UserInputRequest)
	}
	req := form("frm_1")
	if req.ToolCallID != "call_q" || len(req.Questions) != 3 || req.Questions[0].Options[1].Label != "Blue" || !req.Questions[0].AllowOther || !req.Questions[1].MultiSelect {
		t.Fatalf("request = %+v", req)
	}
	ctx := context.Background()
	if err := req.Answer(ctx, map[string][]string{"q0": {"Blue"}, "q1": {"Small", "Large"}, "q2": {"Yes"}}); err != nil {
		t.Fatal(err)
	}
	r := f.next("POST", "/api/session/"+fakeSession+"/form/frm_1/reply")
	answer := r.Body["answer"].(map[string]any)
	if answer["q0"] != "blue" || len(answer["q1"].([]any)) != 2 || answer["q2"] != true {
		t.Fatalf("answer = %v", answer)
	}
	if err := form("frm_2").Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	f.next("DELETE", "/api/session/"+fakeSession+"/form/frm_2")
}

func TestInterruptAndQueue(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	queued := send(t, sess, agent.Text("second"))
	time.Sleep(50 * time.Millisecond)
	if n := f.count("POST", "/api/session/"+fakeSession+"/prompt"); n != 1 {
		t.Fatalf("queued input sent early: %d prompts", n)
	}
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.step.failed", map[string]any{"assistantMessageID": "m", "error": map[string]any{"type": "aborted", "message": "Step interrupted"}})
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, first); res.Status != agent.TurnInterrupted || res.StopReason != "user" {
		t.Fatalf("first = %+v", res)
	}
	second := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	if second.Body["text"] != "second" || second.Body["id"] != queued.ID() {
		t.Fatalf("queued prompt = %v (turn %s)", second.Body, queued.ID())
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	if res := waitTurn(t, queued); res.Status != agent.TurnSucceeded {
		t.Fatalf("queued = %+v", res)
	}
	if countOf[*agent.Notice](ev) != 0 {
		t.Error("an aborted step produced a notice")
	}
}

func TestInterruptDuringStart(t *testing.T) {
	f := newFakeServer(t)
	release := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		<-release
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	sess, _ := startFake(t, f, agent.Config{})
	var turn *agent.Turn
	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		turn, sendErr = sess.Send(context.Background(), agent.Text("slow start"))
	}()
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	if err := sess.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST", "/api/session/"+fakeSession+"/interrupt"); n != 0 {
		t.Fatal("interrupt sent before OpenCode accepted the prompt")
	}
	close(release)
	<-done
	if sendErr != nil {
		t.Fatal(sendErr)
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("result = %+v", res)
	}
}

func TestSteerJoinsTurn(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	steered := send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("stop")}, Delivery: agent.DeliverSteer})
	req := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	if req.Body["delivery"] != "steer" {
		t.Fatalf("steer body = %v", req.Body)
	}
	if steered != turn {
		t.Fatal("steer did not join the running turn")
	}
	steerID := req.Body["id"].(string)
	f.emit("session.inbox.enqueued", map[string]any{"inboxID": steerID, "item": map[string]any{"type": "user", "payload": map[string]any{"text": "stop"}}})
	// The execution ends before the steer was delivered: OpenCode runs
	// another execution for it, which belongs to the same turn.
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-turn.Done():
		t.Fatal("turn ended while a steer was undelivered")
	default:
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.inbox.delivered", map[string]any{"inboxID": steerID})
	f.emit("session.text.started", map[string]any{"assistantMessageID": "msg_s", "ordinal": 0})
	f.emit("session.text.ended", map[string]any{"assistantMessageID": "msg_s", "ordinal": 0, "text": "stopped"})
	f.emit("session.execution.succeeded", map[string]any{})
	res := waitTurn(t, turn)
	if res.Text != "stopped" || res.Status != agent.TurnSucceeded {
		t.Fatalf("result = %+v", res)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countOf[*agent.TurnCompleted](ev); n != 1 {
		t.Fatalf("turn completed %d times", n)
	}
}

func TestSteerWhileTurnEnds(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		<-gate
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	var steered *agent.Turn
	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		steered, sendErr = sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("late")}, Delivery: agent.DeliverSteer})
	}()
	req := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	// The execution ends while OpenCode has not answered the steer yet:
	// the turn waits for the execution OpenCode runs for the steer.
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-first.Done():
		t.Fatal("turn ended while a steer was in flight")
	default:
	}
	close(gate)
	<-done
	if sendErr != nil || steered != first {
		t.Fatalf("steer = %v (joined %v)", sendErr, steered == first)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.inbox.delivered", map[string]any{"inboxID": req.Body["id"]})
	f.emit("session.execution.succeeded", map[string]any{})
	if res := waitTurn(t, first); res.Status != agent.TurnSucceeded {
		t.Fatalf("result = %+v", res)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countOf[*agent.TurnStarted](ev); n != 1 {
		t.Fatalf("turn started %d times", n)
	}
}

func TestSteerRejected(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(*fakeRequest) (int, any) {
		return 409, map[string]any{"_tag": "ConflictError", "message": "busy"}
	})
	_, err := sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: agent.DeliverSteer})
	if !errors.Is(err, agent.ErrSteerRejected) {
		t.Fatalf("err = %v", err)
	}
	f.emit("session.execution.succeeded", map[string]any{})
	if res := waitTurn(t, first); res.Status != agent.TurnSucceeded {
		t.Fatalf("result = %+v", res)
	}
}

func TestDeliverInterrupt(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	first := send(t, sess, agent.Text("first"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	next := send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("instead")}, Delivery: agent.DeliverInterrupt})
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, first); res.Status != agent.TurnInterrupted {
		t.Fatalf("first = %+v", res)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "instead" {
		t.Fatalf("next prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, next)
}

func TestFailedTurn(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("hi"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.retry.scheduled", map[string]any{"assistantMessageID": "m", "attempt": 1, "at": time.Now().Add(time.Second).UnixMilli(),
		"error": map[string]any{"type": "rate_limit", "message": "slow down", "status": 429}})
	retry := waitFor[*agent.Retry](t, ev)
	if retry.Attempt != 1 || retry.HTTPStatus != 429 || retry.Code != "rate_limit" || retry.Delay <= 0 {
		t.Fatalf("retry = %+v", retry)
	}
	f.emit("session.execution.failed", map[string]any{"error": map[string]any{"type": "provider.rate_limit", "message": "limit reached", "status": 429}})
	res := waitTurn(t, turn)
	var te *agent.TurnError
	if res.Status != agent.TurnFailed || !errors.As(res.Err, &te) || te.Code != "provider.rate_limit" || te.HTTPStatus != 429 || !te.Retryable {
		t.Fatalf("result = %+v err %v", res, res.Err)
	}
}

func TestPromptRejected(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/"+fakeSession+"/prompt", func(*fakeRequest) (int, any) {
		return 400, map[string]any{"_tag": "InvalidRequestError", "message": "bad"}
	})
	sess, _ := startFake(t, f, agent.Config{})
	_, err := sess.Send(context.Background(), agent.Text("hi"))
	if pe, ok := errors.AsType[*agent.ProtocolError](err); !ok || pe.ErrorCode != "InvalidRequestError" || pe.Code != 400 {
		t.Fatalf("err = %v", err)
	}
	// The session stays usable.
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) { return 200, data(map[string]any{"id": r.Body["id"]}) })
	turn := send(t, sess, agent.Text("again"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
}

func TestResumeNotFound(t *testing.T) {
	f := newFakeServer(t)
	_, err := f.provider().Start(context.Background(), agent.Config{Cwd: workDir, Resume: "ses_missing"})
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestResumeAppliesSettings(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{Resume: fakeSession, Model: "opencode/test-model#high", PermissionMode: agent.PermissionPlan, Title: "Renamed", AppendSystemPrompt: "be brief", Cwd: otherDir})
	f.next("GET", "/api/session/"+fakeSession)
	if r := f.next("POST", "/api/session/"+fakeSession+"/move"); r.Body["directory"] != otherDir {
		t.Fatalf("move = %v", r.Body)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/model"); fakeproc.Str(r.Body, "model", "variant") != "high" {
		t.Fatalf("model = %v", r.Body)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/agent"); r.Body["agent"] != "plan" {
		t.Fatalf("agent = %v", r.Body)
	}
	f.next("PATCH", "/api/session/"+fakeSession)
	if r := f.next("PUT", "/api/experimental/session/"+fakeSession+"/instructions/entries/agent-cli-connector"); r.Body["value"] != "be brief" {
		t.Fatalf("instructions = %v", r.Body)
	}
	info := sess.Info()
	if info.Effort != "high" || info.PermissionMode != agent.PermissionPlan || info.Title != "Renamed" || info.Cwd != otherDir {
		t.Fatalf("info = %+v", info)
	}
}

func TestModelsAndModes(t *testing.T) {
	f := newFakeServer(t)
	if _, err := f.provider().Start(context.Background(), agent.Config{Cwd: workDir, Model: "opencode/nope"}); err == nil {
		t.Fatal("unknown model accepted")
	}
	if _, err := f.provider().Start(context.Background(), agent.Config{Cwd: workDir, PermissionMode: agent.PermissionDontAsk}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("dontAsk: %v", err)
	}
	sess, _ := startFake(t, f, agent.Config{})
	ctx := context.Background()
	if err := sess.SetEffort(ctx, "max"); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("effort max: %v", err)
	}
	if err := sess.SetEffort(ctx, agent.EffortLow); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/model"); fakeproc.Str(r.Body, "model", "variant") != "low" {
		t.Fatalf("effort body = %v", r.Body)
	}
	if err := sess.SetModel(ctx, "opencode/text-only"); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/model"); fakeproc.Str(r.Body, "model", "id") != "text-only" || fakeproc.Str(r.Body, "model", "variant") != "" {
		t.Fatalf("model body = %v", r.Body)
	}
	if _, err := sess.Send(ctx, agent.Prompt(agent.TextPart("see"), agent.ImageData("image/png", []byte{1}))); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("image to a text-only model: %v", err)
	}
	if err := sess.SetPermissionMode(ctx, agent.PermissionPlan); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/agent"); r.Body["agent"] != "plan" {
		t.Fatalf("agent = %v", r.Body)
	}
	f.next("PATCH", "/api/session/"+fakeSession)
	if err := sess.SetPermissionMode(ctx, agent.PermissionBypass); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/agent"); r.Body["agent"] != "build" {
		t.Fatalf("agent = %v", r.Body)
	}
	if r := f.next("PATCH", "/api/session/"+fakeSession); fakeproc.Str(r.Body, "permissions", 0, "action") != "*" {
		t.Fatalf("bypass rules = %v", r.Body)
	}
}

func TestConfigOutputSchemaIgnored(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	f := newFakeServer(t)
	_, err := f.provider().Start(context.Background(), agent.Config{Cwd: workDir, OutputSchema: schema, Strict: true})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("strict Config.OutputSchema: %v", err)
	}
	_, ev := startFake(t, newFakeServer(t), agent.Config{OutputSchema: schema})
	ev.wait(t, func(e agent.Event) bool {
		n, ok := e.(*agent.Notice)
		return ok && n.Code == "config_ignored" && strings.Contains(n.Message, "OutputSchema")
	})
}

func TestInputParts(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	ctx := context.Background()
	if _, err := sess.Send(ctx, agent.Prompt(agent.ImageURL("https://example.com/a.png"))); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("image URL: %v", err)
	}
	if _, err := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("x")}, OutputSchema: json.RawMessage(`{}`)}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("output schema: %v", err)
	}
	send(t, sess, agent.Input{Parts: []agent.Part{
		agent.TextPart("look"), agent.ImageData("image/png", []byte("png")), agent.FileRef("notes.txt"), agent.SkillPart("docs", ""),
	}, ProviderOptions: &opencode.TurnOptions{Agents: []string{"explore"}}})
	r := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	// RFC 8089: a Windows drive path gets a slash, file:///D:/work/notes.txt.
	notesURI := "file://" + filepath.ToSlash(filepath.Join(workDir, "notes.txt"))
	if runtime.GOOS == "windows" {
		notesURI = "file:///" + filepath.ToSlash(filepath.Join(workDir, "notes.txt"))
	}
	if fakeproc.Str(r.Body, "files", 0, "uri") != "data:image/png;base64,cG5n" || fakeproc.Str(r.Body, "files", 1, "uri") != notesURI ||
		fakeproc.Str(r.Body, "skills", 0, "id") != "docs" || fakeproc.Str(r.Body, "agents", 0, "name") != "explore" {
		t.Fatalf("prompt = %s", r.Raw)
	}
}

func TestSlashCommandsAndCompact(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("/review branch"))
	if r := f.next("POST", "/api/session/"+fakeSession+"/command"); r.Body["name"] != "review" || r.Body["text"] != "branch" {
		t.Fatalf("command = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.inbox.enqueued", map[string]any{"inboxID": "msg_native", "item": map[string]any{"type": "user", "payload": map[string]any{"text": "review"}}})
	f.emit("session.inbox.delivered", map[string]any{"inboxID": "msg_native"})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
	// The command's turn rewinds through the message OpenCode made for it.
	if err := sess.Rewind(context.Background(), turn.ID()); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/revert/stage"); r.Body["messageID"] != "msg_native" {
		t.Fatalf("stage = %v", r.Body)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Compact(context.Background(), "ignored") }()
	r := f.next("POST", "/api/session/"+fakeSession+"/compact")
	if id, _ := r.Body["id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("compact = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.compaction.started", map[string]any{"reason": "manual", "recent": "", "inputID": r.Body["id"]})
	f.emit("session.compaction.ended", map[string]any{"reason": "manual", "text": "summary", "recent": ""})
	f.emit("session.execution.succeeded", map[string]any{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c := ev.wait(t, func(e agent.Event) bool { c, ok := e.(*agent.Compaction); return ok && c.Done }).(*agent.Compaction)
	if c.Summary != "summary" || c.Trigger != "manual" {
		t.Fatalf("compaction = %+v", c)
	}
}

func messages() []any {
	return []any{
		map[string]any{"id": "msg_u1", "type": "user", "text": "first", "time": map[string]any{"created": 1000}, "metadata": map[string]any{"clientID": "c1"}},
		map[string]any{"id": "msg_a1", "type": "assistant", "time": map[string]any{"created": 1100}, "content": []any{
			map[string]any{"type": "reasoning", "text": "hmm"},
			map[string]any{"type": "tool", "id": "call_1", "name": "read", "state": map[string]any{"status": "completed", "input": map[string]any{"filePath": "/a"}, "content": []any{map[string]any{"type": "text", "text": "data"}}}},
			map[string]any{"type": "text", "text": "answer one"},
		}},
		map[string]any{"id": "msg_i1", "type": "idle", "outcome": "succeeded", "time": map[string]any{"created": 1200}},
		map[string]any{"id": "msg_u2", "type": "user", "text": "second", "time": map[string]any{"created": 2000}},
		map[string]any{"id": "msg_a2", "type": "assistant", "time": map[string]any{"created": 2100}, "content": []any{
			map[string]any{"type": "tool", "id": "call_2", "name": "shell", "state": map[string]any{"status": "error", "input": map[string]any{"command": "rm"}, "error": map[string]any{"type": "permission.rejected", "message": "no"}}},
			map[string]any{"type": "text", "text": "answer two"},
		}},
	}
}

// pageMessages serves messages with cursors, as OpenCode does.
func pageMessages(r *fakeRequest, all []any) (int, any) {
	order := r.Query.Get("order")
	cursor := r.Query.Get("cursor")
	if cursor != "" && order != "" {
		return 400, map[string]any{"_tag": "InvalidRequestError", "message": "cursor with order"}
	}
	offset := 0
	if cursor != "" {
		var at string
		order, at, _ = strings.Cut(cursor, ":")
		offset, _ = strconv.Atoi(at)
	}
	if order == "" {
		order = "desc"
	}
	list := slices.Clone(all)
	if order == "desc" {
		slices.Reverse(list)
	}
	limit := 100
	if l := r.Query.Get("limit"); l != "" {
		limit, _ = strconv.Atoi(l)
	}
	end := min(offset+limit, len(list))
	page := list[offset:end]
	var next any
	if end < len(list) {
		next = order + ":" + strconv.Itoa(end)
	}
	return 200, map[string]any{"data": page, "cursor": map[string]any{"next": next}}
}

func TestHistoryPagingAndSessions(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/message", func(r *fakeRequest) (int, any) { return pageMessages(r, messages()) })
	p := f.provider()
	ctx := context.Background()
	items, err := p.ReadHistory(ctx, fakeSession, agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, string(it.Kind)+":"+it.ID+"@"+it.TurnID)
	}
	want := []string{
		"user_message:msg_u1@msg_u1", "reasoning:msg_a1:reasoning:0@msg_u1", "tool_call:call_1@msg_u1", "assistant_message:msg_a1:text:0@msg_u1",
		"user_message:msg_u2@msg_u2", "tool_call:call_2@msg_u2", "assistant_message:msg_a2:text:0@msg_u2",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %v", got)
	}
	if items[0].ClientID != "c1" || items[2].Tool.Kind != agent.ToolRead || items[2].Tool.Path != "/a" || items[5].Status != agent.ItemDeclined || items[0].Time.IsZero() {
		t.Fatalf("details = %+v %+v %+v", items[0], items[2].Tool, items[5])
	}
	// Pages of two messages: the newest item needs only the last pages.
	f.on("GET /api/session/"+fakeSession+"/message", func(r *fakeRequest) (int, any) {
		r.Query.Set("limit", "2")
		return pageMessages(r, messages())
	})
	page, err := p.ReadHistory(ctx, fakeSession, agent.ReadHistoryOptions{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != "msg_u2" || page[1].ID != "call_2" {
		t.Fatalf("page = %+v", page)
	}
	f.on("GET /api/session", func(r *fakeRequest) (int, any) {
		if r.Query.Get("directory") != workDir || r.Query.Get("order") != "desc" {
			t.Errorf("list query = %v", r.Query)
		}
		child := sessionInfo("ses_child")
		child["parentID"] = fakeSession
		return 200, map[string]any{"data": []any{sessionInfo("ses_new"), child, sessionInfo("ses_mid"), sessionInfo("ses_old")}, "cursor": map[string]any{}}
	})
	list, err := p.ListSessions(ctx, agent.ListSessionsOptions{Cwd: workDir, Offset: 1, Limit: 1})
	if err != nil || len(list) != 1 || list[0].ID != "ses_mid" || list[0].Model != "opencode/test-model" || !list[0].CreatedAt.Equal(time.UnixMilli(1000)) {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	if err := p.DeleteSession(ctx, fakeSession, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSession(ctx, "ses_gone", ""); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if _, err := p.ReadHistory(ctx, "ses_gone", agent.ReadHistoryOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("history missing = %v", err)
	}
}

func TestForkAndRewind(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/message", func(r *fakeRequest) (int, any) { return pageMessages(r, messages()) })
	sess, _ := startFake(t, f, agent.Config{})
	ctx := context.Background()
	forked, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: "msg_u1"})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()
	if r := f.next("POST", "/api/session/"+fakeSession+"/fork"); r.Body["before"] != "msg_u2" {
		t.Fatalf("fork = %v", r.Body)
	}
	if forked.ID() != "ses_fork" {
		t.Fatalf("fork ID = %s", forked.ID())
	}
	if _, err := sess.ForkSession(ctx, agent.Config{ForkAtTurn: "msg_unknown"}); err == nil {
		t.Fatal("fork at an unknown turn succeeded")
	}
	if err := sess.Rewind(ctx, "msg_u2"); err != nil {
		t.Fatal(err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/revert/stage"); r.Body["messageID"] != "msg_u2" || r.Body["files"] != false {
		t.Fatalf("stage = %v", r.Body)
	}
	f.next("POST", "/api/session/"+fakeSession+"/revert/commit")
	send(t, sess, agent.Text("busy"))
	if err := sess.Rewind(ctx, "msg_u1"); err == nil {
		t.Fatal("rewind while busy succeeded")
	}
}

func TestSubagentTask(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/ses_kid/interrupt", func(*fakeRequest) (int, any) { return 200, map[string]any{"interrupted": true} })
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("delegate"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call_sub", "name": "subagent"})
	f.emit("session.tool.called", map[string]any{"assistantMessageID": "m", "id": "call_sub", "input": map[string]any{"agent": "general", "description": "Find kiwi", "prompt": "say kiwi"}})
	f.emit("session.created", map[string]any{"sessionID": "ses_kid", "parentID": fakeSession, "title": "Find kiwi", "agent": "general"})
	f.emit("session.tool.progress", map[string]any{"assistantMessageID": "m", "id": "call_sub", "metadata": map[string]any{"sessionID": "ses_kid", "status": "running"}})
	f.emit("session.execution.started", map[string]any{"sessionID": "ses_kid"})
	f.emit("session.text.started", map[string]any{"sessionID": "ses_kid", "assistantMessageID": "msg_k", "ordinal": 0})
	f.emit("session.text.delta", map[string]any{"sessionID": "ses_kid", "assistantMessageID": "msg_k", "ordinal": 0, "delta": "kiwi"})
	f.emit("session.tool.input.started", map[string]any{"sessionID": "ses_kid", "assistantMessageID": "msg_k", "id": "call_1", "name": "read"})
	f.emit("session.text.ended", map[string]any{"sessionID": "ses_kid", "assistantMessageID": "msg_k", "ordinal": 0, "text": "kiwi"})
	f.emit("form.created", map[string]any{"sessionID": "", "form": map[string]any{
		"id": "frm_kid", "sessionID": "ses_kid", "title": "Q", "metadata": map[string]any{"kind": "question"},
		"fields": []any{map[string]any{"key": "q0", "title": "Which", "type": "string"}}}})
	if q := ev.wait(t, func(e agent.Event) bool { _, ok := e.(*agent.UserInputRequest); return ok }).(*agent.UserInputRequest); q.SubagentID != "ses_kid" {
		t.Errorf("subagent question SubagentID = %q", q.SubagentID)
	}
	f.emit("session.tool.success", map[string]any{"assistantMessageID": "m", "id": "call_sub",
		"content":  []any{map[string]any{"type": "text", "text": "<subagent sessionID=\"ses_kid\" state=\"completed\">\nkiwi\n</subagent>"}},
		"metadata": map[string]any{"sessionID": "ses_kid", "status": "completed"}})
	done := ev.wait(t, func(e agent.Event) bool {
		u, ok := e.(*agent.TaskUpdated)
		return ok && u.Task.Status == agent.TaskCompleted
	}).(*agent.TaskUpdated).Task
	// A subagent is known until the turn that ran it ends.
	if err := sess.StopTask(context.Background(), "ses_kid"); err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/ses_kid/interrupt")
	f.emit("session.execution.succeeded", map[string]any{})
	res := waitTurn(t, turn)
	if done.ID != "ses_kid" || done.Output != "kiwi" || done.ToolCallID != "call_sub" || done.Subagent.Type != "general" || done.Subagent.Prompt != "say kiwi" || done.ToolUses != 1 {
		t.Fatalf("task = %+v %+v", done, done.Subagent)
	}
	delta := waitFor[*agent.TextDelta](t, ev)
	if delta.SubagentID != "ses_kid" || delta.ParentID != "call_sub" {
		t.Fatalf("subagent delta = %+v", delta)
	}
	for _, it := range res.Items {
		if it.ID == "ses_kid/call_1" && it.SubagentID != "ses_kid" {
			t.Errorf("subagent tool item = %+v", it)
		}
	}
	if res.Text != "" {
		t.Errorf("turn text took the subagent's text: %q", res.Text)
	}
	if err := sess.StopTask(context.Background(), "ses_other"); err == nil {
		t.Fatal("stopped an unknown task")
	}
}

func TestStreamReconnect(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("hi"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		info["outcome"] = "succeeded"
		return 200, data(info)
	})
	f.dropStreams()
	res := waitTurn(t, turn)
	if res.Status != agent.TurnSucceeded {
		t.Fatalf("result = %+v", res)
	}
	if f.streamCount() < 2 {
		t.Fatal("no reconnect")
	}
	select {
	case <-sess.Done():
		t.Fatalf("session ended: %v", sess.Err())
	default:
	}
}

func TestHostToolsAndMCP(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	added := map[string]map[string]any{}
	f.on("PUT /api/experimental/mcp/*", func(r *fakeRequest) (int, any) {
		mu.Lock()
		added[strings.TrimPrefix(r.Path, "/api/experimental/mcp/")] = r.Body["config"].(map[string]any)
		mu.Unlock()
		return 204, nil
	})
	f.on("GET /api/mcp", func(*fakeRequest) (int, any) {
		return 200, map[string]any{"data": []any{map[string]any{"name": "taken", "status": map[string]any{"status": "connected"}}}}
	})
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("PUT /api/experimental/session/*", func(*fakeRequest) (int, any) { return 204, nil })
	tool := agent.Tool{Name: "add", Description: "adds", NeedsApproval: true, Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
		return agent.TextResult("sum " + string(call.Arguments)), nil
	}}
	sess, ev := startFake(t, f, agent.Config{
		Tools: []agent.Tool{tool},
		MCPServers: map[string]agent.MCPServer{
			"docs":  {Command: "docs-mcp", Args: []string{"--x"}, AlwaysLoad: true, ApprovedTools: []string{"search"}, ToolTimeout: 2 * time.Second},
			"taken": agent.HTTPServer("http://example.com/mcp"),
		},
	})
	created := f.next("POST", "/api/session")
	mu.Lock()
	var host string
	var hostCfg map[string]any
	for name, c := range added {
		if strings.HasPrefix(name, "agenthost-") {
			host, hostCfg = name, c
		}
	}
	docs := added["docs"]
	_, takenAdded := added["taken"]
	mu.Unlock()
	if host == "" || docs == nil || takenAdded {
		t.Fatalf("added = %v", added)
	}
	if fakeproc.Str(docs, "type") != "local" || fakeproc.Get(docs, "codemode") != false || fakeproc.Get(docs, "timeout", "execution") != float64(2000) {
		t.Fatalf("docs config = %v", docs)
	}
	var actions []string
	for _, r := range created.Body["permissions"].([]any) {
		actions = append(actions, fakeproc.Str(r, "action")+"="+fakeproc.Str(r, "effect"))
	}
	for _, want := range []string{"docs_search=allow", "agenthost-*=deny", host + "_*=allow", host + "_add=ask"} {
		if !slices.Contains(actions, want) {
			t.Errorf("rules %v lack %s", actions, want)
		}
	}
	waitFor[*agent.Notice](t, ev)
	call := func(token, body string) (*http.Response, []byte) {
		req, _ := http.NewRequest("POST", fakeproc.Str(hostCfg, "url"), bytes.NewBufferString(body))
		req.Header.Set("Authorization", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp, data
	}
	token := fakeproc.Str(hostCfg, "headers", "Authorization")
	if resp, _ := call("Bearer wrong", `{}`); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	_, list := call(token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(string(list), `"add"`) {
		t.Fatalf("tools/list = %s", list)
	}
	_, out := call(token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add","arguments":{"a":1}}}`)
	if !strings.Contains(string(out), `sum {\"a\":1}`) {
		t.Fatalf("tools/call = %s", out)
	}
	if resp, _ := call(token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != 202 {
		t.Fatalf("notification: %d", resp.StatusCode)
	}
	// Tool items of the host server are host tools.
	send(t, sess, agent.Text("add"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.tool.input.started", map[string]any{"assistantMessageID": "m", "id": "call_h", "name": host + "_add"})
	item := waitFor[*agent.ItemStarted](t, ev).Item
	if item.Tool.Kind != agent.ToolHost || item.Tool.DisplayName != "add" {
		t.Fatalf("host item = %+v", item.Tool)
	}
	_ = sess.Close()
	// An attached server forgets what the session added.
	f.next("DELETE", "/api/experimental/mcp/"+host)
	f.next("DELETE", "/api/experimental/mcp/docs")
}

func TestEphemeralDeletesOnClose(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{Ephemeral: true})
	_ = sess.Close()
	f.next("DELETE", "/api/session/"+fakeSession)
}

func TestPrivateServer(t *testing.T) {
	f := newFakeServer(t)
	closed := make(chan struct{})
	spawner, started := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		for _, kv := range h.Cmd.Env {
			if v, ok := strings.CutPrefix(kv, "OPENCODE_PASSWORD="); ok {
				f.mu.Lock()
				f.password = v
				f.mu.Unlock()
			}
		}
		h.Send(map[string]any{"url": f.srv.URL})
		h.WaitClosed()
		close(closed)
	})
	p := opencode.New(opencode.Options{Path: "oc", Spawner: spawner})
	sess, err := p.StartSession(context.Background(), agent.Config{Cwd: workDir, SystemPrompt: "You are terse.", Env: map[string]string{"A": "1"}, UnsetEnv: []string{"B"}})
	if err != nil {
		t.Fatal(err)
	}
	h := <-started
	if got := strings.Join(h.Cmd.Args, " "); got != "serve --stdio --hostname 127.0.0.1 --port 0" || h.Cmd.Path != "oc" || h.Cmd.Dir != workDir {
		t.Fatalf("command = %s %s in %s", h.Cmd.Path, got, h.Cmd.Dir)
	}
	// A session server loads its project's config.
	for _, kv := range h.Cmd.Env {
		if strings.HasPrefix(kv, "OPENCODE_DISABLE_PROJECT_CONFIG=") || strings.HasPrefix(kv, "OPENCODE_CONFIG_PROJECT_DISABLE=") {
			t.Fatalf("session server env has %s", kv)
		}
	}
	if !slices.Contains(h.Cmd.UnsetEnv, "OPENCODE_SERVER_PASSWORD") || !slices.Contains(h.Cmd.UnsetEnv, "B") || !slices.Contains(h.Cmd.Env, "A=1") {
		t.Fatalf("env = %v unset %v", h.Cmd.Env, h.Cmd.UnsetEnv)
	}
	var config string
	for _, kv := range h.Cmd.Env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT="); ok {
			config = v
		}
	}
	if !strings.Contains(config, `"build":{"system":"You are terse."}`) || !strings.Contains(config, `"plan":{"system":"You are terse."}`) {
		t.Fatalf("config = %s", config)
	}
	if sess.Server() == nil || !sess.Server().Private() {
		t.Fatal("not a private server")
	}
	_ = sess.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("server input not closed")
	}
	// A shared or attached server refuses SystemPrompt.
	if _, err := f.provider().Start(context.Background(), agent.Config{Cwd: workDir, SystemPrompt: "x"}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("attached SystemPrompt: %v", err)
	}
}

func TestPrivateServerExit(t *testing.T) {
	f := newFakeServer(t)
	stop := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		h.Send(map[string]any{"url": f.srv.URL})
		<-stop
		h.Stderr("boom")
	})
	sess, err := opencode.New(opencode.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{Cwd: workDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Server().Err(); err != nil {
		t.Fatalf("running server Err = %v", err)
	}
	close(stop)
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end with the server")
	}
	var exit *agent.ExitError
	if !errors.As(sess.Err(), &exit) {
		t.Fatalf("err = %v", sess.Err())
	}
	if !errors.As(sess.Server().Err(), &exit) {
		t.Errorf("exited server Err = %v", sess.Server().Err())
	}
}

func TestSharedServer(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	spawns := 0
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		mu.Lock()
		spawns++
		mu.Unlock()
		if slices.Contains(h.Cmd.Env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
			t.Error("the shared server, which serves sessions, has project config off")
		}
		h.Send(map[string]any{"url": f.srv.URL})
		h.WaitClosed()
	})
	p := opencode.New(opencode.Options{Spawner: spawner, SharedServer: true, HelperDir: t.TempDir()})
	a, err := p.Start(context.Background(), agent.Config{Cwd: workDir, Env: map[string]string{"K": "V"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := f.next("PUT", "/api/session/"+fakeSession+"/environment"); fakeproc.Str(r.Body, "variables", "K") != "V" {
		t.Fatalf("environment = %v", r.Body)
	}
	b, err := p.Start(context.Background(), agent.Config{Cwd: workDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := spawns
	mu.Unlock()
	if n != 1 {
		t.Fatalf("spawned %d servers", n)
	}
	_ = a.Close()
	_ = b.Close()
}

// TestAnswerWhileTurnEnds answers a request with AllowSession, whose rule
// update is in flight, while the execution ends and withdraws the request.
func TestAnswerWhileTurnEnds(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	turn := send(t, sess, agent.Text("run"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "shell", "resources": []any{"ls"}, "save": []any{"ls *"}})
	req := waitFor[*agent.PermissionRequest](t, ev)
	gate := make(chan struct{})
	f.on("PATCH /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		<-gate
		return 204, nil
	})
	answered := make(chan error, 1)
	go func() { answered <- req.AllowForSession(context.Background()) }()
	f.next("PATCH", "/api/session/"+fakeSession)
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	close(gate)
	select {
	case err := <-answered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answer deadlocked")
	}
	waitTurn(t, turn)
}

func TestTurnOptionsOfAnotherTypeRefused(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	_, err := sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("x")}, ProviderOptions: "options"})
	if err == nil || !strings.Contains(err.Error(), "Input.ProviderOptions holds string; want *opencode.TurnOptions") {
		t.Errorf("Send = %v", err)
	}
	_, err = sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("x")}, Delivery: "Steer"})
	if err == nil || !strings.Contains(err.Error(), `unknown Input.Delivery "Steer"`) {
		t.Errorf("Send with bad delivery = %v", err)
	}
}

func TestListSubagents(t *testing.T) {
	f := newFakeServer(t)
	child := func(id, parent string) map[string]any {
		n := sessionInfo(id)
		n["parentID"] = parent
		n["agent"] = "general"
		return n
	}
	f.on("GET /api/session", func(r *fakeRequest) (int, any) {
		switch r.Query.Get("parentID") {
		case fakeSession:
			return 200, map[string]any{"data": []any{child("ses_kid", fakeSession)}, "cursor": map[string]any{}}
		case "ses_kid":
			return 200, map[string]any{"data": []any{child("ses_grandkid", "ses_kid")}, "cursor": map[string]any{}}
		case "ses_grandkid":
			return 500, map[string]any{"_tag": "UnknownError", "message": "broken"}
		}
		return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}}
	})
	p := f.provider()
	ctx := context.Background()
	tasks, err := p.ListSubagents(ctx, fakeSession, agent.ListSubagentsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != "ses_kid" || tasks[0].ParentID != "" || tasks[1].ID != "ses_grandkid" ||
		tasks[1].ParentID != "ses_kid" || tasks[1].Subagent.Depth != 2 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if _, err := p.ListSubagents(ctx, "ses_missing", agent.ListSubagentsOptions{}); !errors.Is(err, agent.ErrSessionNotFound) {
		t.Errorf("unknown session = %v, want ErrSessionNotFound", err)
	}
}
