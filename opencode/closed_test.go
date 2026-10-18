package opencode_test

import (
	"context"
	"errors"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/sessiontest"

	agent "github.com/saanuregh/agent-cli-connector"
)

func TestCallsAfterCloseReturnErrClosed(t *testing.T) {
	f := newFakeServer(t)
	sess, _ := startFake(t, f, agent.Config{})
	ctx := context.Background()
	_ = sess.Close()
	for name, err := range map[string]error{
		"SetModel":          sess.SetModel(ctx, "opencode/test-model"),
		"SetEffort":         sess.SetEffort(ctx, agent.EffortLow),
		"SetPermissionMode": sess.SetPermissionMode(ctx, agent.PermissionPlan),
		"SetTitle":          sess.SetTitle(ctx, "t"),
		"Inbox":             second(sess.Inbox(ctx)),
		"Diff":              second(sess.Diff(ctx)),
		"Export":            second(sess.Export(ctx)),
		"Messages":          second(sess.Messages(ctx)),
		"SideQuestion":      second(sess.SideQuestion(ctx, "q")),
		"Agents":            second(sess.Agents(ctx)),
		"PermissionRules":   second(sess.PermissionRules(ctx)),
		"Shell":             sess.Shell(ctx, "ls"),
		"Synthetic":         sess.Synthetic(ctx, "x", "d"),
		"SetInstructions":   sess.SetInstructions(ctx, "k", "v"),
		"CancelInbox":       sess.CancelInbox(ctx, "i"),
		"Compact":           sess.Compact(ctx, ""),
		"Rewind":            sess.Rewind(ctx, "msg_1"),
	} {
		if !errors.Is(err, agent.ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestSessionLifecycleContract(t *testing.T) {
	f := newFakeServer(t)
	sess, err := f.provider().StartSession(context.Background(), agent.Config{Cwd: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	sessiontest.Lifecycle(t, sess)
}
