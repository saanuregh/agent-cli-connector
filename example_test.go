package agent_test

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
)

// The examples use agenttest, a scripted fake provider, so they run without
// a harness CLI. A real program starts claude.New, codex.New, pi.New, or
// opencode.New the same way.

// A session runs turns: Run sends input and waits for the result.
func Example() {
	provider := agenttest.New(func(t *agenttest.Turn) error {
		t.Text("Hello from " + t.Input().PlainText() + ".")
		return nil
	})
	ctx := context.Background()
	sess, err := provider.Start(ctx, agent.Config{})
	if err != nil {
		panic(err)
	}
	defer sess.Close()

	res, err := sess.Run(ctx, agent.Text("the example"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Status, res.Text)
	// Output: completed Hello from the example.
}

// A turn's own events stream its reply while it runs.
func ExampleTurn_Events() {
	provider := agenttest.New(func(t *agenttest.Turn) error {
		t.Text("one ")
		t.Text("two")
		return nil
	})
	ctx := context.Background()
	sess, _ := provider.Start(ctx, agent.Config{})
	defer sess.Close()

	turn, _ := sess.Send(ctx, agent.Text("count"))
	for ev := range turn.Events() {
		if d, ok := ev.(*agent.TextDelta); ok {
			fmt.Print(d.Delta)
		}
	}
	fmt.Println()
	// Output: one two
}

// Handlers answer requests without reading events, here a policy that
// refuses shell commands.
func ExampleHandlers() {
	provider := agenttest.New(func(t *agenttest.Turn) error {
		resp := t.Permission(agent.PermissionRequest{Kind: agent.PermissionCommand, ToolName: "bash", Command: "rm -rf build"})
		t.Text(string(resp.Decision) + ": " + resp.Message)
		return nil
	})
	ctx := context.Background()
	sess, _ := provider.Start(ctx, agent.Config{
		Handlers: agent.Handlers{
			Permission: func(_ context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
				if req.Kind == agent.PermissionCommand {
					return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "no shell commands"}
				}
				return agent.PermissionResponse{Decision: agent.DecisionAllow}
			},
		},
	})
	defer sess.Close()

	res, _ := sess.Run(ctx, agent.Text("clean up"))
	fmt.Println(res.Text)
	// Output: deny: no shell commands
}

// A Transcript folds events into the items a UI shows.
func ExampleTranscript() {
	provider := agenttest.New(func(t *agenttest.Turn) error {
		t.ToolCall(agent.ToolShell, "bash", `{"command":"ls"}`, "main.go")
		t.Text("There is one file.")
		return nil
	})
	ctx := context.Background()
	sess, _ := provider.Start(ctx, agent.Config{})

	var transcript agent.Transcript
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sess.Events() {
			transcript.Apply(ev)
		}
	}()
	_, _ = sess.Run(ctx, agent.Text("list files"))
	_ = sess.Close()
	<-done

	for _, it := range transcript.Items() {
		if it.Tool != nil {
			fmt.Println(it.Kind, it.Tool.Name, it.Tool.Output)
			continue
		}
		fmt.Println(it.Kind, it.Text)
	}
	// Output:
	// user_message list files
	// tool_call bash main.go
	// assistant_message There is one file.
}

// NewTool turns a Go function into a tool the model can call; the library
// serves it over each harness's own extension point.
func ExampleNewTool() {
	type args struct {
		City string `json:"city"`
	}
	weather := agent.NewTool("weather", "Returns the weather for a city.",
		json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		func(_ context.Context, a args) (agent.ToolResult, error) {
			return agent.TextResult("Sunny in " + a.City), nil
		})
	weather.ReadOnly = true
	_ = agent.Config{Tools: []agent.Tool{weather}}
}

// EventType names an event's type, for logs and wire formats.
func ExampleEventType() {
	fmt.Println(agent.EventType(&agent.TaskUpdated{}))
	// Output: TaskUpdated
}

// Usage.Add sums token counts and cost; context sizes take the latest value.
func ExampleUsage_Add() {
	var total agent.Usage
	total.Add(agent.Usage{InputTokens: 100, OutputTokens: 20, ContextTokens: 120})
	total.Add(agent.Usage{InputTokens: 50, OutputTokens: 10, ContextTokens: 180})
	fmt.Println(total.InputTokens, total.OutputTokens, total.ContextTokens)
	// Output: 150 30 180
}
