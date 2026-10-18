package providerkit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// startLog records the turns a Starter started, in order.
type startLog struct {
	mu      sync.Mutex
	started []string
	ch      chan string
}

func newStartLog() *startLog { return &startLog{ch: make(chan string, 16)} }

func (l *startLog) add(id string) {
	l.mu.Lock()
	l.started = append(l.started, id)
	l.mu.Unlock()
	l.ch <- id
}

func (l *startLog) next(t *testing.T) string {
	t.Helper()
	select {
	case id := <-l.ch:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("no start")
		return ""
	}
}

func (l *startLog) none(t *testing.T) {
	t.Helper()
	select {
	case id := <-l.ch:
		t.Fatalf("unexpected start of %s", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestQueueStartsInOrderWhenSlotFrees(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	log := newStartLog()
	rt.SetStarter(func(_ context.Context, id string, _ agent.Input) error { log.add(id); return nil })
	if _, ok, err := rt.StartIdle(context.Background(), "a", func(context.Context) error { return nil }); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, ok, _ := rt.StartIdle(context.Background(), "x", func(context.Context) error { return nil }); ok {
		t.Fatal("StartIdle started while a turn runs")
	}
	rt.Enqueue("b", agent.Text("b"))
	rt.Enqueue("c", agent.Text("c"))
	log.none(t)
	rt.FinishTurn("a", &agent.TurnResult{})
	if id := log.next(t); id != "b" {
		t.Fatalf("started %s, want b", id)
	}
	log.none(t)
	rt.FinishTurn("b", &agent.TurnResult{})
	if id := log.next(t); id != "c" {
		t.Fatalf("started %s, want c", id)
	}
}

func TestEnqueueWhenIdleStarts(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	log := newStartLog()
	rt.SetStarter(func(_ context.Context, id string, _ agent.Input) error { log.add(id); return nil })
	rt.Enqueue("a", agent.Text("a"))
	if id := log.next(t); id != "a" {
		t.Fatalf("started %s", id)
	}
}

func TestFailedStartAbandonsAndMovesOn(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	log := newStartLog()
	boom := errors.New("boom")
	rt.SetStarter(func(_ context.Context, id string, _ agent.Input) error {
		log.add(id)
		if id == "a" {
			return boom
		}
		return nil
	})
	a, _ := rt.Enqueue("a", agent.Text("a"))
	b, _ := rt.Enqueue("b", agent.Text("b"))
	log.next(t)
	if id := log.next(t); id != "b" {
		t.Fatalf("started %s, want b", id)
	}
	if _, err := a.Wait(context.Background()); !errors.Is(err, boom) {
		t.Errorf("failed turn error = %v", err)
	}
	if id, _ := rt.Running(); id != "b" {
		t.Errorf("running %q", id)
	}
	select {
	case <-b.Done():
		t.Error("started turn ended")
	default:
	}
}

func TestStartThatEndsTurnMovesOn(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	log := newStartLog()
	rt.SetStarter(func(_ context.Context, id string, _ agent.Input) error {
		log.add(id)
		if id == "a" {
			rt.FinishTurn(id, &agent.TurnResult{StopReason: "handled"})
		}
		return nil
	})
	rt.Enqueue("a", agent.Text("a"))
	rt.Enqueue("b", agent.Text("b"))
	log.next(t)
	if id := log.next(t); id != "b" {
		t.Fatalf("started %s, want b", id)
	}
}

func TestSteerGateHoldsQueueAndAdopts(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	log := newStartLog()
	rt.SetStarter(func(_ context.Context, id string, _ agent.Input) error { log.add(id); return nil })
	rt.StartIdle(context.Background(), "a", func(context.Context) error { return nil })
	rt.NewTurn("s")
	if !rt.BeginSteer("s", "a") {
		t.Fatal("steer into the running turn refused")
	}
	rt.Enqueue("b", agent.Text("b"))
	rt.FinishTurn("a", &agent.TurnResult{})
	log.none(t)
	if id, _ := rt.RunStarted(""); id != "s" {
		t.Fatalf("run adopted as %q, want s", id)
	}
	rt.FinishTurn("s", &agent.TurnResult{})
	if id := log.next(t); id != "b" {
		t.Fatalf("started %s", id)
	}
	// The starter has run; the pump finishes the start just after.
	waitStarted(t, rt, "b")

	if rt.BeginSteer("s2", "a") {
		t.Fatal("steer into a finished turn accepted")
	}
	if !rt.BeginSteer("s2", "b") {
		t.Fatal("steer into the running turn refused")
	}
	rt.Enqueue("c", agent.Text("c"))
	rt.FinishTurn("b", &agent.TurnResult{})
	log.none(t)
	rt.EndSteer("s2")
	if id := log.next(t); id != "c" {
		t.Fatalf("started %s", id)
	}
}

func TestSteerRefusedWhileStarting(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	release := make(chan struct{})
	go rt.StartIdle(context.Background(), "a", func(context.Context) error { <-release; return nil })
	for {
		if id, starting := rt.Running(); id == "a" && starting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if rt.BeginSteer("s", "a") {
		t.Error("steer accepted while the turn starts")
	}
	close(release)
}

func TestInterruptDuringStartIsReportedByAccepted(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	var interrupt bool
	rt.StartIdle(context.Background(), "p", func(context.Context) error {
		if id, starting := rt.Interrupt(""); id != "p" || !starting {
			t.Errorf("Interrupt = %q, %v", id, starting)
		}
		if id, _ := rt.Interrupt("other"); id != "" {
			t.Errorf("Interrupt(other) = %q", id)
		}
		interrupt = rt.Accepted("p", "n")
		return nil
	})
	if !interrupt {
		t.Error("Accepted did not report the interrupt")
	}
	if id, pending := rt.Interrupting(); id != "n" || pending {
		t.Errorf("Interrupting = %q, %v", id, pending)
	}
	rt.FinishTurn("n", &agent.TurnResult{})
	if id, _ := rt.Interrupting(); id != "" {
		t.Errorf("interrupt outlived its turn: %q", id)
	}
}

func TestClearQueueCancels(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.SetStarter(func(context.Context, string, agent.Input) error { return nil })
	rt.StartIdle(context.Background(), "a", func(context.Context) error { return nil })
	b, _ := rt.Enqueue("b", agent.Text("b"))
	if in := rt.ClearQueue(); len(in) != 1 || in[0].PlainText() != "b" {
		t.Fatalf("cleared %+v", in)
	}
	if _, err := b.Wait(context.Background()); !errors.Is(err, agent.ErrCancelled) {
		t.Errorf("cleared turn error = %v", err)
	}
}

// A harness that reports a run while its start is in flight confirms the
// start: the turn takes the harness ID and counts as running at once.
func TestRunStartedDuringStartAcceptsIt(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	var turn *agent.Turn
	turn, _, _ = rt.StartIdle(context.Background(), "p", func(context.Context) error {
		rt.Interrupt("")
		if id, starting := rt.RunStarted("n"); id != "n" || starting {
			t.Errorf("RunStarted = %q, %v", id, starting)
		}
		if id, pending := rt.Interrupting(); id != "n" || !pending {
			t.Errorf("Interrupting = %q, %v", id, pending)
		}
		if !rt.Accepted("p", "n") {
			t.Error("Accepted lost the interrupt asked for during the start")
		}
		return nil
	})
	if turn.ID() != "n" || !rt.HasTurn("n") || rt.HasTurn("p") {
		t.Errorf("turn %q; tracked %v", turn.ID(), rt.TurnIDs())
	}
}

// An interrupt naming a turn by the ID it had before the harness renamed it
// still reaches that turn.
func TestInterruptByPreRenameID(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _, _ = rt.StartIdle(context.Background(), "prov", func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	before, _ := rt.Running() // the caller reads the slot before the rename
	rt.Accepted("prov", "native")
	if id, _ := rt.Interrupt(before); id != "native" {
		t.Errorf("Interrupt(%q) = %q, want native", before, id)
	}
	close(release)
}

// waitStarted waits until turn id holds the slot with its start done.
func waitStarted(t *testing.T, rt *Runtime, id string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		if running, starting := rt.Running(); running == id && !starting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never finished starting", id)
		}
	}
}
