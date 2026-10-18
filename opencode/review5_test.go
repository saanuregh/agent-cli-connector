package opencode_test

import (
	"context"
	"errors"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Regression tests for the fifth review, ported from the reviewer's
// reproductions and asserting the fixed behavior.

// An interrupt recorded for a steer's execution that waits for steerMu
// while that execution ends is dropped, not sent to the turn that
// replaced it.
func TestStaleInterruptSkipsNextTurn(t *testing.T) {
	f, sess, turn, steer, steerID, gate, _ := steerScene(t)
	delGate := make(chan struct{})
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) { <-delGate; return 204, nil })
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	next := send(t, sess, agent.Text("next"))
	close(gate)
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID())
	// OpenCode starts the steer's execution while the take-back is in
	// flight, and the execution ends.
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	f.emit("session.execution.succeeded", map[string]any{})
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "next" {
		t.Fatalf("prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	before := f.count("POST", "/api/session/"+fakeSession+"/interrupt")
	close(delGate)
	<-steer
	time.Sleep(200 * time.Millisecond)
	if after := f.count("POST", "/api/session/"+fakeSession+"/interrupt"); after != before {
		t.Fatalf("a stale interrupt was sent while turn %s runs", next.ID())
	}
	f.emit("session.execution.succeeded", map[string]any{})
	if res := waitTurn(t, next); res.Status != agent.TurnSucceeded {
		t.Fatalf("next = %+v", res)
	}
}

// A take-back that fails still ends the steer after the grace period, so
// queued input starts.
func TestFailedTakeBackFreesQueue(t *testing.T) {
	f, sess, turn, steer, steerID, gate, _ := steerScene(t)
	f.on("DELETE /api/session/"+fakeSession+"/inbox/*", func(*fakeRequest) (int, any) {
		return 500, map[string]any{"_tag": "InternalError", "message": "boom"}
	})
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	waitTurn(t, turn)
	close(gate)
	st := <-steer
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID())
	next := send(t, sess, agent.Text("next"))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := st.Wait(ctx); !errors.Is(err, agent.ErrCancelled) {
		t.Fatalf("steer turn = %v", err)
	}
	if r := f.next("POST", "/api/session/"+fakeSession+"/prompt"); r.Body["text"] != "next" {
		t.Fatalf("prompt = %v", r.Body)
	}
	f.emit("session.execution.started", map[string]any{})
	f.emit("session.execution.succeeded", map[string]any{})
	waitTurn(t, next)
}

// An interrupt whose context ends while it waits for a steer in flight is
// still sent.
func TestInterruptSentAfterItsContextEnds(t *testing.T) {
	f, sess, turn, steer, _, gate, _ := steerScene(t)
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		done <- sess.(agent.TurnInterrupter).InterruptTurn(ctx, turn.ID())
	}()
	time.Sleep(600 * time.Millisecond)
	close(gate)
	<-steer
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.next("POST", "/api/session/"+fakeSession+"/interrupt")
	f.emit("session.execution.interrupted", map[string]any{"reason": "user"})
	if res := waitTurn(t, turn); res.Status != agent.TurnInterrupted {
		t.Fatalf("turn = %+v", res)
	}
}

// Close on an attached server takes back a steer parked for an execution
// of its own, which would otherwise run unwatched.
func TestCloseTakesBackParkedSteer(t *testing.T) {
	f, sess, turn, steer, steerID, gate, _ := steerScene(t)
	f.on("GET /api/session/"+fakeSession+"/permission", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session/"+fakeSession+"/form", func(*fakeRequest) (int, any) { return 200, data([]any{}) })
	f.on("GET /api/session", func(*fakeRequest) (int, any) { return 200, map[string]any{"data": []any{}, "cursor": map[string]any{}} })
	f.on("GET /api/session/"+fakeSession, func(*fakeRequest) (int, any) {
		info := sessionInfo(fakeSession)
		info["outcome"] = "succeeded"
		return 200, data(info)
	})
	// A reconnect ends the turn as succeeded while the steer is on its
	// way, so the steer waits for an execution of its own.
	f.dropStreams()
	waitTurn(t, turn)
	close(gate)
	st := <-steer
	if st == turn {
		t.Fatal("the steer joined the ended turn")
	}
	if n := f.count("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID()); n != 0 {
		t.Fatal("took back a steer OpenCode is about to run")
	}
	_ = sess.Close()
	f.next("DELETE", "/api/session/"+fakeSession+"/inbox/"+steerID())
}
