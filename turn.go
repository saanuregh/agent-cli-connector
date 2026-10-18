package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"
)

// TurnStatus is the outcome of a turn.
type TurnStatus string

// Turn outcomes.
const (
	TurnSucceeded   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
)

// TurnResult describes a finished turn.
type TurnResult struct {
	TurnID string
	Status TurnStatus
	// Text is the final assistant message.
	Text string
	// StructuredOutput is the answer matching Input.OutputSchema or
	// Config.OutputSchema.
	StructuredOutput json.RawMessage
	// Usage is the token use of this turn. CostUSD is this turn's cost
	// when the harness reports cost.
	Usage Usage
	// Duration is the turn's wall-clock time.
	Duration time.Duration
	// Steps is the number of model requests the turn made, when known.
	Steps int
	// StopReason is the native reason the turn ended, such as "end_turn",
	// "max_turns", or "aborted".
	StopReason string
	// Err describes a failed or interrupted turn. It is a *TurnError, and
	// for an interrupted turn it matches ErrInterrupted.
	Err error
	// Items are the transcript items completed during the turn, in order.
	Items []Item
	// Origin tells what started the turn when the harness reports it,
	// such as "human" or "task-notification" for a turn a finished
	// background task started.
	Origin string
	// Raw is the native message that ended the turn.
	Raw json.RawMessage
}

// TurnError describes why a turn failed.
type TurnError struct {
	// Code is a harness-specific error class, such as "rate_limit",
	// "error_max_turns", or "contextWindowExceeded".
	Code    string
	Message string
	// HTTPStatus is the failed API response status, when known.
	HTTPStatus int
	// Retryable reports that sending the input again may succeed, such as
	// after a rate limit or overload.
	Retryable bool
	// ResetsAt is when an exhausted usage limit resets; zero when unknown.
	ResetsAt time.Time
	// Err is a sentinel the failure matches through errors.Is, such as
	// ErrSessionNotFound; nil when none applies.
	Err error
}

// Unwrap returns Err.
func (e *TurnError) Unwrap() error { return e.Err }

func (e *TurnError) Error() string {
	prefix := "turn failed"
	if errors.Is(e.Err, ErrInterrupted) {
		prefix = "turn interrupted"
	}
	switch {
	case e.Code == "" && e.Message == "":
		return prefix
	case e.Code == "":
		return prefix + ": " + e.Message
	case e.Message == "":
		return prefix + ": " + e.Code
	}
	return fmt.Sprintf("%s: %s: %s", prefix, e.Code, e.Message)
}

// normalizeTurnResult makes res.Err a *TurnError for a turn that failed or
// was interrupted, and makes an interrupted turn's error match
// ErrInterrupted.
func normalizeTurnResult(res *TurnResult) {
	if res == nil || (res.Status != TurnFailed && res.Status != TurnInterrupted) {
		return
	}
	var te *TurnError
	if !errors.As(res.Err, &te) {
		te = &TurnError{Code: res.StopReason}
		if res.Err != nil {
			te.Message = res.Err.Error()
		}
		res.Err = te
	}
	if res.Status == TurnInterrupted && te.Err == nil {
		te.Err = ErrInterrupted
	}
}

// Turn is a handle to submitted input. It completes when the harness
// reports the turn's outcome.
type Turn struct {
	idMu   sync.Mutex
	id     string
	done   chan struct{}
	once   sync.Once
	result *TurnResult
	err    error

	// events holds the turn's events for Events; notify is closed and
	// replaced when one is added or the turn ends.
	evMu     sync.Mutex
	events   []Event
	notify   chan struct{}
	finished bool
}

// newTurn returns a pending turn and the function that completes it. The
// complete function may be called more than once; only the first call has
// an effect.
func newTurn(id string) (*Turn, func(*TurnResult, error)) {
	t := &Turn{id: id, done: make(chan struct{}), notify: make(chan struct{})}
	return t, t.complete
}

func (t *Turn) complete(res *TurnResult, err error) {
	t.once.Do(func() {
		normalizeTurnResult(res)
		t.result = res
		t.err = err
		close(t.done)
		t.evMu.Lock()
		t.finished = true
		close(t.notify)
		t.evMu.Unlock()
	})
}

// addEvent records an event of the turn for Events.
func (t *Turn) addEvent(ev Event) {
	t.evMu.Lock()
	defer t.evMu.Unlock()
	if t.finished {
		return
	}
	t.events = append(t.events, ev)
	close(t.notify)
	t.notify = make(chan struct{})
}

// Events returns the turn's events: those already delivered since the turn
// was submitted, then each new one as it arrives, ending after the
// TurnCompleted event or when the turn ends without one. Events from
// outside any turn, such as the session starting, are not included, and
// every event still goes to Session.Events as well. Breaking out of the loop
// stops the sequence; it can be ranged over again from the start.
//
//	turn, err := sess.Send(ctx, agent.Text("Explain this repository."))
//	for ev := range turn.Events() {
//		if d, ok := ev.(*agent.TextDelta); ok {
//			fmt.Print(d.Delta)
//		}
//	}
//
// The turn keeps its events until it is no longer referenced.
func (t *Turn) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for i := 0; ; {
			t.evMu.Lock()
			if i < len(t.events) {
				ev := t.events[i]
				i++
				t.evMu.Unlock()
				if !yield(ev) {
					return
				}
				continue
			}
			if t.finished {
				t.evMu.Unlock()
				return
			}
			ch := t.notify
			t.evMu.Unlock()
			<-ch
		}
	}
}

// ID returns the turn identifier. Input queued behind a running turn may
// have a provisional ID until the harness starts its turn and assigns one.
func (t *Turn) ID() string {
	t.idMu.Lock()
	defer t.idMu.Unlock()
	return t.id
}

// setTurnID replaces a turn's provisional ID with the harness's ID.
func setTurnID(t *Turn, id string) {
	t.idMu.Lock()
	t.id = id
	t.idMu.Unlock()
}

// Done is closed when the turn finished.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Wait blocks until the turn finished or ctx ended. A turn that failed or
// was interrupted returns its result together with result.Err, a
// *TurnError; errors.Is(err, ErrInterrupted) tells the two apart. A nil
// result means the turn could not run to an outcome, for example because
// the session closed or ctx ended.
func (t *Turn) Wait(ctx context.Context) (*TurnResult, error) {
	select {
	case <-t.done:
		if t.err == nil && t.result != nil && t.result.Status != TurnSucceeded && t.result.Err != nil {
			return t.result, t.result.Err
		}
		return t.result, t.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
