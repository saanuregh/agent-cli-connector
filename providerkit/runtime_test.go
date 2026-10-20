package providerkit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

func next(t *testing.T, ch <-chan agent.Event) agent.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

func TestStartupEventsFollowSessionStarted(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.SetSessionID("s1")
	rt.Emit(&agent.Notice{Message: "early"})
	rt.Started(&agent.SessionStarted{})
	rt.Emit(&agent.Notice{Message: "late"})
	ch := rt.Events()
	defer finishAndDrain(rt)
	if _, ok := next(t, ch).(*agent.SessionStarted); !ok {
		t.Fatal("first event is not SessionStarted")
	}
	if n := next(t, ch).(*agent.Notice); n.Message != "early" || n.SessionID != "s1" || n.Provider != "test" || n.Time.IsZero() {
		t.Errorf("early notice = %+v", n)
	}
	if n := next(t, ch).(*agent.Notice); n.Message != "late" {
		t.Errorf("late notice = %+v", n)
	}
}

func TestStatusDedupAndTurnStamping(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	turn, err := rt.NewTurn("t1")
	if err != nil {
		t.Fatal(err)
	}
	rt.Emit(&agent.StatusChanged{Status: agent.StatusRunning})
	rt.Emit(&agent.StatusChanged{Status: agent.StatusRunning})
	rt.Emit(&agent.ItemCompleted{Item: agent.Item{ID: "i1", Kind: agent.ItemAssistantMessage, Text: "hi"}})
	rt.FinishTurn("", &agent.TurnResult{Status: agent.TurnSucceeded})
	rt.Emit(&agent.StatusChanged{Status: agent.StatusIdle})

	ch := rt.Events()
	defer finishAndDrain(rt)
	next(t, ch)
	if s := next(t, ch).(*agent.StatusChanged); s.TurnID != "t1" {
		t.Errorf("status turn = %q", s.TurnID)
	}
	if _, ok := next(t, ch).(*agent.ItemCompleted); !ok {
		t.Fatal("duplicate status was not dropped")
	}
	done := next(t, ch).(*agent.TurnCompleted)
	if done.Result.TurnID != "t1" || len(done.Result.Items) != 1 {
		t.Errorf("turn completed = %+v", done.Result)
	}
	res, err := turn.Wait(context.Background())
	if err != nil || res != done.Result {
		t.Errorf("turn wait = %v, %v", res, err)
	}
	if rt.Busy() {
		t.Error("runtime still busy")
	}
}

func TestRequestHandlerAndResolution(t *testing.T) {
	rt := NewRuntime("test", agent.Config{Handlers: agent.Handlers{
		Permission: func(ctx context.Context, req *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "no"}
		},
	}})
	rt.Started(&agent.SessionStarted{})
	got := make(chan agent.PermissionResponse, 1)
	req := NewPermissionRequest(agent.PermissionRequest{ID: "r1"}, func(ctx context.Context, resp agent.PermissionResponse) error {
		got <- resp
		return nil
	})
	rt.Request(req)
	if resp := <-got; resp.Decision != agent.DecisionDeny || resp.Message != "no" {
		t.Errorf("response = %+v", resp)
	}
	ch := rt.Events()
	defer finishAndDrain(rt)
	next(t, ch)
	resolved := next(t, ch).(*agent.RequestResolved)
	if resolved.Outcome != agent.RequestAnswered || resolved.Request != req {
		t.Errorf("resolved = %+v", resolved)
	}
	if err := req.Allow(context.Background()); !errors.Is(err, agent.ErrRequestDone) {
		t.Errorf("second answer = %v", err)
	}
	if len(rt.PendingRequests()) != 0 {
		t.Error("request still pending")
	}
}

