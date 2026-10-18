package pi_test

import (
	"context"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// Pi compacts in _prepareRun, before the "started" reply and agent_start.
func TestRound3PrePromptCompactionUsage(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		p := h.RecvWhere(isCommand("prompt"))
		h.Send(map[string]any{"type": "compaction_start", "reason": "threshold"})
		h.Send(map[string]any{"type": "compaction_end", "reason": "threshold", "result": map[string]any{"summary": "s", "tokensBefore": 100,
			"usage": map[string]any{"input": 1000, "output": 50, "totalTokens": 1050, "cost": map[string]any{"total": 0.5}}}})
		respond(h, p, map[string]any{"disposition": "started"})
		h.Send(map[string]any{"type": "agent_start"})
		h.Send(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
			"usage": map[string]any{"input": 10, "output": 5, "totalTokens": 15, "cost": map[string]any{"total": 0.01}}}})
		h.Send(map[string]any{"type": "agent_settled", "aborted": false})
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.Run(ctx, agent.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("usage %+v", res.Usage)
	if res.Usage.TotalTokens != 1065 {
		t.Errorf("compaction usage not counted: total %d", res.Usage.TotalTokens)
	}
}
