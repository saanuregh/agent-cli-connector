package codex

// Regression tests for races found in review. They replay an interleaving
// by setting state or calling the second half of a method directly.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

const rThread = "th-1"

func rStart(h *fakeproc.Harness) bool {
	init := h.RecvWhere(fakeproc.Method("initialize"))
	if init == nil {
		return false
	}
	h.Reply(init, map[string]any{"userAgent": "fake", "codexHome": "/h", "platformFamily": "unix", "platformOs": "linux"})
	h.RecvWhere(fakeproc.Method("initialized"))
	st := h.RecvWhere(fakeproc.Method("thread/start"))
	h.Reply(st, map[string]any{"thread": map[string]any{"id": rThread, "cwd": "/w"}, "model": "m", "cwd": "/w"})
	return true
}
func rTurnStart(h *fakeproc.Harness, id string) map[string]any {
	ts := h.RecvWhere(fakeproc.Method("turn/start"))
	if ts == nil {
		return nil
	}
	h.Reply(ts, map[string]any{"turn": map[string]any{"id": id, "status": "inProgress", "items": []any{}}})
	h.Notify("turn/started", map[string]any{"threadId": rThread, "turn": map[string]any{"id": id, "status": "inProgress", "items": []any{}}})
	return ts
}
func rTurnDone(h *fakeproc.Harness, id, status string) {
	h.Notify("turn/completed", map[string]any{"threadId": rThread, "turn": map[string]any{"id": id, "status": status, "items": []any{}}})
}

// Send(DeliverInterrupt) split around a natural turn end: the running turn
// completes after the input is queued, the queue sends turn/start for the
// input, and only then does the interrupt run. It must not stop the input;
// an unexpected turn/interrupt would go unanswered and hang the test.
func TestInterruptSparesInterruptingInput(t *testing.T) {
	startSeen := make(chan struct{})
	release := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		rTurnStart(h, "t1")
		<-release
		rTurnDone(h, "t1", "completed")
		ts := h.RecvWhere(fakeproc.Method("turn/start"))
		close(startSeen)
		time.Sleep(100 * time.Millisecond)
		h.Reply(ts, map[string]any{"turn": map[string]any{"id": "t2", "status": "inProgress", "items": []any{}}})
		h.Notify("turn/started", map[string]any{"threadId": rThread, "turn": map[string]any{"id": "t2", "status": "inProgress", "items": []any{}}})
		rTurnDone(h, "t2", "completed")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Send(ctx, agent.Text("one")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	in := agent.Text("STOP AND DO THIS")
	in.Delivery = agent.DeliverInterrupt
	x, err := s.enqueue(in)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	<-startSeen
	if err := s.interruptTurn(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := x.Wait(wctx)
	if err != nil || res.Status != agent.TurnSucceeded {
		t.Errorf("interrupting input result %+v, %v", res, err)
	}
}

func TestRewindRefusedWhileTurnRuns(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		rTurnStart(h, "t1")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Send(ctx, agent.Text("one")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := s.Rewind(ctx, "t0"); err == nil || !strings.Contains(err.Error(), "while a turn runs") {
		t.Errorf("Rewind while running = %v", err)
	}
}

// Replay: Send read the running turn t1; t1 completes before steer takes
// its handle.
func TestSteerAfterTurnEndsDoesNotOrphanTurn(t *testing.T) {
	steerSeen := make(chan struct{})
	proceed := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		rTurnStart(h, "t1")
		rTurnDone(h, "t1", "completed")
		st := h.RecvWhere(fakeproc.Method("turn/steer"))
		close(steerSeen)
		<-proceed
		h.Send(map[string]any{"id": st["id"], "error": map[string]any{"code": -32600, "message": "no active turn to steer"}})
		rTurnStart(h, "t2")
		rTurnDone(h, "t2", "completed")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Send(ctx, agent.Text("one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	in := agent.Text("steer")
	in.Delivery = agent.DeliverSteer
	type r struct {
		turn *agent.Turn
		err  error
	}
	done := make(chan r, 1)
	// Send saw t1 running before it completed; its steer runs now.
	go func() { tu, e := s.steer(ctx, in, "t1"); done <- r{tu, e} }()
	<-steerSeen
	close(proceed)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := got.turn.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if ids := s.rt.TurnIDs(); slices.Contains(ids, "t1") {
		t.Errorf("completed turn t1 re-registered and never completes; TurnIDs=%v", ids)
	}
}

// Run with -race: Transcript must not modify tool calls the translator
// still holds.
func TestTranscriptDoesNotShareToolCalls(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		rTurnStart(h, "t1")
		h.Notify("item/started", map[string]any{"threadId": rThread, "turnId": "t1", "item": map[string]any{"type": "commandExecution", "id": "c1", "command": "ls", "cwd": "/w", "status": "inProgress", "commandActions": []any{}}})
		for range 200 {
			h.Notify("item/commandExecution/outputDelta", map[string]any{"threadId": rThread, "turnId": "t1", "itemId": "c1", "delta": "x"})
		}
		rTurnDone(h, "t1", "interrupted")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var tr agent.Transcript
	t1, _ := s.Send(ctx, agent.Text("one"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range s.Events() {
			tr.Apply(ev)
			if _, ok := ev.(*agent.TurnCompleted); ok {
				return
			}
		}
	}()
	t1.Wait(ctx)
	<-done
}

func TestReviewRefusedWhileTurnRuns(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		rTurnStart(h, "t1")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Send(ctx, agent.Text("one")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := s.Review(ctx, ReviewUncommitted()); err == nil || !strings.Contains(err.Error(), "while a turn runs") {
		t.Errorf("Review while running = %v", err)
	}
	if running, _ := s.rt.Running(); running != "t1" {
		t.Errorf("running turn after the refused review = %q, want t1", running)
	}
}
