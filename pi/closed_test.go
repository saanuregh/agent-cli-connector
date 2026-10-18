package pi_test

import (
	"context"
	"errors"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/sessiontest"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func TestCallsAfterCloseReturnErrClosed(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		h.WaitClosed()
	})
	ctx := context.Background()
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(ctx, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()
	for name, err := range map[string]error{
		"SetModel":  sess.SetModel(ctx, "p/m"),
		"SetEffort": sess.SetEffort(ctx, agent.EffortLow),
		"SetTitle":  sess.SetTitle(ctx, "t"),
		"State":     second(sess.State(ctx)),
		"Stats":     second(sess.Stats(ctx)),
		"Messages":  second(sess.Messages(ctx)),
		"Bash":      second(sess.Bash(ctx, "ls", false)),
		"Compact":   sess.Compact(ctx, ""),
		"FollowUp":  sess.FollowUp(ctx, "x"),
	} {
		if !errors.Is(err, agent.ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
}

func second[T any](_ T, err error) error { return err }

func TestSessionLifecycleContract(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !startup(h) {
			return
		}
		h.WaitClosed()
	})
	sess, err := pi.New(pi.Options{Spawner: spawner, DisableBridge: true}).StartSession(context.Background(), agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sessiontest.Lifecycle(t, sess)
}
