package pi

// Regression tests for races found in review. They replay an interleaving
// by calling the second half of a method directly.

import (
	"context"
	"slices"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

func rIs(typ string) func(map[string]any) bool {
	return func(m map[string]any) bool { return fakeproc.Str(m, "type") == typ }
}
func rResp(h *fakeproc.Harness, cmd map[string]any, data any) {
	h.Send(map[string]any{"id": cmd["id"], "type": "response", "command": cmd["type"], "success": true, "data": data})
}
func rStartup(h *fakeproc.Harness) bool {
	st := h.RecvWhere(rIs("get_state"))
	if st == nil {
		return false
	}
	rResp(h, st, map[string]any{"sessionId": "pi-1", "model": map[string]any{"id": "m", "provider": "p"}})
	c := h.RecvWhere(rIs("get_commands"))
	rResp(h, c, map[string]any{"commands": []any{}})
	return true
}
func rFinishRun(h *fakeproc.Harness, text string) {
	h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": text}}}})
	h.Send(map[string]any{"type": "agent_settled", "aborted": false})
}

// Replays the interleaving where the running turn settles between Send's
// unlock (running=true,current=A) and steer's NewTurn(current).
func TestSteerAfterSettleDoesNotOrphanTurn(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStartup(h) {
			return
		}
		p := h.RecvWhere(rIs("prompt"))
		rResp(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		rFinishRun(h, "one")
		p2 := h.RecvWhere(rIs("prompt"))
		rResp(h, p2, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		rFinishRun(h, "two")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// Send saw running=true,current=first before the settle; steer runs now.
	turn, err := s.steer(ctx, agent.Text("x"), first.ID(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := turn.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if ids := s.rt.TurnIDs(); slices.Contains(ids, first.ID()) {
		t.Errorf("finished turn %s re-registered and never completes; TurnIDs=%v Busy=%v", first.ID(), ids, s.rt.Busy())
	}
}

// Send(DeliverInterrupt) split around a natural settle: the running turn
// ends after the input is queued, the queue starts the input, and only then
// does the interrupt run. It must not stop the input.
func TestInterruptSparesInterruptingInput(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStartup(h) {
			return
		}
		p := h.RecvWhere(rIs("prompt"))
		rResp(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		<-time.After(50 * time.Millisecond)
		rFinishRun(h, "one")
		p2 := h.RecvWhere(rIs("prompt"))
		rResp(h, p2, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		time.Sleep(200 * time.Millisecond) // the interrupt arrives while the input runs
		rFinishRun(h, "two")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	in := agent.Text("STOP AND DO THIS")
	in.Delivery = agent.DeliverInterrupt
	x, err := s.rt.Enqueue(providerutil.NewUUID(), in)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cur, _ := s.rt.Running(); cur == x.ID() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the interrupting input never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.interruptTurn(ctx, first.ID()); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := x.Wait(wctx)
	if err != nil || res.Status != agent.TurnSucceeded || res.Text != "two" {
		t.Errorf("interrupting input result %+v, %v", res, err)
	}
}

// A steer sent while a built-in command runs has no Pi run to join; it is
// queued behind the command and the queue keeps moving.
func TestSteerDuringBuiltinIsQueued(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStartup(h) {
			return
		}
		ac := h.RecvWhere(rIs("set_auto_compaction"))
		time.Sleep(50 * time.Millisecond)
		rResp(h, ac, nil)
		p := h.RecvWhere(rIs("prompt"))
		rResp(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		rFinishRun(h, "steer reply")
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() {
		for range s.Events() {
		}
	}()
	b, err := s.Send(ctx, agent.Text("/autocompact on"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	in := agent.Text("hi")
	in.Delivery = agent.DeliverSteer
	st, err := s.Send(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := b.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	res, err := st.Wait(wctx)
	if err != nil || res.Text != "steer reply" {
		t.Errorf("steer turn %+v, %v", res, err)
	}
	if !s.rt.Idle() {
		t.Error("queue stuck: the session is not idle after its turns ended")
	}
}

// Pi starts a run for a steer, then the steer's prompt fails (here Pi
// answers with an error). The run is live, so the steer's turn must stay
// and queued input must wait for it.
func TestAdoptedSteerSurvivesFailedReply(t *testing.T) {
	queuedStarted := make(chan struct{}, 1)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStartup(h) {
			return
		}
		p := h.RecvWhere(rIs("prompt"))
		rResp(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		steer := h.RecvWhere(rIs("prompt"))
		rFinishRun(h, "one")                          // the first run ends
		h.Send(map[string]any{"type": "agent_start"}) // Pi starts a run for the steer
		h.Send(map[string]any{"id": steer["id"], "type": "response", "command": "prompt", "success": false, "error": "boom"})
		time.Sleep(100 * time.Millisecond)
		rFinishRun(h, "steered")
		q := h.RecvWhere(rIs("prompt"))
		if q != nil {
			queuedStarted <- struct{}{}
			rResp(h, q, map[string]any{"disposition": "started"})
			h.Send(map[string]any{"type": "agent_start"})
			rFinishRun(h, "queued")
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	s, err := New(Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() {
		for range s.Events() {
		}
	}()
	if _, err := s.Send(ctx, agent.Text("go")); err != nil {
		t.Fatal(err)
	}
	queued := agent.Text("later")
	in := agent.Text("steer")
	in.Delivery = agent.DeliverSteer
	st, err := s.Send(ctx, in)
	if err != nil {
		t.Fatalf("steer = %v", err)
	}
	q, err := s.Send(ctx, queued)
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := st.Wait(wctx)
	if err != nil || res.Text != "steered" {
		t.Errorf("steer turn = %+v, %v", res, err)
	}
	if res, err := q.Wait(wctx); err != nil || res.Text != "queued" {
		t.Errorf("queued turn = %+v, %v", res, err)
	}
}
