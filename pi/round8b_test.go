package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestRound8RefreshKeepsSwitchedCwd(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s.jsonl")
	os.WriteFile(file, []byte(`{"type":"session","version":3,"id":"pi-2","cwd":"/other"}`+"\n"), 0o600)
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		for {
			m := h.Recv()
			if m == nil {
				return
			}
			switch fakeproc.Str(m, "type") {
			case "switch_session":
				respond(h, m, map[string]any{"cancelled": false})
			case "get_state":
				respond(h, m, map[string]any{"sessionId": "pi-2", "sessionFile": file, "thinkingLevel": "high"})
			case "get_commands":
				respond(h, m, map[string]any{"commands": []any{}})
			default:
				respond(h, m, map[string]any{})
			}
		}
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.SwitchSession(ctx, file); err != nil {
		t.Fatal(err)
	}
	t.Logf("after switch cwd=%q", sess.Info().Cwd)
	if err := sess.SetEffort(ctx, agent.Effort("high")); err != nil {
		t.Fatal(err)
	}
	if got := sess.Info().Cwd; got != "/other" {
		t.Errorf("after SetEffort cwd=%q, want /other", got)
	}
}