func TestRequestEventWithdrawnOnFinish(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	req := NewUserInputRequest(agent.UserInputRequest{ID: "q1"}, func(context.Context, agent.UserInputResponse) error { return nil })
	rt.Request(req)
	turn, _ := rt.NewTurn("t1")
	ch := rt.Events()
	next(t, ch)
	if ev := next(t, ch); ev != agent.Event(req) {
		t.Fatalf("request event = %T", ev)
	}
	if got := rt.PendingRequests(); len(got) != 1 {
		t.Fatalf("pending = %d", len(got))
	}
	boom := errors.New("boom")
	rt.Finish(boom)
	select {
	case <-req.Done():
	case <-time.After(time.Second):
		t.Fatal("request not withdrawn")
	}
	if req.Outcome() != agent.RequestWithdrawn {
		t.Errorf("outcome = %q", req.Outcome())
	}
	if _, err := turn.Wait(context.Background()); !errors.Is(err, boom) {
		t.Errorf("turn error = %v", err)
	}
	for range ch {
	}
	if !errors.Is(rt.ClosedErr(), agent.ErrClosed) || !errors.Is(rt.ClosedErr(), boom) {
		t.Errorf("closed err = %v", rt.ClosedErr())
	}
}

func TestQueueLimitReportsDrops(t *testing.T) {
	rt := NewRuntime("test", agent.Config{EventBufferLimit: 3})
	rt.Started(&agent.SessionStarted{})
	for range 10 {
		rt.Emit(&agent.Notice{Message: "n"})
	}
	ch := rt.Events()
	defer finishAndDrain(rt)
	dropped := next(t, ch).(*agent.EventsDropped)
	if dropped.Count != 8 {
		t.Errorf("dropped = %d, want 8", dropped.Count)
	}
	if _, ok := next(t, ch).(*agent.SessionStarted); !ok {
		t.Fatal("SessionStarted was dropped")
	}
	for range 2 {
		if _, ok := next(t, ch).(*agent.Notice); !ok {
			t.Fatal("expected notice")
		}
	}
}

