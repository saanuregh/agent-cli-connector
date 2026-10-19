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

// Interrupt during /compact should send abort (Pi's abort aborts compaction).
func TestRound3InterruptDuringCompactSendsAbort(t *testing.T) {
	got := make(chan string, 8)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		c := h.RecvWhere(isCommand("compact"))
		got <- "compact"
		for {
			m := h.Recv()
			if m == nil {
				return
			}
			typ := fakeproc.Str(m, "type")
			got <- typ
			respond(h, m, map[string]any{})
			if typ == "abort" {
				h.Send(map[string]any{"id": c["id"], "type": "response", "command": "compact", "success": false, "error": "Compaction cancelled"})
			}
		}
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	turn, err := sess.Send(ctx, agent.Text("/compact"))
	if err != nil {
		t.Fatal(err)
	}
	<-got
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	var seen []string
	timeout := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case s := <-got:
			seen = append(seen, s)
		case <-timeout:
			break loop
		}
	}
	t.Logf("commands after interrupt: %v", seen)
	wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	res, err := turn.Wait(wctx)
	t.Logf("compact turn: %+v %v", res, err)
	for _, s := range seen {
		if s == "abort" {
			return
		}
	}
	t.Errorf("no abort sent")
}

// CompactWithResult whose ctx ends frees the slot while Pi still compacts.
func TestRound3CompactCancelFreesSlotEarly(t *testing.T) {
	order := make(chan string, 8)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		c := h.RecvWhere(isCommand("compact"))
		order <- "compact-received"
		m := h.Recv()
		if m == nil {
			return
		}
		order <- fakeproc.Str(m, "type") + "-while-compacting"
		switch fakeproc.Str(m, "type") {
		case "prompt":
			h.Send(map[string]any{"id": m["id"], "type": "response", "command": "prompt", "success": false, "error": "Cannot submit a prompt while compaction is in progress. Wait for compaction to finish and retry."})
		case "abort":
			respond(h, m, nil)
		}
		respond(h, c, map[string]any{"summary": "s"})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, err := sess.CompactWithResult(cctx, ""); t.Logf("compact: %v", err) }()
	<-order
	turn, err := sess.Send(ctx, agent.Text("next"))
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	select {
	case s := <-order:
		t.Logf("pi got %s", s)
		if s == "prompt-while-compacting" {
			wctx, c2 := context.WithTimeout(ctx, time.Second)
			defer c2()
			_, err := turn.Wait(wctx)
			t.Errorf("queued input started while Pi compacts; turn err: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// Send whose ctx ends while the prompt command waits leaves Pi's run untracked.
func TestRound3SendCtxExpiresDuringPrompt(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		p := h.RecvWhere(isCommand("prompt"))
		time.Sleep(150 * time.Millisecond)
		h.Send(map[string]any{"type": "agent_start"})
		respond(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	c := collect(sess)
	sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	in := agent.Text("hello")
	in.ClientID = "client-1"
	_, err = sess.Send(sctx, in)
	t.Logf("Send err: %v", err)
	ev := c.waitFor(t, func(e agent.Event) bool { _, ok := e.(*agent.TurnCompleted); return ok })
	if err != nil {
		t.Errorf("Send failed (%v) but Pi ran the prompt as turn %s", err, ev.Meta().TurnID)
	}
}
