package opencode_test

import (
	"context"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the third review, ported from the reviewer's
// reproductions and asserting the fixed behavior.

// A turn whose execution ends while a reconnect reads /active is not
// followed by an adopted phantom execution.
func TestNoAdoptAfterTurnEnds(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}} })
	sess, ev := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("x"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) {
		f.emit("session.execution.succeeded", map[string]any{})
		time.Sleep(200 * time.Millisecond)
		return 200, data(map[string]any{fakeSession: map[string]any{"type": "running"}})
	})
	f.dropStreams()
	waitTurn(t, turn)
	time.Sleep(500 * time.Millisecond)
	if started, completed := countOf[*agent.TurnStarted](ev), countOf[*agent.TurnCompleted](ev); started != completed {
		t.Fatalf("stranded adopted turn: %d started, %d completed", started, completed)
	}
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) { return 200, data(map[string]any{}) })
	next := send(t, sess, agent.Text("next"))
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "next" {
		t.Fatalf("prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, next)
}

// A steer taken back after its turn was interrupted is taken back even
// when the caller's context ended meanwhile.
func TestSteerTakeBackIgnoresCancelledCtx(t *testing.T) {
	f := newFakeServer(t)
	deletes := make(chan string, 8)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(r *fakeRequest) (int, any) { deletes <- r.Path; return 204, nil })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		<-gate
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *agent.Turn, 1)
	go func() {
		st, _ := sess.Send(ctx, agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
		done <- st
	}()
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	cancel()
	close(gate)
	<-done
	select {
	case <-deletes:
	case <-time.After(5 * time.Second):
		t.Fatal("steer not taken back")
	}
}

// Interrupt while a steer's request is in flight waits for the request,
// then stops the turn the steer joined and takes the steer back; later
// input runs.
func TestInterruptDuringSteerPost(t *testing.T) {
	f := newFakeServer(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		if r.Body["delivery"] == "steer" {
			<-gate
		}
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	steerDone := make(chan *agent.Turn, 1)
	go func() {
		st, _ := sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
		steerDone <- st
	}()
	steerReq := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	interrupted := make(chan error, 1)
	go func() { interrupted <- sess.Interrupt(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	if n := f.count("POST", "/api/session/"+fakeSession+"/interrupt"); n != 0 {
		t.Fatal("interrupted while the steer request was in flight")
	}
	close(gate)
	if st := <-steerDone; st != turn {
		t.Fatal("the steer did not join the running turn")
	}
	if err := <-interrupted; err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerReq.Body["id"].(string))
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("turn = %+v", res)
	}
	next := send(t, sess, agent.Text("after"))
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "after" {
		t.Fatalf("prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, next)
}

// A grant on a resumed session in an empty mode re-applies the session's
// inherited denies, and then its own host tool rules after them.
func TestInheritedHostDenyAfterGrant(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		info["permissions"] = []any{map[string]any{"action": "agenthost-*", "resource": "*", "effect": "deny"},
			map[string]any{"action": "edit", "resource": "*", "effect": "ask"}}
		return 200, data(info)
	})
	f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	tool := agent.Tool{Name: "lookup", Description: "d", Handler: func(context.Context, agent.ToolCallRequest) (agent.ToolResult, error) { return agent.ToolResult{}, nil }}
	sess, ev := startFake(t, f, agent.Config{Resume: fakeSession, Tools: []agent.Tool{tool}})
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "edit", "resources": []any{"a.go"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.next("PATCH", "/api/session/"+fakeSession)
	rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	own, lastDeny := -1, -1
	for i, r := range rules {
		if strings.HasPrefix(r, "agenthost-") && strings.Contains(r, "_*:*=allow") {
			own = i
		}
		if r == "agenthost-*:*=deny" {
			lastDeny = i
		}
	}
	if own < 0 || lastDeny > own {
		t.Fatalf("own host tools denied after the grant: %v", rules)
	}
}

// External fields are acknowledged and hidden fields take their defaults,
// as OpenCode's reply check requires.
func TestExternalAndHiddenFormFields(t *testing.T) {
	f := newFakeServer(t)
	f.on("POST /api/session/"+fakeSession+"/form/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("ask"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("form.created", map[string]any{"sessionID": "", "form": map[string]any{
		"id": "frm_1", "sessionID": fakeSession, "title": "Auth",
		"fields": []any{
			map[string]any{"key": "open", "type": "external", "url": "https://example.com/auth"},
			map[string]any{"key": "region", "type": "string", "hidden": true, "required": true, "default": "eu"},
			map[string]any{"key": "zone", "type": "string", "when": []any{map[string]any{"key": "region", "op": "eq", "value": "eu"}}},
		},
	}})
	req := waitFor[*agent.UserInputRequest](t, ev)
	if err := req.Answer(context.Background(), map[string][]string{"open": {"done"}, "zone": {"west"}}); err != nil {
		t.Fatal(err)
	}
	answer := f.next("POST", "/api/session/"+fakeSession+"/form/frm_1/reply").Body["answer"].(map[string]any)
	if answer["open"] != true || answer["region"] != "eu" || answer["zone"] != "west" {
		t.Fatalf("answer = %v", answer)
	}
}

// Interrupting a held turn while OpenCode already runs the steers'
// execution leaves the turn to that execution's end, so the end does not
// hit the queued turn that starts next.
func TestHeldInterruptWithRunningSteerExecution(t *testing.T) {
	f := newFakeServer(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	next := send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("next")}, Delivery: agent.DeliverInterrupt})
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	time.Sleep(50 * time.Millisecond)
	select {
	case <-turn.Done():
		t.Fatal("the held turn ended before the steers' execution did")
	default:
	}
	if n := f.count("POST", "/api/session/"+fakeSession+"/prompt"); n != 2 {
		t.Fatalf("prompts = %d; queued input started while the steers' execution ran", n)
	}
	// The steers' execution, started before the interrupt reached
	// OpenCode, reports its start and its interruption.
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("held turn = %+v", res)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "next" {
		t.Fatalf("prompt = %v", r.Body)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-next.Done():
		t.Fatal("the queued turn ended by a stray end event")
	default:
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, next)
}

// A slow MCP connect of one session does not hold up a session in another
// directory.
func TestMCPConnectDoesNotBlockOtherDirectories(t *testing.T) {
	f := newFakeServer(t)
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	release := make(chan struct{})
	f.on("PUT /api/experimental/mcp/*", func(r *fakeRequest) (int, any) {
		if strings.HasSuffix(r.Path, "/slow") {
			<-release
		}
		return 204, nil
	})
	defer close(release)
	p := f.provider()
	a, err := p.Start(context.Background(), agent.Config{Cwd: "/work", MCPServers: map[string]agent.MCPServer{"fast": agent.StdioServer("x")}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b, err := p.Start(context.Background(), agent.Config{Cwd: "/other", MCPServers: map[string]agent.MCPServer{"slow": agent.StdioServer("y")}})
		if err == nil {
			_ = b.Close()
		}
	}()
	f.next("PUT", "/api/experimental/mcp/slow")
	closed := make(chan struct{})
	go func() { _ = a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for another directory's MCP connect")
	}
}

func TestInvalidServerURLIsRedacted(t *testing.T) {
	for _, u := range []string{"user:hunter2@host", "http//user:hunter2@", "%zz:hunter2"} {
		_, err := newFakeServer(t).providerWithURL(u).Start(context.Background(), agent.Config{Cwd: "/work"})
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("%q: %v", u, err)
		}
	}
}
