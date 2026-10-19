package opencode_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// Regression tests for the sixth review; the first three are the
// reviewer's reproductions, asserting the fixed behavior.

// Leaving plan with SetAgent restores the mode before it, so a later grant
// keeps that mode's asks.
func TestGrantAfterLeavingPlanKeepsAsks(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	ctx := context.Background()
	if err := sess.SetAgent(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
	if err := sess.SetAgent(ctx, "build"); err != nil {
		t.Fatal(err)
	}
	if mode := sess.Info().PermissionMode; mode != agent.PermissionDefault {
		t.Fatalf("mode after leaving plan = %q", mode)
	}
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "edit", "resources": []any{"a.go"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(ctx); err != nil {
		t.Fatal(err)
	}
	var rules []string
	for !slices.Contains(rules, "edit:*=allow") {
		rules = ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	}
	if !slices.Contains(rules, "shell:*=ask") {
		t.Fatalf("rules = %v", rules)
	}
}

// An empty mode on a new session keeps the rules it was created with.
func TestEmptyModeKeepsNewSessionRules(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	ctx := context.Background()
	if err := sess.SetPermissionMode(ctx, ""); err != nil {
		t.Fatal(err)
	}
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "shell", "resources": []any{"ls -la"}, "save": []any{"ls *"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(ctx); err != nil {
		t.Fatal(err)
	}
	var rules []string
	for {
		rules = ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
		if slices.Contains(rules, "shell:ls *=allow") {
			break
		}
	}
	if !slices.Contains(rules, "shell:*=ask") || !slices.Contains(rules, "edit:*=ask") {
		t.Fatalf("rules = %v", rules)
	}
}

// Close takes back the running turn's undelivered steer.
func TestCloseTakesBackJoinedSteer(t *testing.T) {
	f := newFakeServer(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	if st := send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer}); st != turn {
		t.Fatal("the steer did not join the running turn")
	}
	id := f.next("POST", "/api/session/"+fakeSession+"/prompt").Body["id"].(string)
	_ = sess.Close()
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+id)
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
}

// Interrupt stops background subagents even when its context ended while
// it waited for a steer.
func TestInterruptStopsBackgroundAfterCtxEnds(t *testing.T) {
	f, sess, _, steer, _, gate, ev := steerScene(t)
	f.on("POST /api/session/ses_child/interrupt", func(*fakeRequest) (int, any) { return 200, map[string]any{"interrupted": true} })
	f.emit("session.tool.input.started", map[string]any{"id": "call_s", "name": "subagent"})
	f.emit("session.tool.called", map[string]any{"id": "call_s", "input": map[string]any{"prompt": "p", "background": true}})
	f.emit("session.created", map[string]any{"sessionID": "ses_child", "parentID": fakeSession, "title": "child"})
	f.emit("session.tool.progress", map[string]any{"id": "call_s", "metadata": map[string]any{"sessionID": "ses_child"}})
	f.emit("session.tool.success", map[string]any{"id": "call_s", "content": []any{}, "metadata": map[string]any{"sessionID": "ses_child", "status": "running"}})
	ev.wait(t, func(e agent.Event) bool {
		u, ok := e.(*agent.TaskUpdated)
		return ok && u.Task.ID == "ses_child" && u.Task.Subagent != nil && u.Task.Subagent.Background
	})
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		done <- sess.Interrupt(ctx)
	}()
	time.Sleep(600 * time.Millisecond)
	close(gate)
	<-steer
	<-done
	f.next("POST", "/api/session/ses_child/interrupt")
}

