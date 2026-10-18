package opencode_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/opencode"
)

// Regression tests for the second review; the first five are the
// reviewer's reproductions, asserting the fixed behavior.

// Interrupting a held turn stops OpenCode before the turn ends, so queued
// DeliverInterrupt input reaches OpenCode only after the interrupt.
func TestHeldInterruptBeforeQueuedInput(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	var order []string
	f.on("POST /api/session/"+fakeSession+"/interrupt", func(*fakeRequest) (int, any) {
		mu.Lock()
		order = append(order, "interrupt")
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		// Nothing runs: the steers' execution had not started.
		return 200, map[string]any{"interrupted": false}
	})
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) {
		mu.Lock()
		order = append(order, "take back")
		mu.Unlock()
		return 204, nil
	})
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		mu.Lock()
		order = append(order, "prompt:"+r.Body["text"].(string))
		mu.Unlock()
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	send(t, sess, agent.Input{Parts: []agent.Part{agent.TextPart("next")}, Delivery: agent.DeliverInterrupt})
	waitTurn(t, turn)
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	// The fake hands the request over before its handler records it.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 3 {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// Steers are taken back first, so OpenCode cannot start their
	// execution after the interrupt.
	if !slices.Equal(order, []string{"take back", "interrupt", "prompt:next"}) {
		t.Fatalf("order = %v", order)
	}
}

// A reconnect while a start is in flight leaves the new turn running.
func TestReconnectDuringStart(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}} })
	f.on("GET /api/session/"+fakeSession+"/message", func(r *fakeRequest) (int, any) {
		return pageMessages(r, []any{map[string]any{"id": "msg_old", "type": "user"}, map[string]any{"id": "msg_idle", "type": "idle", "outcome": "succeeded"}})
	})
	sess, _ := startFake(t, f, agent.Config{})
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		<-gate
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		info["outcome"] = "succeeded"
		return 200, data(info)
	})
	var turn *agent.Turn
	done := make(chan struct{})
	go func() { defer close(done); turn, _ = sess.Send(context.Background(), agent.Text("new")) }()
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.dropStreams()
	f.next("GET", "/api/session/active")
	time.Sleep(100 * time.Millisecond)
	close(gate)
	<-done
	// After the start: the turn's execution has neither started nor ended.
	f.dropStreams()
	f.next("GET", "/api/session/active")
	time.Sleep(200 * time.Millisecond)
	select {
	case <-turn.Done():
		res, _ := turn.Wait(context.Background())
		t.Fatalf("turn ended before its execution started: %+v", res)
	default:
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
}

// An execution that ends while /active is read is not adopted as a turn
// that would never end.
func TestNoAdoptAfterEnd(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}} })
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) {
		f.emit("session.execution.succeeded", map[string]any{})
		time.Sleep(200 * time.Millisecond)
		return 200, data(map[string]any{fakeSession: map[string]any{"type": "running"}})
	})
	sess, ev := startFake(t, f, agent.Config{Resume: fakeSession})
	time.Sleep(300 * time.Millisecond)
	if started, completed := countOf[*agent.TurnStarted](ev), countOf[*agent.TurnCompleted](ev); started != completed {
		t.Fatalf("stranded turn: %d started, %d completed", started, completed)
	}
	// The session is idle: new input starts at once.
	turn := send(t, sess, agent.Text("x"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
}

// A session registering an MCP server while the last holder removes it
// registers it again.
func TestMCPReleaseRace(t *testing.T) {
	f := newFakeServer(t)
	var mu sync.Mutex
	listed := map[string]bool{}
	f.on("GET /api/mcp", func(*fakeRequest) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		var out []any
		for n := range listed {
			out = append(out, map[string]any{"name": n})
		}
		return 200, map[string]any{"data": out}
	})
	f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) {
		mu.Lock()
		listed["docs"] = true
		mu.Unlock()
		return 204, nil
	})
	deleting := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) {
		once.Do(func() { close(deleting) })
		<-release
		mu.Lock()
		delete(listed, "docs")
		mu.Unlock()
		return 204, nil
	})
	p := f.provider()
	cfg := agent.Config{Cwd: "/work", MCPServers: map[string]agent.MCPServer{"docs": agent.StdioServer("docs-mcp")}}
	a, err := p.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Close() }()
	<-deleting
	started := make(chan agent.Session, 1)
	go func() {
		b, err := p.Start(context.Background(), cfg)
		if err != nil {
			t.Error(err)
		}
		started <- b
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	b := <-started
	if b == nil {
		return
	}
	defer b.Close()
	mu.Lock()
	defer mu.Unlock()
	if !listed["docs"] || f.count("PUT", "/api/experimental/mcp/docs") != 2 {
		t.Fatalf("session b runs without its MCP server; PUTs=%d", f.count("PUT", "/api/experimental/mcp/docs"))
	}
}

