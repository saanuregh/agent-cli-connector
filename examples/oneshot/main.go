// Command oneshot sends one prompt to a harness, streams the reply, and
// prints what the turn used. It is the smallest complete program: start a
// session, send, stream, wait, close.
//
//	go run ./examples/oneshot "Summarize this repository in three bullets"
//	go run ./examples/oneshot -provider codex "What does go.mod require?"
//	git diff | go run ./examples/oneshot -provider pi "Review this diff:"
//
// Tool calls that need approval are refused, so the agent can read but not
// change anything. The session is ephemeral: nothing is left in the
// harness's session list.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func main() {
	providerName := flag.String("provider", "claude", "provider: claude, codex, pi, or opencode")
	model := flag.String("model", "", "model to use (default: the harness's)")
	flag.Parse()

	prompt := strings.Join(flag.Args(), " ")
	if stat, _ := os.Stdin.Stat(); stat.Mode()&os.ModeCharDevice == 0 {
		piped, err := io.ReadAll(os.Stdin)
		if err != nil {
			fail(err)
		}
		prompt = strings.TrimSpace(prompt + "\n\n" + string(piped))
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: oneshot [-provider name] [-model id] <prompt>")
		os.Exit(2)
	}

	if err := run(*providerName, *model, prompt); err != nil {
		fail(err)
	}
}

func run(providerName, model, prompt string) error {
	// Ctrl+C cancels the context, which stops waiting; the deferred Close
	// then stops the harness.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	registry := agent.NewRegistry(claude.New(claude.Options{}), codex.New(codex.Options{}), pi.New(pi.Options{}), opencode.New(opencode.Options{}))
	sess, err := registry.Start(ctx, providerName, agent.Config{
		Model:     model,
		Ephemeral: true,
		Handlers: agent.Handlers{
			Permission: func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
				return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "This run is read-only."}
			},
		},
	})
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()

	turn, err := sess.Send(ctx, agent.Text(prompt))
	if err != nil {
		return err
	}
	// The turn's own events, so this program never reads Session.Events.
	for ev := range turn.Events() {
		switch ev := ev.(type) {
		case *agent.TextDelta:
			if ev.ParentID == "" { // not a subagent's text
				fmt.Print(ev.Delta)
			}
		case *agent.ItemStarted:
			if t := ev.Item.Tool; t != nil {
				fmt.Fprintf(os.Stderr, "[%s %s]\n", t.Kind, t.Name)
			}
		}
	}

	res, err := turn.Wait(ctx)
	if res == nil {
		return err
	}
	fmt.Println()
	u := res.Usage
	fmt.Fprintf(os.Stderr, "\n%s on %s: %s, %d in / %d out tokens", sess.Provider(), sess.Info().Model, res.Status, u.InputTokens, u.OutputTokens)
	if u.CostUSD > 0 {
		fmt.Fprintf(os.Stderr, ", $%.4f", u.CostUSD)
	}
	fmt.Fprintf(os.Stderr, ", %s\n", res.Duration.Round(100*time.Millisecond))
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