// A steer delivered into a turn belongs to that turn in history, and
// ForkAtTurn keeps it.
func TestHistorySteerJoinsTurn(t *testing.T) {
	f := newFakeServer(t)
	msgs := []any{
		map[string]any{"id": "msg_u1", "type": "user", "text": "first"},
		map[string]any{"id": "msg_a1", "type": "assistant", "content": []any{map[string]any{"type": "text", "text": "one"}}},
		map[string]any{"id": "msg_s1", "type": "user", "text": "steer"},
		map[string]any{"id": "msg_a2", "type": "assistant", "content": []any{map[string]any{"type": "text", "text": "two"}}},
		map[string]any{"id": "msg_i1", "type": "idle", "outcome": "succeeded"},
		// OpenCode puts a mode reminder before the next prompt.
		map[string]any{"id": "msg_sw", "type": "agent-switched"},
		map[string]any{"id": "msg_r1", "type": "synthetic", "text": "<system-reminder>plan</system-reminder>"},
		map[string]any{"id": "msg_u2", "type": "user", "text": "second"},
		map[string]any{"id": "msg_a3", "type": "assistant", "content": []any{map[string]any{"type": "text", "text": "three"}}},
		map[string]any{"id": "msg_i2", "type": "idle", "outcome": "succeeded"},
		// A subagent report wakes the session: a turn without a prompt.
		map[string]any{"id": "msg_rep", "type": "synthetic", "text": "report"},
		map[string]any{"id": "msg_a4", "type": "assistant", "content": []any{map[string]any{"type": "text", "text": "four"}}},
		map[string]any{"id": "msg_i3", "type": "idle", "outcome": "succeeded"},
	}
	f.on("GET /api/session/"+fakeSession+"/message", func(r *fakeRequest) (int, any) { return pageMessages(r, msgs) })
	items, err := f.provider().ReadHistory(context.Background(), fakeSession, agent.ReadHistoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turns := map[string]string{}
	for _, it := range items {
		turns[it.ID] = it.TurnID
	}
	if turns["msg_s1"] != "msg_u1" || turns["msg_a2:text:0"] != "msg_u1" || turns["msg_u2"] != "msg_u2" ||
		turns["msg_r1"] != "msg_u2" || turns["msg_rep"] != "msg_rep" || turns["msg_a4:text:0"] != "msg_rep" {
		t.Fatalf("turns = %v", turns)
	}
	sess, _ := startFake(t, f, agent.Config{})
	forked, err := sess.ForkSession(context.Background(), agent.Config{ForkAtTurn: "msg_u1"})
	if err != nil {
		t.Fatal(err)
	}
	defer forked.Close()
	if r := f.next("POST", "/api/session/"+fakeSession+"/fork"); r.Body["before"] != "msg_u2" {
		t.Fatalf("fork = %v", r.Body)
	}
}

// A grant racing a mode change writes the new mode's rules: the grant's
// rule write starts first and is slow, the mode change waits for it, and
// the rules the server keeps are the new mode's.
func TestGrantDuringModeChange(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{PermissionMode: agent.PermissionDefault})
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "edit", "resources": []any{"a.go"}, "save": []any{"*"}})
	req := waitFor[*agent.PermissionRequest](t, ev)
	gate := make(chan struct{})
	var mu sync.Mutex
	var kept []string
	first := true
	f.on("PATCH /api/session/"+fakeSession, func(r *fakeRequest) (int, any) {
		mu.Lock()
		wait := first
		first = false
		mu.Unlock()
		if wait {
			<-gate
		}
		mu.Lock()
		kept = ruleList(t, r.Body["permissions"])
		mu.Unlock()
		return 204, nil
	})
	grantDone := make(chan error, 1)
	go func() { grantDone <- req.AllowForSession(context.Background()) }()
	f.next("PATCH", "/api/session/"+fakeSession)
	modeDone := make(chan error, 1)
	go func() { modeDone <- sess.SetPermissionMode(context.Background(), agent.PermissionReadOnly) }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if err := <-grantDone; err != nil {
		t.Fatal(err)
	}
	if err := <-modeDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// readOnly's deny decides edits, so the grant answers no ask there.
	if !slices.Contains(kept, "*:*=deny") || slices.Contains(kept, "edit:*=allow") {
		t.Fatalf("the server kept another mode's rules: %v", kept)
	}
}

