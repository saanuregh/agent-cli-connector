package codex_test

import (
	"context"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// An app-server that exits right after answering thread/start can end
// before the session registers; the session must still end.
func TestSessionEndsWhenServerExitsAfterThreadStart(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) { startThread(t, h) })
	for i := range 200 {
		shared := i%2 == 1
		p := codex.New(codex.Options{Spawner: spawner, SharedServer: shared})
		sess, err := p.StartSession(context.Background(), agent.Config{})
		if err == nil {
			select {
			case <-sess.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("shared=%v: session did not end after the app-server exited", shared)
			}
		}
		_ = p.Close()
	}
}
