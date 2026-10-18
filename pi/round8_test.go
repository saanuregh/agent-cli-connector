package pi_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// A switch whose ctx ends after the command was written keeps the slot
// until Pi answers, so queued input never reaches the session Pi is
// tearing down.
func TestRound8SwitchCtxCancelKeepsSlot(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	gotSwitch := make(chan struct{})
	prompted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		sw := h.RecvWhere(isCommand("new_session"))
		close(gotSwitch)
		go func() {
			time.Sleep(300 * time.Millisecond)
			note("switch-answered")
			respond(h, sw, map[string]any{"cancelled": false})
		}()
		for m := h.Recv(); m != nil; m = h.Recv() {
			typ := fakeproc.Str(m, "type")
			note(typ)
			switch typ {
			case "prompt":
				respond(h, m, map[string]any{"disposition": "started"})
				h.Send(map[string]any{"type": "agent_start"})
				h.Send(map[string]any{"type": "agent_settled", "aborted": false})
				close(prompted)
			case "get_commands":
				respond(h, m, map[string]any{"commands": []any{}})
			default:
				respond(h, m, map[string]any{"sessionId": "pi-2"})
			}
		}
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := sess.NewSession(cctx, ""); done <- err }()
	<-gotSwitch
	turn, err := sess.Send(ctx, agent.Text("after"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("NewSession = %v", err)
	}
	if res, err := turn.Wait(ctx); err != nil || res.Status != agent.TurnSucceeded {
		t.Errorf("queued turn %+v, %v", res, err)
	}
	<-prompted
	mu.Lock()
	defer mu.Unlock()
	for _, s := range order {
		if s == "prompt" {
			t.Errorf("prompt reached Pi before the switch was answered: %v", order)
			break
		}
		if s == "switch-answered" {
			break
		}
	}
	if sess.ID() != "pi-2" {
		t.Errorf("session ID after the switch = %s", sess.ID())
	}
}