// A steer still on its way when the session closes is taken back: Close
// waits for the request, and the steer, admitted while closing, takes
// itself back.
func TestCloseWhileSteerInFlight(t *testing.T) {
	f, sess, _, steer, steerID, gate, _ := steerScene(t)
	closed := make(chan struct{})
	go func() { _ = sess.Close(); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	<-steer
	<-closed
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID())
}

// A failed interrupt whose turn ended meanwhile takes the turn's steers
// back instead of restoring them.
func TestFailedInterruptAfterTurnEnded(t *testing.T) {
	f := newFakeServer(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
	id := f.next("POST", "/api/session/"+fakeSession+"/prompt").Body["id"].(string)
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/interrupt", func(*fakeRequest) (int, any) {
		<-gate
		return 500, map[string]any{"_tag": "InternalError", "message": "boom"}
	})
	done := make(chan error, 1)
	go func() { done <- sess.Interrupt(context.Background()) }()
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.failed", map[string]any{"error": map[string]any{"type": "x", "message": "y"}})
	waitTurn(t, turn)
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("the failed interrupt reported success")
	}
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+id)
}

// A provider call on a shared server keeps it running while a session
// closes.
func TestProviderCallLeasesSharedServer(t *testing.T) {
	f := newFakeServer(t)
	stopped := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		h.Send(map[string]any{"url": f.srv.URL})
		h.WaitClosed()
		close(stopped)
	})
	gate := make(chan struct{})
	p := opencode.New(opencode.Options{Spawner: spawner, SharedServer: true, HelperDir: t.TempDir()})
	sess, err := p.Start(context.Background(), agent.Config{Cwd: workDir})
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	var once sync.Once
	f.on("GET /api/model", func(*fakeRequest) (int, any) {
		once.Do(func() { close(reached) })
		<-gate
		return 200, map[string]any{"data": []any{}}
	})
	listed := make(chan error, 1)
	go func() { _, err := p.ListModels(context.Background()); listed <- err }()
	<-reached
	_ = sess.Close()
	select {
	case <-stopped:
		t.Fatal("the shared server stopped under a provider call")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	if err := <-listed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the shared server outlived its last user")
	}
}

func TestHostToolsOnAttachedServerNeedOptIn(t *testing.T) {
	f := newFakeServer(t)
	tool := agent.Tool{Name: "t", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) { return agent.ToolResult{}, nil }}
	p := opencode.New(opencode.Options{ServerURL: f.srv.URL, Password: "secret"})
	if _, err := p.Start(context.Background(), agent.Config{Cwd: workDir, Tools: []agent.Tool{tool}}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// A grant covers the ask of a host tool on a session whose empty mode
// keeps the rules it was created with.
func TestGrantCoversHostToolAskInEmptyMode(t *testing.T) {
	f := newFakeServer(t)
	f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	tool := agent.Tool{Name: "secret", NeedsApproval: true, Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) { return agent.ToolResult{}, nil }}
	sess, ev := startFake(t, f, agent.Config{Tools: []agent.Tool{tool}})
	created := ruleList(t, f.next("POST", "/api/session").Body["permissions"])
	var ask string
	for _, r := range created {
		if len(r) > 4 && r[len(r)-4:] == "=ask" {
			ask = r[:len(r)-len("=ask")]
		}
	}
	if ask == "" {
		t.Fatalf("no host tool ask in %v", created)
	}
	send(t, sess, agent.Text("go"))
	f.emit("session.execution.started", map[string]any{})
	action := ask[:len(ask)-len(":*")]
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": action, "resources": []any{"*"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	lastAsk, grant := -1, slices.Index(rules, ask+"=allow")
	for i, r := range rules {
		if r == ask+"=ask" {
			lastAsk = i
		}
	}
	if grant < 0 || lastAsk > grant {
		t.Fatalf("the grant does not cover the host tool's ask: %v", rules)
	}
}
