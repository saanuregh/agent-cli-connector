package opencode_test

import (
	"context"
	"errors"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the fourth review. Interrupts now wait for a steer
// request in flight, so the reviewer's reproductions are reshaped where
// they called Interrupt during one; the cancellation windows that remain
// come from a turn ending through an event while the steer is on its way.

// steerScene starts a turn and a steer whose request waits for gate.
func steerScene(t *testing.T) (f *fakeServer, sess agentSession, turn *agent.Turn, steer chan *agent.Turn, steerID func() string, gate chan struct{}, ev *events) {
	t.Helper()
	f = newFakeServer(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	s, ev := startFake(t, f, agent.Config{})
	turn = send(t, s, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	gate = make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/prompt", func(r *fakeRequest) (int, any) {
		if r.Body["delivery"] == "steer" {
			<-gate
		}
		return 200, data(map[string]any{"id": r.Body["id"]})
	})
	steer = make(chan *agent.Turn, 1)
	go func() {
		st, _ := s.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer})
		steer <- st
	}()
	req := f.next("POST", "/api/session/"+fakeSession+"/prompt")
	return f, s, turn, steer, func() string { return req.Body["id"].(string) }, gate, ev
}

type agentSession = agent.Session

// The turn ends while the steer is on its way, and OpenCode runs the
// steer as its own execution before the request returns: that run is
// interrupted, not left running.
func TestCancelledSteerAdoptedIsInterrupted(t *testing.T) {
	f, _, turn, steer, steerID, gate, ev := steerScene(t)
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	close(gate)
	st := <-steer
	if st.ID() != steerID() {
		t.Fatalf("steer turn %s, request %s", st.ID(), steerID())
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, st); res.Status != agent.TurnInterrupted {
		t.Fatalf("steer turn = %+v", res)
	}
	assertOnlyTurns(t, ev, turn.ID(), st.ID())
}

// The same, with the execution starting after the request returned: the
// taken-back steer's execution is adopted under the steer's own ID and
// interrupted, never as a phantom turn.
func TestCancelledSteerLateExecutionIsInterrupted(t *testing.T) {
	f, _, turn, steer, steerID, gate, ev := steerScene(t)
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	close(gate)
	st := <-steer
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID())
	f.emit("session.execution.started", map[string]any{})
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, st); res.Status != agent.TurnInterrupted {
		t.Fatalf("steer turn = %+v", res)
	}
	assertOnlyTurns(t, ev, turn.ID(), st.ID())
}

// A taken-back steer whose take-back OpenCode reports ends at once with
// ErrCancelled.
func TestCancelledSteerTakeBackReported(t *testing.T) {
	f, sess, turn, steer, steerID, gate, _ := steerScene(t)
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	close(gate)
	st := <-steer
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID())
	f.emit("session.inbox.cancelled", map[string]any{"inboxID": steerID()})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := st.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Fatalf("steer turn = %v", err)
	}
	next := send(t, sess, agent.Text("next"))
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "next" {
		t.Fatalf("prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, next)
}

func assertOnlyTurns(t *testing.T, ev *events, ids ...string) {
	t.Helper()
	for _, e := range ev.all() {
		if s, ok := e.(*agent.TurnStarted); ok {
			known := false
			for _, id := range ids {
				known = known || s.TurnID == id
			}
			if !known {
				t.Fatalf("phantom turn %s started", s.TurnID)
			}
		}
	}
}

// A turn that starts while a reconnect reads /active is not ended by the
// reconcile.
func TestReconcileKeepsTurnStartedMeanwhile(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}} })
	sess, _ := startFake(t, f, agent.Config{})
	turns := make(chan *agent.Turn, 1)
	f.on("GET /api/session/active", func(*fakeRequest) (int, any) {
		go func() {
			st, err := sess.Send(context.Background(), agent.Text("new"))
			if err != nil {
				t.Errorf("send: %v", err)
			}
			turns <- st
		}()
		for f.count("POST", "/api/session/"+fakeSession+"/prompt") == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		f.emit("session.execution.started", map[string]any{})
		time.Sleep(100 * time.Millisecond)
		return 200, data(map[string]any{})
	})
	f.dropStreams()
	turn := <-turns
	time.Sleep(500 * time.Millisecond)
	select {
	case <-turn.Done():
		res, err := turn.Wait(context.Background())
		t.Fatalf("running turn ended by reconcile: %+v %v", res, err)
	default:
	}
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, turn)
}

