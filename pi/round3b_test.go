package pi_test

import (
	"context"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestRound3ReusedClientID(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		for i, text := range []string{"one", "two"} {
			p := h.RecvWhere(isCommand("prompt"))
			respond(h, p, map[string]any{"disposition": "started"})
			h.Send(map[string]any{"type": "agent_start"})
			h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
			if i == 1 {
				time.Sleep(100 * time.Millisecond)
			}
			h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	c := collect(sess)
	in := agent.Text("hi")
	in.ClientID = "c1"
	r1, err := sess.Run(ctx, in)
	t.Logf("first: %q %v", r1.Text, err)
	r2, err := sess.Run(ctx, in)
	t.Logf("second: %q %v", r2.Text, err)
	time.Sleep(200 * time.Millisecond)
	n := 0
	for _, e := range c.all() {
		if _, ok := e.(*agent.TurnCompleted); ok {
			n++
		}
	}
	if r2.Text != "two" || n != 2 {
		t.Errorf("second result text %q, TurnCompleted events %d", r2.Text, n)
	}
}
