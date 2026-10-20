package agenttest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
	"github.com/saanuregh/agent-cli-connector/internal/sessiontest"
)

func TestSessionLifecycleContract(t *testing.T) {
	sess, err := agenttest.New(func(*agenttest.Turn) error { return nil }).Start(context.Background(), agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sessiontest.Lifecycle(t, sess)
}

func TestRequestContract(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		t.Permission(agent.PermissionRequest{ToolName: "edit"})
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	turn, err := sess.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	var resolved *agent.RequestResolved
	for ev := range turn.Events() {
		switch ev := ev.(type) {
		case *agent.PermissionRequest:
			if ev.Outcome() != agent.RequestPending || len(sess.PendingRequests()) != 1 {
				t.Errorf("pending request: outcome %q, pending %d", ev.Outcome(), len(sess.PendingRequests()))
			}
			if err := ev.Allow(ctx); err != nil {
				t.Fatal(err)
			}
			if err := ev.Deny(ctx, "again"); !errors.Is(err, agent.ErrRequestDone) {
				t.Errorf("second answer = %v, want ErrRequestDone", err)
			}
			select {
			case <-ev.Done():
			default:
				t.Error("Done not closed after the answer")
			}
			if ev.Outcome() != agent.RequestAnswered {
				t.Errorf("outcome = %q", ev.Outcome())
			}
		case *agent.RequestResolved:
			resolved = ev
		}
	}
	if resolved == nil || resolved.Outcome != agent.RequestAnswered {
		t.Errorf("RequestResolved = %+v", resolved)
	}
	if n := len(sess.PendingRequests()); n != 0 {
		t.Errorf("pending after answer = %d", n)
	}
}

func TestTurnContract(t *testing.T) {
	fail := errors.New("boom")
	p := agenttest.New(func(t *agenttest.Turn) error {
		t.Text("partial")
		if t.Input().PlainText() == "fail" {
			return fail
		}
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, prompt := range []string{"ok", "fail"} {
		turn, err := sess.Send(ctx, agent.Text(prompt))
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for ev := range turn.Events() {
			kinds = append(kinds, fmt.Sprintf("%T", ev))
			if ev.Meta().TurnID != turn.ID() {
				t.Errorf("%s: event %T has TurnID %q, want %q", prompt, ev, ev.Meta().TurnID, turn.ID())
			}
		}
		if len(kinds) == 0 || kinds[len(kinds)-1] != "*agent.TurnCompleted" {
			t.Errorf("%s: events %v do not end with TurnCompleted", prompt, kinds)
		}
		if !slices.Contains(kinds, "*agent.TurnStarted") {
			t.Errorf("%s: no TurnStarted in %v", prompt, kinds)
		}
		res, err := turn.Wait(ctx)
		var te *agent.TurnError
		switch prompt {
		case "ok":
			if err != nil || res.Status != agent.TurnSucceeded || res.Text != "partial" {
				t.Errorf("ok turn = %+v, %v", res, err)
			}
		case "fail":
			if res == nil || res.Status != agent.TurnFailed || !errors.As(err, &te) || err != res.Err {
				t.Errorf("failed turn = %+v, %v", res, err)
			}
		}
		again := 0
		for range turn.Events() {
			again++
		}
		if again != len(kinds) {
			t.Errorf("%s: ranging again gave %d events, want %d", prompt, again, len(kinds))
		}
	}
}

func TestNoIdleStatusWhileQueuedInputRuns(t *testing.T) {
	release := make(chan struct{})
	p := agenttest.New(func(t *agenttest.Turn) error {
		if t.Input().PlainText() == "first" {
			<-release
		}
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		sess.Close()
		for range sess.Events() {
		}
	}()
	if _, err := sess.Send(ctx, agent.Text("first")); err != nil {
		t.Fatal(err)
	}
	second, err := sess.Send(ctx, agent.Text("second"))
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := second.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	var statuses []agent.SessionStatus
	for ev := range sess.Events() {
		if s, ok := ev.(*agent.StatusChanged); ok {
			statuses = append(statuses, s.Status)
		}
		if c, ok := ev.(*agent.TurnCompleted); ok && c.TurnID == second.ID() {
			break
		}
	}
	if slices.Contains(statuses, agent.StatusIdle) {
		t.Errorf("idle reported while queued input ran: %v", statuses)
	}
}

func TestHandlerContextEndsWhenRequestWithdrawn(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		t.Permission(agent.PermissionRequest{ToolName: "edit"})
		return nil
	})
	entered, cancelled := make(chan struct{}), make(chan struct{})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{Handlers: agent.Handlers{
		Permission: func(ctx context.Context, _ *agent.PermissionRequest) agent.PermissionResponse {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	turn, err := sess.Send(ctx, agent.Text("go"))
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler context not cancelled after the request was withdrawn")
	}
	if _, err := turn.Wait(ctx); !errors.Is(err, agent.ErrInterrupted) {
		t.Errorf("turn = %v, want ErrInterrupted", err)
	}
}
