package claude_test

import (
	"context"
	"errors"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/sessiontest"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

func TestCallsAfterCloseReturnErrClosed(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()
	for name, err := range map[string]error{
		"SetModel":          sess.SetModel(ctx, "m"),
		"SetEffort":         sess.SetEffort(ctx, agent.EffortLow),
		"SetPermissionMode": sess.SetPermissionMode(ctx, agent.PermissionPlan),
		"SetTitle":          sess.SetTitle(ctx, "t"),
		"Interrupt":         sess.Interrupt(ctx),
		"ContextUsage":      second(sess.ContextUsage(ctx, false)),
		"Settings":          second(sess.Settings(ctx)),
		"Status":            second(sess.Status(ctx)),
		"SideQuestion":      second(sess.SideQuestion(ctx, "q")),
		"MCPServerStatus":   second(sess.MCPServerStatus(ctx)),
		"Compact":           sess.Compact(ctx, ""),
		"SendMessage":       sess.SendMessage(map[string]any{"type": "user"}),
	} {
		if name == "Interrupt" {
			continue // an idle Interrupt sends nothing
		}
		if !errors.Is(err, agent.ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestSessionLifecycleContract(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if handshake(h) == nil {
			return
		}
		h.WaitClosed()
	})
	sess, err := claude.New(claude.Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sessiontest.Lifecycle(t, sess)
}
