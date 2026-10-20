package agenttest_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/agenttest"
)

func Example() {
	p := agenttest.New(func(t *agenttest.Turn) error {
		if t.Permission(agent.PermissionRequest{ToolName: "bash", Command: "ls"}).Decision != agent.DecisionAllow {
			t.Text("Not allowed.")
			return nil
		}
		t.ToolCall(agent.ToolShell, "bash", `{"command":"ls"}`, "main.go")
		t.Text("There is one file.")
		return nil
	})
	ctx := context.Background()
	sess, _ := p.Start(ctx, agent.Config{Handlers: agent.Handlers{
		Permission: func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		},
	}})
	defer sess.Close()
	res, _ := sess.Run(ctx, agent.Text("What files are here?"))
	fmt.Println(res.Status, res.Text)
	// Output: completed There is one file.
}

func TestEventsAndTurnEvents(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		t.Reasoning("thinking")
		t.Text("hello ")
		t.Text("world")
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
	if _, ok := (<-sess.Events()).(*agent.SessionStarted); !ok {
		t.Fatal("first event is not SessionStarted")
	}
	turn, err := sess.Send(ctx, agent.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var completed bool
	for ev := range turn.Events() {
		switch ev := ev.(type) {
		case *agent.TextDelta:
			text += ev.Delta
		case *agent.TurnCompleted:
			completed = ev.Result.Status == agent.TurnSucceeded
		}
	}
	if text != "hello world" || !completed {
		t.Errorf("turn events text %q, completed %v", text, completed)
	}
	if got := p.Sessions()[0].Inputs(); len(got) != 1 || got[0].PlainText() != "hi" {
		t.Errorf("inputs %+v", got)
	}
}

func TestPermissionThroughEventsAndFailure(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		if t.Permission(agent.PermissionRequest{ToolName: "edit"}).Decision != agent.DecisionAllow {
			return errors.New("denied")
		}
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() {
		for ev := range sess.Events() {
			if req, ok := ev.(*agent.PermissionRequest); ok {
				_ = req.Deny(ctx, "no")
			}
		}
	}()
	res, err := sess.Run(ctx, agent.Text("edit it"))
	if res == nil || res.Status != agent.TurnFailed || err == nil || err != res.Err {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestInterruptQueueAndClose(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		if t.Input().PlainText() == "slow" {
			<-t.Context().Done()
		}
		t.Text("done " + t.Input().PlainText())
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	slow, err := sess.Send(ctx, agent.Text("slow"))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := sess.Send(ctx, agent.Text("next"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if res, err := slow.Wait(wctx); !errors.Is(err, agent.ErrInterrupted) || res.Status != agent.TurnInterrupted {
		t.Errorf("interrupted turn %+v, %v", res, err)
	}
	if res, err := queued.Wait(wctx); err != nil || res.Text != "done next" {
		t.Errorf("queued turn %+v, %v", res, err)
	}
	pending, err := sess.Send(ctx, agent.Text("slow"))
	if err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if _, err := pending.Wait(wctx); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("turn after Close: %v", err)
	}
	if _, err := sess.Send(ctx, agent.Text("x")); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Send after Close: %v", err)
	}
}

func TestInvalidAnswersAreRefused(t *testing.T) {
	var got agent.PermissionResponse
	p := agenttest.New(func(t *agenttest.Turn) error {
		got = t.Permission(agent.PermissionRequest{ToolName: "edit"})
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{Handlers: agent.Handlers{
		Permission: func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Run(ctx, agent.Text("edit it")); err != nil {
		t.Fatal(err)
	}
	if got.Decision != agent.DecisionDeny {
		t.Errorf("zero handler answer gave %+v, want a denial", got)
	}

	direct, err := agenttest.New(func(t *agenttest.Turn) error {
		t.Permission(agent.PermissionRequest{ToolName: "edit"})
		return nil
	}).Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	turn, err := direct.Send(ctx, agent.Text("edit it"))
	if err != nil {
		t.Fatal(err)
	}
	for ev := range turn.Events() {
		if req, ok := ev.(*agent.PermissionRequest); ok {
			if err := req.Respond(ctx, agent.PermissionResponse{Decision: "yes"}); !errors.Is(err, agent.ErrInvalidAnswer) {
				t.Errorf("Respond with an unknown decision = %v", err)
			}
			if req.Outcome() != agent.RequestPending {
				t.Errorf("request answered by an invalid response: %s", req.Outcome())
			}
			if err := req.Allow(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestUsageAddsAllFields(t *testing.T) {
	p := agenttest.New(func(t *agenttest.Turn) error {
		t.Usage(agent.Usage{InputTokens: 1, CacheReadTokens: 2, ReasoningTokens: 3, ContextWindow: 100})
		t.Usage(agent.Usage{InputTokens: 4, CacheWriteTokens: 5, ContextTokens: 6})
		return nil
	})
	ctx := context.Background()
	sess, err := p.Start(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	res, err := sess.Run(ctx, agent.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	want := agent.Usage{InputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 5, ReasoningTokens: 3, ContextWindow: 100, ContextTokens: 6}
	if res.Usage != want {
		t.Errorf("usage %+v, want %+v", res.Usage, want)
	}
}
