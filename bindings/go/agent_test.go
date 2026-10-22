package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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
	events := sess.Events()
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
	for ev := range events {
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

	// A handler answers instead; the request is still an event, marked handled.
	ctx, sess = start(t, agent.WithHandlers(agent.Handlers{
		Permission: func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{Decision: agent.PermissionDecisionDeny}
		},
	}))
	turn, err = sess.Send(ctx, agent.Text("needs permission"))
	if err != nil {
		t.Fatal(err)
	}
	handled := 0
	for ev := range turn.Events() {
		if _, ok := ev.Payload.(agent.PermissionRequest); ok && sess.Handled(ev) {
			handled++
		}
	}
	res, err = turn.Wait(ctx)
	if err != nil || res.Text != "permission denied" || handled != 1 {
		t.Fatalf("result %+v, error %v, handled %d", res, err, handled)
	}
}

func TestDialogAndNativeHelpers(t *testing.T) {
	ctx, sess := start(t)
	cases := []struct {
		prompt string
		answer func(agent.Event) error
		want   string
	}{
		{"dialog", func(ev agent.Event) error {
			req := ev.Payload.(agent.DialogRequest)
			return req.Complete(ctx, sess, "ok")
		}, `dialog completed "ok"`},
		{"dialog", func(ev agent.Event) error {
			req := ev.Payload.(agent.DialogRequest)
			return req.Cancel(ctx, sess)
		}, "dialog cancelled"},
		{"native", func(ev agent.Event) error {
			req := ev.Payload.(agent.NativeRequest)
			return req.Reply(ctx, sess, "pong")
		}, `native result "pong"`},
		{"native", func(ev agent.Event) error {
			req := ev.Payload.(agent.NativeRequest)
			return req.Fail(ctx, sess, 7, "nope")
		}, "native error 7 nope"},
	}
	for _, c := range cases {
		turn, err := sess.Send(ctx, agent.Text(c.prompt))
		if err != nil {
			t.Fatal(err)
		}
		for ev := range turn.Events() {
			if ev.Type == agent.EventTypeDialogRequest || ev.Type == agent.EventTypeNativeRequest {
				if sess.Handled(ev) {
					t.Fatal("unhandled request marked handled")
				}
				if err := c.answer(ev); err != nil {
					t.Fatal(err)
				}
			}
		}
		if res, err := turn.Wait(ctx); err != nil || res.Text != c.want {
			t.Fatalf("result %+v, error %v, want %q", res, err, c.want)
		}
	}
}

func TestHandlerPanicFailsClosed(t *testing.T) {
	ctx, sess := start(t, agent.WithHandlers(agent.Handlers{
		Dialog: func(context.Context, *agent.DialogRequest) agent.DialogResponse { panic("boom") },
		Native: func(context.Context, *agent.NativeRequest) agent.NativeResponse { panic("boom") },
	}))
	for prompt, want := range map[string]string{"dialog": "dialog cancelled", "native": "native error -32603 boom"} {
		if res, err := sess.Run(ctx, agent.Text(prompt)); err != nil || res.Text != want {
			t.Fatalf("result %+v, error %v, want %q", res, err, want)
		}
	}
	ctx, sess = start(t, agent.WithHandlers(agent.Handlers{
		Dialog: func(context.Context, *agent.DialogRequest) agent.DialogResponse {
			return agent.DialogResponse{Behavior: agent.DialogResponseBehaviorCompleted}
		},
	}))
	if res, err := sess.Run(ctx, agent.Text("dialog")); err != nil || res.Text != "dialog completed" {
		t.Fatalf("result %+v, error %v", res, err)
	}
}

func TestSubscribe(t *testing.T) {
	ctx, sess := start(t)
	var mu sync.Mutex
	var got []agent.Event
	firstDone := make(chan struct{})
	unsubscribe := sess.Subscribe(func(ev agent.Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ev)
		if ev.Type == agent.EventTypeTurnCompleted {
			close(firstDone)
		}
	})
	sess.Subscribe(func(agent.Event) { panic("listener failure") })
	if _, err := sess.Run(ctx, agent.Text("one")); err != nil {
		t.Fatal(err)
	}
	<-firstDone
	unsubscribe()
	turn, err := sess.Send(ctx, agent.Text("two"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ev := range got {
		if ev.TurnID == turn.ID() {
			t.Fatalf("listener called after unsubscribe: %s", ev.Type)
		}
	}
}

func TestSubscribeListenerAnswersRequest(t *testing.T) {
	ctx, sess := start(t)
	sess.Subscribe(func(ev agent.Event) {
		if req, ok := ev.Payload.(agent.PermissionRequest); ok {
			if err := req.Allow(ctx, sess); err != nil {
				t.Error(err)
			}
		}
	})
	if res, err := sess.Run(ctx, agent.Text("needs permission")); err != nil || res.Text != "needs permission" {
		t.Fatalf("result %+v, error %v", res, err)
	}
}

func TestEventsBufferFromFirstCall(t *testing.T) {
	ctx, sess := start(t)
	if _, err := sess.Run(ctx, agent.Text("before")); err != nil {
		t.Fatal(err)
	}
	events := sess.Events()
	turn, err := sess.Send(ctx, agent.Text("after"))
	if err != nil {
		t.Fatal(err)
	}
	first := true
	for ev := range events {
		if first {
			first = false
			if ev.Type != agent.EventTypeSessionStarted {
				t.Fatalf("first event %s", ev.Type)
			}
			continue
		}
		if ev.Type == agent.EventTypeSessionStarted || (ev.TurnID != "" && ev.TurnID != turn.ID()) {
			t.Fatalf("event from before Events: %s turn %q", ev.Type, ev.TurnID)
		}
		if ev.Type == agent.EventTypeTurnCompleted {
			break
		}
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
