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

// An interrupt while Pi compacts before a prompt's run starts (before the
// "started" reply) is sent at once, since an abort stops the compaction.
func TestRound4InterruptDuringPrePromptCompaction(t *testing.T) {
	got := make(chan string, 8)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		p := h.RecvWhere(isCommand("prompt"))
		h.Send(map[string]any{"type": "compaction_start", "reason": "threshold"})
		// Compaction runs; see whether abort arrives within 300ms.
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
	c := collect(sess)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = sess.Send(ctx, agent.Text("hi")) }()
	c.waitFor(t, func(e agent.Event) bool {
		s, ok := e.(*agent.StatusChanged)
		return ok && s.Status == agent.StatusCompacting
	})
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-got
	if first != "abort" && first != "clear_queue" {
		t.Errorf("no abort while Pi compacts before the prompt starts: %s", first)
	}
	wg.Wait()
}