// Reporting drops reads the session ID, which takes the runtime lock that
// emitters hold while they push; it must not do so under the queue lock.
func TestQueueDropsUnderLoadDoNotDeadlock(t *testing.T) {
	rt := NewRuntime("test", agent.Config{EventBufferLimit: 1})
	rt.Started(&agent.SessionStarted{})
	const emitters, perEmitter = 4, 2000
	received := make(chan int)
	go func() {
		notices := 0
		for ev := range rt.Events() {
			switch ev := ev.(type) {
			case *agent.Notice:
				notices++
			case *agent.EventsDropped:
				notices += ev.Count
			}
		}
		received <- notices
	}()
	var wg sync.WaitGroup
	for range emitters {
		wg.Go(func() {
			for range perEmitter {
				rt.Emit(&agent.Notice{Message: "n"})
			}
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		rt.Finish(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("emitters and Finish deadlocked with the event reader")
	}
	select {
	case n := <-received:
		if n != emitters*perEmitter {
			t.Errorf("delivered plus dropped = %d, want %d", n, emitters*perEmitter)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("event channel never closed")
	}
}

func TestAcceptedRenamesTurn(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	turn, ok, err := rt.StartIdle(context.Background(), "provisional", func(context.Context) error {
		rt.Accepted("provisional", "real")
		return nil
	})
	if !ok || err != nil || turn.ID() != "real" {
		t.Fatalf("StartIdle = %v, %v, %v", turn, ok, err)
	}
	if id, starting := rt.Running(); id != "real" || starting {
		t.Errorf("Running = %q, %v", id, starting)
	}
	rt.FinishTurn("real", &agent.TurnResult{})
	select {
	case <-turn.Done():
	default:
		t.Error("renamed turn did not complete")
	}
	if !rt.Idle() {
		t.Error("slot still taken")
	}
}

func TestWithdrawBeforeRegister(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	rt.WithdrawRequest("late")
	req := NewPermissionRequest(agent.PermissionRequest{ID: "late"}, func(context.Context, agent.PermissionResponse) error { return nil })
	rt.Request(req)
	select {
	case <-req.Done():
	case <-time.After(time.Second):
		t.Fatal("request withdrawn early was registered")
	}
	if len(rt.PendingRequests()) != 0 {
		t.Error("withdrawn request is pending")
	}
}

func TestLimitNeverDropsRequests(t *testing.T) {
	rt := NewRuntime("test", agent.Config{EventBufferLimit: 2})
	rt.Started(&agent.SessionStarted{})
	req := NewUserInputRequest(agent.UserInputRequest{ID: "q"}, func(context.Context, agent.UserInputResponse) error { return nil })
	rt.Request(req)
	for range 5 {
		rt.Emit(&agent.Notice{})
	}
	ch := rt.Events()
	defer finishAndDrain(rt)
	seen := false
	for range 4 {
		if next(t, ch) == agent.Event(req) {
			seen = true
		}
	}
	if !seen {
		t.Error("request was dropped")
	}
}

func TestItemsRemovedLeaveTurnResult(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	turn, _ := rt.NewTurn("t1")
	rt.Emit(&agent.ItemCompleted{Item: agent.Item{ID: "a", Kind: agent.ItemAssistantMessage}})
	rt.Emit(&agent.ItemCompleted{Item: agent.Item{ID: "b", Kind: agent.ItemAssistantMessage}})
	rt.Emit(&agent.ItemsRemoved{IDs: []string{"a"}})
	rt.FinishTurn("t1", &agent.TurnResult{Status: agent.TurnSucceeded})
	res, err := turn.Wait(context.Background())
	if err != nil || len(res.Items) != 1 || res.Items[0].ID != "b" {
		t.Errorf("items %+v, %v", res, err)
	}
}

func TestNewTurnForEndedTurnReturnsEndedHandle(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	first, _ := rt.NewTurn("t1")
	rt.FinishTurn("t1", &agent.TurnResult{Status: agent.TurnSucceeded, Text: "done"})
	again, err := rt.NewTurn("t1")
	if err != nil || again != first {
		t.Fatalf("NewTurn after finish = %p, %v; want the ended handle %p", again, err, first)
	}
	if rt.Busy() || len(rt.TurnIDs()) != 0 {
		t.Errorf("ended turn tracked again: %v", rt.TurnIDs())
	}
	res, err := again.Wait(context.Background())
	if err != nil || res.Text != "done" {
		t.Errorf("result %+v, %v", res, err)
	}
}

func TestTurnEventsReplayAndFollow(t *testing.T) { synctest.Test(t, testTurnEventsReplayAndFollow) }

func testTurnEventsReplayAndFollow(t *testing.T) {
	rt := NewRuntime("test", agent.Config{})
	rt.Started(&agent.SessionStarted{})
	turn, err := rt.NewTurn("t1")
	if err != nil {
		t.Fatal(err)
	}
	rt.Emit(&agent.TextDelta{EventMeta: agent.EventMeta{TurnID: "t1"}, Delta: "a"})
	rt.Emit(&agent.StatusChanged{Status: agent.StatusRunning}) // stamped with the running turn
	rt.Emit(&agent.TextDelta{EventMeta: agent.EventMeta{TurnID: "other"}, Delta: "x"})

	got := make(chan []string, 1)
	go func() {
		var names []string
		for ev := range turn.Events() {
			switch ev := ev.(type) {
			case *agent.TextDelta:
				names = append(names, "text:"+ev.Delta)
			case *agent.TurnCompleted:
				names = append(names, "completed")
			default:
				names = append(names, fmt.Sprintf("%T", ev))
			}
		}
		got <- names
	}()
	synctest.Wait() // the reader has replayed and waits for more
	rt.Emit(&agent.TextDelta{EventMeta: agent.EventMeta{TurnID: "t1"}, Delta: "b"})
	rt.FinishTurn("t1", &agent.TurnResult{Status: agent.TurnSucceeded})
	select {
	case names := <-got:
		want := []string{"text:a", "*agent.StatusChanged", "text:b", "completed"}
		if !slices.Equal(names, want) {
			t.Errorf("turn events %v, want %v", names, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Events did not end after the turn completed")
	}
	// Ranging again replays everything; breaking early stops.
	n := 0
	for range turn.Events() {
		n++
		break
	}
	if n != 1 {
		t.Errorf("second range yielded %d events before break", n)
	}
}

// finishAndDrain ends rt and reads its events until the channel closes, so
// the delivery goroutine exits.
func finishAndDrain(rt *Runtime) {
	rt.Finish(nil)
	for range rt.Events() {
	}
}
