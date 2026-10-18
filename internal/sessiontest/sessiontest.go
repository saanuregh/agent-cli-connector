// Package sessiontest checks the agent.Session contract shared by every
// provider.
package sessiontest

import (
	"context"
	"errors"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Lifecycle checks the start and Close contract of a freshly started,
// idle session: SessionStarted is the first event, Close is idempotent and
// clean, and Done and Events close afterwards.
func Lifecycle(t *testing.T, sess agent.Session) {
	t.Helper()
	if sess.ID() == "" {
		t.Error("ID is empty after Start")
	}
	select {
	case ev := <-sess.Events():
		if _, ok := ev.(*agent.SessionStarted); !ok {
			t.Errorf("first event = %T, want *agent.SessionStarted", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first event")
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after Close")
	}
	if err := sess.Err(); err != nil {
		t.Errorf("Err after a clean Close = %v", err)
	}
	if _, err := sess.Send(context.Background(), agent.Text("x")); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Send after Close = %v, want ErrClosed", err)
	}
	if _, err := sess.Run(context.Background(), agent.Text("x")); !errors.Is(err, agent.ErrClosed) {
		t.Errorf("Run after Close = %v, want ErrClosed", err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sess.Events():
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("Events not closed after Close")
		}
	}
}
