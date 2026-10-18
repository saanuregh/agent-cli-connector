package pi_test

import (
	"context"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// Interrupt asked while the prompt start is in flight, before Pi begins its
// pre-prompt compaction: is the compaction aborted once it starts?
func TestRound5InterruptBeforeCompactionStart(t *testing.T) {
	got := make(chan string, 8)
	interrupted := make(chan struct{})
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		p := h.RecvWhere(isCommand("prompt"))
		<-interrupted
		h.Send(map[string]any{"type": "compaction_start", "reason": "threshold"})
		done := make(chan struct{})
		go func() {
			defer close(done)
			m := h.Recv()
			if m != nil {
				got <- fakeproc.Str(m, "type")
				respond(h, m, map[string]any{})
			}
		}()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			got <- "none-during-compaction"
		}
		h.Send(map[string]any{"type": "compaction_end", "reason": "threshold", "result": map[string]any{"summary": "s"}})
		respond(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		for m := h.Recv(); m != nil; m = h.Recv() {
			respond(h, m, map[string]any{})
		}
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = sess.Send(ctx, agent.Text("hi")) }()
	time.Sleep(100 * time.Millisecond)
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	close(interrupted)
	first := <-got
	if first != "abort" {
		t.Errorf("compaction that began after the interrupt was not aborted: %s", first)
	}
	wg.Wait()
}
