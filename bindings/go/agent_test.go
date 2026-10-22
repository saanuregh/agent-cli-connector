package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector/bindings/go"
)

func start(t *testing.T, opts ...agent.SessionOption) (context.Context, *agent.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client, err := agent.Open(ctx, agent.WithProviders(map[string]any{"fake": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	sess, err := client.Start(ctx, "fake", agent.Config{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, sess
}

func TestTurnStreamsDeltas(t *testing.T) {
	ctx, sess := start(t)
	turn, err := sess.Send(ctx, agent.Text("hello brave world"))
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	for ev := range turn.Events() {
		if d, ok := ev.Payload.(agent.TextDelta); ok {
			streamed.WriteString(d.Delta)
		}
	}
	res, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello brave world" || streamed.String() != res.Text {
		t.Fatalf("result %q, streamed %q", res.Text, streamed.String())
	}

	// The session stream has the same turn, from session_started on.
	var types []agent.EventType
	for ev := range sess.Events() {
		types = append(types, ev.Type)
		if ev.Type == agent.EventTypeTurnCompleted {
			break
		}
	}
	if types[0] != agent.EventTypeSessionStarted {
		t.Fatalf("first event %s", types[0])
	}

	if err := sess.Call(ctx, "no_such_method", nil, nil); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("native call: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Send(ctx, agent.Text("again")); !errors.Is(err, agent.ErrSessionNotFound) && !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("send after close: %v", err)
	}
}

func TestPermissionRequest(t *testing.T) {
	ctx, sess := start(t)
	turn, err := sess.Send(ctx, agent.Text("needs permission"))
	if err != nil {
		t.Fatal(err)
	}
	for ev := range turn.Events() {
		if req, ok := ev.Payload.(agent.PermissionRequest); ok {
			if err := sess.Allow(ctx, req.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := turn.Wait(ctx)
	if err != nil || res.Text != "needs permission" {
		t.Fatalf("result %+v, error %v", res, err)
	}

	// A handler answers instead, and the request is not an event.
	ctx, sess = start(t, agent.WithHandlers(agent.Handlers{
		Permission: func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{Decision: agent.PermissionDecisionDeny}
		},
	}))
	res, err = sess.Run(ctx, agent.Text("needs permission"))
	if err != nil || res.Text != "permission denied" {
		t.Fatalf("result %+v, error %v", res, err)
	}
}

func TestHostTool(t *testing.T) {
	type args struct{}
	lookup := agent.NewTool("lookup", "Looks up a ticket.", json.RawMessage(`{"type":"object"}`),
		func(context.Context, args) (agent.ToolResult, error) {
			return agent.TextResult("ticket 42"), nil
		})
	ctx, sess := start(t, agent.WithTools(lookup))
	res, err := sess.Run(ctx, agent.Text("tool:lookup"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ticket 42" {
		t.Fatalf("result %q", res.Text)
	}
}

func TestInterruptCancelsToolCall(t *testing.T) {
	type args struct{}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	slow := agent.NewTool("slow", "Waits until cancelled.", json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, _ args) (agent.ToolResult, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return agent.ErrorResult("cancelled"), nil
		})
	ctx, sess := start(t, agent.WithTools(slow))
	turn, err := sess.Send(ctx, agent.Text("tool:slow"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	// An interrupted turn returns its result and an error naming it.
	if res, err := turn.Wait(ctx); res == nil || res.Status != agent.TurnStatusInterrupted || !errors.Is(err, agent.ErrInterrupted) {
		t.Fatalf("result %+v, error %v", res, err)
	}
}
