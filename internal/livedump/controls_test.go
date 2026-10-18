//go:build live

package livedump

import (
	"context"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// TestUniformControls drives SetTitle, Compact, and ForkSession through the
// optional uniform interfaces on every provider.
func TestUniformControls(t *testing.T) {
	providers := map[string]agent.Provider{
		"claude": claude.New(claude.Options{}),
		"codex":  codex.New(codex.Options{}),
		"pi":     pi.New(pi.Options{}),
	}
	models := map[string]string{"claude": "haiku"}
	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cwd := t.TempDir()
			store := livetest.New(t, p, cwd)
			var env map[string]string
			if name == "pi" {
				env = livetest.PiEnv(t)
			}
			sess, err := p.Start(ctx, agent.Config{Cwd: cwd, Env: env, Model: models[name], Effort: agent.EffortLow, PermissionMode: agent.PermissionBypass,
				Handlers: agent.Handlers{UserInput: func(context.Context, *agent.UserInputRequest) agent.UserInputResponse {
					return agent.UserInputResponse{Cancelled: true}
				}}})
			if err != nil {
				t.Fatal(err)
			}
			store.Track(sess)
			defer sess.Close()
			go func() {
				for ev := range sess.Events() {
					switch ev.(type) {
					case *agent.TextDelta, *agent.ReasoningDelta, *agent.ToolInputDelta, *agent.UsageUpdated:
					default:
						if testing.Verbose() {
							t.Log(describe(ev))
						}
					}
				}
			}()
			if _, err := sess.Run(ctx, agent.Text("The code word is KIWI. Reply with OK.")); err != nil {
				t.Fatal(err)
			}
			if err := sess.(agent.Titler).SetTitle(ctx, "code word test"); err != nil {
				t.Errorf("SetTitle: %v", err)
			}
			// Pi refuses to compact a session this small; that refusal is the
			// harness's answer, not a library failure.
			if err := sess.(agent.Compacter).Compact(ctx, ""); err != nil && !strings.Contains(err.Error(), "Nothing to compact") {
				t.Errorf("Compact: %v", err)
			}
			fork, err := sess.(agent.Forker).ForkSession(ctx, agent.Config{})
			if err != nil {
				t.Fatalf("ForkSession: %v", err)
			}
			store.Track(fork)
			defer fork.Close()
			go func() {
				for range fork.Events() {
				}
			}()
			if fork.ID() == sess.ID() {
				t.Errorf("fork kept the session ID %s", fork.ID())
			}
			res, err := fork.Run(ctx, agent.Text("What was the code word? Reply with just the word."))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.ToUpper(res.Text), "KIWI") {
				t.Errorf("fork reply %q", res.Text)
			}
		})
	}
}