// A turn that ends while a steer applies its model: the steer is never
// sent and fails with ErrCancelled.
func TestTurnEndsDuringSteerSetup(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/model", func(*fakeRequest) (int, any) { <-gate; return 204, nil })
	steer := make(chan *agent.Turn, 1)
	go func() {
		st, _ := sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer, Model: "opencode/test-model#high"})
		steer <- st
	}()
	f.next("POST", "/api/session/"+fakeSession+"/model")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	close(gate)
	st := <-steer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := st.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Fatalf("steer turn = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.count("POST", "/api/session/"+fakeSession+"/prompt"); n != 1 {
		t.Fatalf("prompts = %d; the steer was sent after its turn ended", n)
	}
}

// Interrupt while a steer applies its model waits for the steer, then
// stops the turn it joined.
func TestInterruptDuringSteerSetup(t *testing.T) {
	f := newFakeServer(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, _ := startFake(t, f, agent.Config{})
	turn := send(t, sess, agent.Text("count"))
	f.next("POST", "/api/session/"+fakeSession+"/prompt")
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	gate := make(chan struct{})
	f.on("POST /api/session/"+fakeSession+"/model", func(*fakeRequest) (int, any) { <-gate; return 204, nil })
	steer := make(chan *agent.Turn, 1)
	go func() {
		st, _ := sess.Send(context.Background(), agent.Input{Parts: []agent.Part{agent.TextPart("steer")}, Delivery: agent.DeliverSteer, Model: "opencode/test-model#high"})
		steer <- st
	}()
	f.next("POST", "/api/session/"+fakeSession+"/model")
	interrupted := make(chan error, 1)
	go func() { interrupted <- sess.Interrupt(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	if st := <-steer; st != turn {
		t.Fatal("the steer did not join the running turn")
	}
	if err := <-interrupted; err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("turn = %+v", res)
	}
}

// A failed interrupt leaves the turn and its steer as they were: the
// steer, never taken back, still holds the turn's end until it runs.
func TestFailedInterruptKeepsSteer(t *testing.T) {
	f, sess, turn, steer, steerID, gate, _ := steerScene(t)
	f.on("POST /api/session/"+fakeSession+"/interrupt", func(*fakeRequest) (int, any) {
		return 500, map[string]any{"_tag": "InternalError", "message": "boom"}
	})
	interrupted := make(chan error, 1)
	go func() { interrupted <- sess.Interrupt(context.Background()) }()
	close(gate)
	if st := <-steer; st != turn {
		t.Fatal("the steer did not join the running turn")
	}
	if err := <-interrupted; err == nil {
		t.Fatal("the failed interrupt reported success")
	}
	if n := f.count("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID()); n != 0 {
		t.Fatal("took the steer back although the interrupt failed")
	}
	f.emit("session.execution.succeeded", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	select {
	case <-turn.Done():
		t.Fatal("the turn ended although its steer had not run")
	default:
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.inbox.delivered", map[string]any{"inboxID": steerID()})
	f.emit("session.execution.succeeded", map[string]any{})
	if res := waitTurn(t, turn); res.Status != agent.TurnSucceeded {
		t.Fatalf("turn = %+v", res)
	}
}

// A grant on a resumed plan-mode session keeps the plan file writable:
// the inherited rules follow the grant in their own order.
func TestGrantKeepsInheritedRuleOrder(t *testing.T) {
	f := newFakeServer(t)
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		info["agent"] = "plan"
		info["permissions"] = []any{
			map[string]any{"action": "shell", "resource": "*", "effect": "ask"},
			map[string]any{"action": "edit", "resource": "*", "effect": "deny"},
			map[string]any{"action": "edit", "resource": "/work/.opencode/plans/*", "effect": "allow"},
		}
		return 200, data(info)
	})
	f.on("POST /api/session/"+fakeSession+"/permission/*", func(*fakeRequest) (int, any) { return 204, nil })
	sess, ev := startFake(t, f, agent.Config{Resume: fakeSession})
	send(t, sess, agent.Text("x"))
	f.emit("session.execution.started", map[string]any{})
	f.emit("permission.asked", map[string]any{"id": "per_1", "action": "shell", "resources": []any{"ls"}, "save": []any{"ls"}})
	if err := waitFor[*agent.PermissionRequest](t, ev).AllowForSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.next("PATCH", "/api/session/"+fakeSession)
	rules := ruleList(t, f.next("PATCH", "/api/session/"+fakeSession).Body["permissions"])
	last := ""
	for _, r := range rules {
		if r == "edit:*=deny" || r == "edit:/work/.opencode/plans/*=allow" {
			last = r
		}
	}
	if last != "edit:/work/.opencode/plans/*=allow" {
		t.Fatalf("plan file write denied after the grant: %v", rules)
	}
}
