package codex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/sessiontest"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

func TestCallsAfterCloseReturnErrClosed(t *testing.T) {
	for _, shared := range []bool{false, true} {
		spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
			if startThread(t, h) == nil {
				return
			}
			for {
				m := h.Recv()
				if m == nil {
					return
				}
				if m["id"] != nil && m["method"] != nil {
					h.Reply(m, map[string]any{})
				}
			}
		})
		ctx := context.Background()
		p := codex.New(codex.Options{Spawner: spawner, SharedServer: shared})
		sess, err := p.StartSession(ctx, agent.Config{})
		if err != nil {
			t.Fatal(err)
		}
		_ = sess.Close()
		for name, err := range map[string]error{
			"SetModel":          sess.SetModel(ctx, "m"),
			"SetEffort":         sess.SetEffort(ctx, agent.EffortLow),
			"SetPermissionMode": sess.SetPermissionMode(ctx, agent.PermissionPlan),
			"SetTitle":          sess.SetTitle(ctx, "t"),
			"ReadThread":        second(sess.ReadThread(ctx, false)),
			"Goal":              second(sess.Goal(ctx)),
			"ShellCommand":      sess.ShellCommand(ctx, "ls", 0),
			"Compact":           sess.Compact(ctx, ""),
			"Archive":           sess.Archive(ctx),
			"Call":              sess.Call(ctx, "thread/read", nil, nil),
		} {
			if !errors.Is(err, agent.ErrClosed) {
				t.Errorf("shared=%v: %s after Close = %v, want ErrClosed", shared, name, err)
			}
		}
		_ = p.Close()
	}
}

func second[T any](_ T, err error) error { return err }

func TestSessionLifecycleContract(t *testing.T) {
	for _, shared := range []bool{false, true} {
		spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
			if startThread(t, h) == nil {
				return
			}
			for {
				m := h.Recv()
				if m == nil {
					return
				}
				if m["id"] != nil && m["method"] != nil {
					h.Reply(m, map[string]any{})
				}
			}
		})
		p := codex.New(codex.Options{Spawner: spawner, SharedServer: shared})
		sess, err := p.StartSession(context.Background(), agent.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sessiontest.Lifecycle(t, sess)
		_ = p.Close()
	}
}
