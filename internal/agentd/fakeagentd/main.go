// Command fakeagentd is agentd with one scripted fake provider, "fake",
// for testing clients without a harness CLI. Each turn reads its input:
//
//	echo TEXT        replies "echo: TEXT"
//	permission       asks for permission to run "ls", replies the decision
//	ask              asks one question, replies the chosen answer
//	tool NAME ARGS   calls host tool NAME with JSON ARGS, replies its text
//	fail             fails the turn with "scripted failure"
//	wait             runs until interrupted
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
	"github.com/saanuregh/agent-cli-connector/internal/agentd"
)

func main() {
	if err := agentd.Serve(context.Background(), os.Stdin, os.Stdout, agentd.Config{Providers: []agentd.ProviderSpec{spec()}}); err != nil {
		fmt.Fprintln(os.Stderr, "fakeagentd:", err)
		os.Exit(1)
	}
}

func spec() agentd.ProviderSpec {
	p := &provider{}
	p.Provider = agenttest.New(p.play)
	return agentd.ProviderSpec{
		Name: "fake",
		New:  func(json.RawMessage, agentd.ProviderBridge) (agent.Provider, error) { return p, nil },
	}
}

// provider is agenttest under the name "fake", keeping the tools of the
// last session started so turns can call them.
type provider struct {
	*agenttest.Provider
	mu    sync.Mutex
	tools []agent.Tool
}

func (p *provider) Name() string { return "fake" }

func (p *provider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	p.mu.Lock()
	p.tools = cfg.Tools
	p.mu.Unlock()
	return p.Provider.Start(ctx, cfg)
}

func (p *provider) play(t *agenttest.Turn) error {
	verb, rest, _ := strings.Cut(t.Input().PlainText(), " ")
	switch verb {
	case "echo":
		t.Text("echo: " + rest)
	case "permission":
		resp := t.Permission(agent.PermissionRequest{Kind: agent.PermissionCommand, ToolName: "bash", Command: "ls"})
		t.Text(string(resp.Decision))
	case "ask":
		resp := t.Ask(agent.Question{ID: "q1", Text: "Pick one", Options: []agent.QuestionOption{{Label: "red"}, {Label: "blue"}}})
		t.Text(strings.Join(resp.Answers["q1"], ","))
	case "tool":
		name, args, _ := strings.Cut(rest, " ")
		p.mu.Lock()
		tools := p.tools
		p.mu.Unlock()
		for _, tool := range tools {
			if tool.Name == name {
				res, err := tool.Handler(t.Context(), agent.ToolCallRequest{ID: "call1", Name: name, Arguments: json.RawMessage(args)})
				if err != nil {
					return err
				}
				if len(res.Content) > 0 {
					t.Text(res.Content[0].Text)
				}
				return nil
			}
		}
		return fmt.Errorf("no tool %q", name)
	case "fail":
		return errors.New("scripted failure")
	case "wait":
		<-t.Context().Done()
		return t.Context().Err()
	default:
		t.Text("unknown command")
	}
	return nil
}