// Form conditions follow OpenCode: lists match by inclusion, and an
// unanswered key fails both eq and neq.
func TestFormWhenMatchesOpenCode(t *testing.T) {
	f := newFakeServer(t)
	sess, ev := startFake(t, f, agent.Config{})
	send(t, sess, agent.Text("ask"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("form.created", map[string]any{"sessionID": "", "form": map[string]any{
		"id": "frm_1", "sessionID": fakeSession, "title": "Q",
		"fields": []any{
			map[string]any{"key": "langs", "type": "multiselect", "options": []any{map[string]any{"value": "go", "label": "Go"}, map[string]any{"value": "rs", "label": "Rust"}}},
			map[string]any{"key": "gover", "type": "string", "when": []any{map[string]any{"key": "langs", "op": "eq", "value": "go"}}},
			map[string]any{"key": "other", "type": "string", "when": []any{map[string]any{"key": "missing", "op": "neq", "value": "x"}}},
			map[string]any{"key": "n", "type": "number"},
			map[string]any{"key": "strict", "type": "string", "when": []any{map[string]any{"key": "n", "op": "eq", "value": "3"}}},
			map[string]any{"key": "chained", "type": "string", "when": []any{map[string]any{"key": "other", "op": "neq", "value": "y"}}},
		},
	}})
	req := waitFor[*agent.UserInputRequest](t, ev)
	if err := req.Answer(context.Background(), map[string][]string{"langs": {"Go", "Rust"}, "gover": {"1.24"}, "other": {"y"}, "n": {"3"}, "strict": {"s"}, "chained": {"c"}}); err != nil {
		t.Fatal(err)
	}
	answer := f.next("POST", "/api/session/"+fakeSession+"/form/frm_1/reply").Body["answer"].(map[string]any)
	if answer["gover"] != "1.24" || answer["other"] != nil || answer["n"] != float64(3) || answer["strict"] != nil || answer["chained"] != nil {
		t.Fatalf("answer = %v", answer)
	}
}

func TestAttachedServerRefusesEnv(t *testing.T) {
	f := newFakeServer(t)
	_, err := f.provider().Start(context.Background(), agent.Config{Cwd: "/work", Env: map[string]string{"A": "1"}})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("PUT", "/api/session/"+fakeSession+"/environment"); n != 0 {
		t.Fatalf("environment sent %d times", n)
	}
}

func TestMCPConfigMismatchNotice(t *testing.T) {
	f := newFakeServer(t)
	f.on("PUT /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	f.on("DELETE /api/experimental/mcp/*", func(*fakeRequest) (int, any) { return 204, nil })
	p := f.provider()
	a, err := p.Start(context.Background(), agent.Config{Cwd: "/work", MCPServers: map[string]agent.MCPServer{"docs": agent.StdioServer("docs-mcp")}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := p.StartSession(context.Background(), agent.Config{Cwd: "/work", MCPServers: map[string]agent.MCPServer{"docs": agent.StdioServer("other-mcp")}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ev := &events{}
	go ev.drain(b)
	n := ev.wait(t, func(e agent.Event) bool {
		n, ok := e.(*agent.Notice)
		return ok && n.Code == "mcp_server_config_differs"
	})
	if n == nil {
		t.Fatal("no notice")
	}
}

// With an empty mode, a grant does not lift a resumed session's denies.
func TestGrantKeepsInheritedDenies(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		info["permissions"] = []any{map[string]any{"action": "edit", "resource": "*", "effect": "deny"}}
		return 200, data(info)
	})
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{Resume: fakeSession})
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "edit", "resources": []any{"a.go"}, "save": []any{"*"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The only rule update, at resume, adds the host tool isolation rule:
	// the deny decides the request, so the grant answers no ask and
	// writes nothing.
	rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	if slices.Contains(rules, "edit:*=allow") || f.count("PATCH", "/api/session/"+fakeSession) != 1 {
		t.Fatalf("rules = %v, %d updates", rules, f.count("PATCH", "/api/session/"+fakeSession))
	}
}

var _ = opencode.Name
