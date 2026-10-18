//go:build live

// Package livedump prints the uniform event stream of one real turn per
// provider, for checking the event mapping by eye:
//
//	go test -tags live ./internal/livedump -run Dump -v -provider claude
package livedump

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
	"github.com/saanuregh/agent-cli-connector/pi"
)

var (
	providerFlag = flag.String("provider", "claude", "provider")
	modelFlag    = flag.String("model", "", "model")
	promptFlag   = flag.String("prompt", "Use the add tool to add 2 and 3, then run `echo hi` in the shell, then reply with the sum.", "prompt")
)

func describe(ev agent.Event) string {
	m := ev.Meta()
	turn := m.TurnID
	if len(turn) > 8 {
		turn = turn[:8]
	}
	var detail string
	switch e := ev.(type) {
	case *agent.TextDelta:
		detail = fmt.Sprintf("item=%s parent=%s %q", e.ItemID, e.ParentID, e.Delta)
	case *agent.ReasoningDelta:
		detail = fmt.Sprintf("item=%s summary=%v %q", e.ItemID, e.Summary, e.Delta)
	case *agent.ToolInputDelta:
		detail = fmt.Sprintf("item=%s %q", e.ItemID, e.Delta)
	case *agent.ToolOutputDelta:
		detail = fmt.Sprintf("item=%s %q", e.ItemID, e.Delta)
	case *agent.ItemStarted:
		detail = item(e.Item)
	case *agent.ItemUpdated:
		detail = item(e.Item)
	case *agent.ItemCompleted:
		detail = item(e.Item)
	case *agent.TurnCompleted:
		r := e.Result
		detail = fmt.Sprintf("status=%s text=%q items=%d usage=%+v err=%v", r.Status, trunc(r.Text), len(r.Items), r.Usage, r.Err)
	case *agent.PermissionRequest:
		detail = fmt.Sprintf("id=%s kind=%s tool=%s cmd=%q decisions=%v", e.ID, e.Kind, e.ToolName, e.Command, e.Decisions)
	case *agent.RequestResolved:
		detail = fmt.Sprintf("id=%s outcome=%s", e.Request.RequestID(), e.Outcome)
	case *agent.StatusChanged:
		detail = string(e.Status)
	case *agent.UsageUpdated:
		detail = fmt.Sprintf("%+v", e.Usage)
	case *agent.SessionStarted:
		detail = fmt.Sprintf("model=%s id=%s", e.Info.Model, e.Info.ID)
	case *agent.SessionUpdated:
		detail = fmt.Sprintf("model=%s mode=%s effort=%s tools=%d", e.Info.Model, e.Info.PermissionMode, e.Info.Effort, len(e.Info.Tools))
	case *agent.Notice:
		detail = fmt.Sprintf("%s %s %s", e.Level, e.Code, trunc(e.Message))
	case *agent.RawEvent:
		detail = e.Type
	case *agent.PlanUpdated:
		detail = fmt.Sprintf("%+v", e.Steps)
	case *agent.TaskUpdated:
		detail = fmt.Sprintf("%+v", e.Task)
	}
	return fmt.Sprintf("[%s] %-24T %s", turn, ev, detail)
}

func item(it agent.Item) string {
	s := fmt.Sprintf("id=%s kind=%s status=%s", it.ID, it.Kind, it.Status)
	if it.ParentID != "" {
		s += " parent=" + it.ParentID
	}
	if it.Text != "" {
		s += fmt.Sprintf(" text=%q", trunc(it.Text))
	}
	if t := it.Tool; t != nil {
		s += fmt.Sprintf(" tool=%s/%s input=%s cmd=%q out=%q err=%v", t.Name, t.Kind, trunc(string(t.Input)), t.Command, trunc(t.Output), t.IsError)
	}
	return s
}

func trunc(s string) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}

func TestDump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	reg := agent.NewRegistry(claude.New(claude.Options{}), codex.New(codex.Options{}), pi.New(pi.Options{}))
	p, ok := reg.Get(*providerFlag)
	if !ok {
		t.Fatalf("unknown provider %q", *providerFlag)
	}
	store := livetest.New(t, p, t.TempDir())
	cfg := agent.Config{
		Env:            livetest.PiEnv(t),
		Model:          *modelFlag,
		Effort:         agent.EffortLow,
		Ephemeral:      true,
		PermissionMode: agent.PermissionDefault,
		Tools: []agent.Tool{{
			Name:        "add",
			Description: "Adds two integers.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
			Handler: func(ctx context.Context, call agent.ToolCallRequest) (agent.ToolResult, error) {
				var in struct{ A, B int }
				_ = json.Unmarshal(call.Arguments, &in)
				return agent.TextResult(fmt.Sprint(in.A + in.B)), nil
			},
		}},
	}
	cfg.Cwd = store.Cwd()
	sess, err := reg.Start(ctx, *providerFlag, cfg)
	if err != nil {
		t.Fatal(err)
	}
	store.Track(sess)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sess.Events() {
			t.Log(describe(ev))
			if p, ok := ev.(*agent.PermissionRequest); ok {
				_ = p.Allow(ctx)
			}
		}
	}()
	if _, err := sess.Run(ctx, agent.Text(*promptFlag)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	sess.Close()
	<-done
}
