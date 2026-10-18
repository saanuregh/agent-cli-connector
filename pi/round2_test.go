package pi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// A steer sent while the turn it targets is still starting is queued:
// Pi, still idle, would otherwise start a run for it ahead of that turn.
func TestSteerDuringStartIsQueued(t *testing.T) {
	gotSetModel, steerSent := make(chan struct{}), make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		setModel := h.RecvWhere(isCommand("set_model"))
		close(gotSetModel)
		<-steerSent
		respond(h, setModel, map[string]any{"id": "x", "provider": "openrouter"})
		st := h.RecvWhere(isCommand("get_state"))
		respond(h, st, map[string]any{"sessionId": "pi-1", "model": map[string]any{"id": "x", "provider": "openrouter"}})
		for _, want := range []string{"go", "also", "third"} {
			prompt := h.RecvWhere(isCommand("prompt"))
			if got := fakeproc.Str(prompt, "message"); got != want {
				t.Errorf("prompt %q, want %q", got, want)
			}
			respond(h, prompt, map[string]any{"disposition": "started"})
			h.Send(map[string]any{"type": "agent_start"})
			h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		}
		h.WaitClosed()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	firstCh := make(chan *agent.Turn, 1)
	go func() {
		in := agent.Text("go")
		in.Model = "openrouter/x"
		turn, err := sess.Send(ctx, in)
		if err != nil {
			t.Error(err)
		}
		firstCh <- turn
	}()
	// The first input's start waits on set_model.
	<-gotSetModel
	steer := agent.Text("also")
	steer.Delivery = agent.DeliverSteer
	st, err := sess.Send(ctx, steer)
	close(steerSent)
	if err != nil {
		t.Fatal(err)
	}
	first := <-firstCh
	third, err := sess.Send(ctx, agent.Text("third"))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range []*agent.Turn{first, st, third} {
		if res, err := turn.Wait(ctx); err != nil || res.Status != agent.TurnSucceeded {
			t.Errorf("turn %s: %+v, %v", turn.ID(), res, err)
		}
	}
}

// An interrupt asked while the prompt starts is sent when Pi answers
// "queued" too: the input joined a run Pi was already doing.
func TestInterruptWhileStartingQueuedPrompt(t *testing.T) {
	gotPrompt, interrupted := make(chan struct{}), make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		prompt := h.RecvWhere(isCommand("prompt"))
		close(gotPrompt)
		<-interrupted
		h.Send(map[string]any{"type": "agent_start"})
		respond(h, prompt, map[string]any{"disposition": "queued"})
		if next := h.Recv(); fakeproc.Str(next, "type") != "abort" {
			t.Errorf("after the queued reply got %v, want abort", next)
		} else {
			respond(h, next, nil)
		}
		h.Send(map[string]any{"type": "agent_settled", "aborted": true})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	done := make(chan *agent.Turn, 1)
	go func() {
		turn, err := sess.Send(ctx, agent.Text("go"))
		if err != nil {
			t.Error(err)
		}
		done <- turn
	}()
	<-gotPrompt
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	close(interrupted)
	if res, err := (<-done).Wait(ctx); !errors.Is(err, agent.ErrInterrupted) || res.Status != agent.TurnInterrupted {
		t.Errorf("result %+v, %v", res, err)
	}
}

// Run whose context ends while its input is queued drops that input and
// leaves the running turn alone.
func TestRunCancelWhileQueuedDropsOnlyItsInput(t *testing.T) {
	unexpected := make(chan string, 4)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		p := h.RecvWhere(isCommand("prompt"))
		respond(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		for {
			m := h.Recv()
			if m == nil {
				return
			}
			switch typ := fakeproc.Str(m, "type"); typ {
			case "abort", "prompt":
				unexpected <- typ
				respond(h, m, map[string]any{"disposition": "started"})
			case "finish":
				respond(h, m, nil)
				h.Send(map[string]any{"type": "agent_settled", "aborted": false})
			default:
				respond(h, m, map[string]any{})
			}
		}
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	first, err := sess.Send(ctx, agent.Text("long job"))
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := sess.Run(rctx, agent.Text("queued question")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v", err)
	}
	// Let Pi finish the first run; the dropped input must not start.
	_, _ = sess.Command(ctx, "finish", nil)
	if res, err := first.Wait(ctx); err != nil || res.Status != agent.TurnSucceeded {
		t.Errorf("first turn %+v, %v", res, err)
	}
	select {
	case typ := <-unexpected:
		t.Errorf("Pi got %s", typ)
	case <-time.After(200 * time.Millisecond):
	}
}
