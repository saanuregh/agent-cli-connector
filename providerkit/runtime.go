// Package providerkit holds the shared machinery for implementing an
// agent.Provider: event delivery, turn tracking, request bookkeeping, and
// harness process management. The claude, codex, pi, opencode, and
// agenttest packages are built on it; use it to add a provider for another
// harness.
package providerkit

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/agentlink"
)

// Runtime is the provider-independent state of one session. A provider
// session owns one Runtime, feeds it native messages translated to events,
// and forwards the matching agent.Session methods to it.
type Runtime struct {
	provider string
	handlers agent.Handlers
	logger   *slog.Logger
	queue    *eventQueue

	mu         sync.Mutex
	sessionID  string
	turns      []*turnState
	pending    []agent.Request
	started    bool
	early      []agent.Event
	lastStatus agent.SessionStatus
	withdrawn  map[string]bool

	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	finished bool
	err      error
	// recent holds the handles of recently ended turns, oldest first, so
	// NewTurn for a turn that ended meanwhile returns the ended handle
	// instead of tracking a turn nothing will finish.
	recent []*agent.Turn

	// The turn queue; see turnqueue.go.
	starter Starter
	inputs  []queuedInput
	// running is the turn holding the running slot: the turn the harness
	// works on, or the turn whose start is in flight. It is "" when the
	// slot is free.
	running string
	// starting lists the turns whose start is in flight.
	starting []string
	// steering is a steer in flight that may start a run of its own;
	// queued input waits until it resolves.
	steering string
	// interrupt is the turn an interrupt was asked for, until it ends;
	// interruptPending marks it asked while the turn's start was in
	// flight, so the interrupt is still to be sent.
	interrupt        string
	interruptPending bool
	// renamedFrom and renamedTo are the latest turn rename by Accepted.
	renamedFrom, renamedTo string
	// pumping is set while a goroutine starts queued input.
	pumping bool
}

// recentTurns bounds Runtime.recent.
const recentTurns = 64

type turnState struct {
	turn     *agent.Turn
	complete func(*agent.TurnResult, error)
	started  time.Time
	items    []agent.Item
}

// NewRuntime returns a runtime for one session.
func NewRuntime(provider string, cfg agent.Config) *Runtime {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{
		provider: provider,
		handlers: cfg.Handlers,
		logger:   logger.With("provider", provider),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	r.queue = newEventQueue(cfg.EventBufferLimit, r.baseMeta)
	return r
}

// Logger returns the session logger. It is never nil.
func (r *Runtime) Logger() *slog.Logger { return r.logger }

// Context returns a context that ends when the session ends.
func (r *Runtime) Context() context.Context { return r.ctx }

// SetSessionID records the native session ID used to stamp events.
func (r *Runtime) SetSessionID(id string) {
	r.mu.Lock()
	r.sessionID = id
	r.mu.Unlock()
}

// SessionID returns the native session ID.
func (r *Runtime) SessionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessionID
}

// Events returns the event channel.
func (r *Runtime) Events() <-chan agent.Event { return r.queue.channel() }

func (r *Runtime) baseMeta() agent.EventMeta {
	return agent.EventMeta{Provider: r.provider, SessionID: r.SessionID()}
}

// Emit stamps ev with the provider, session, time, and current turn where
// they are missing, records completed items for the turn result, and queues
// ev for delivery. Events emitted before Started are held back and
// delivered right after the SessionStarted event. A StatusChanged event
// that repeats the current status is dropped.
func (r *Runtime) Emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.emitLocked(ev)
	}
}

// emitLocked delivers ev; the caller holds r.mu and the runtime is not
// finished.
func (r *Runtime) emitLocked(ev agent.Event) {
	if sc, ok := ev.(*agent.StatusChanged); ok {
		if sc.Status == r.lastStatus {
			return
		}
		r.lastStatus = sc.Status
	}
	r.stampLocked(ev)
	t := r.turnLocked(ev.Meta().TurnID)
	if t != nil {
		addTurnEvent(t.turn, ev)
	}
	switch ev := ev.(type) {
	case *agent.ItemCompleted:
		if t != nil {
			t.items = append(t.items, ev.Item)
		}
	case *agent.ItemsRemoved:
		removed := make(map[string]bool, len(ev.IDs))
		for _, id := range ev.IDs {
			removed[id] = true
		}
		for _, t := range r.turns {
			t.items = slices.DeleteFunc(t.items, func(it agent.Item) bool { return removed[it.ID] })
		}
	}
	r.deliverLocked(ev)
}

// deliverLocked queues ev, or holds it until Started; the caller holds r.mu.
func (r *Runtime) deliverLocked(ev agent.Event) {
	if !r.started {
		r.early = append(r.early, ev)
		return
	}
	// Pushing under r.mu keeps delivery in emit order across goroutines;
	// the queue lock is always taken after r.mu.
	r.queue.push(ev)
}

// Started delivers the SessionStarted event followed by the events emitted
// during startup. Providers call it once the session is ready.
func (r *Runtime) Started(ev *agent.SessionStarted) {
	r.mu.Lock()
	if r.finished || r.started {
		r.mu.Unlock()
		return
	}
	r.stampLocked(ev)
	r.started = true
	r.queue.push(ev)
	for _, e := range r.early {
		r.queue.push(e)
	}
	r.early = nil
	r.mu.Unlock()
}

func (r *Runtime) stampLocked(ev agent.Event) {
	m := ev.Meta()
	if m.Provider == "" {
		m.Provider = r.provider
	}
	if m.SessionID == "" {
		m.SessionID = r.sessionID
	}
	if m.Time.IsZero() {
		m.Time = time.Now()
	}
	if m.TurnID == "" && len(r.turns) > 0 {
		switch ev.(type) {
		case *agent.SessionStarted, *agent.SessionUpdated:
		default:
			// The running turn, else the oldest unfinished one: queued turns
			// produce no events.
			m.TurnID = cmp.Or(r.running, r.turns[0].turn.ID())
		}
	}
}

func (r *Runtime) turnLocked(id string) *turnState {
	for _, t := range r.turns {
		if t.turn.ID() == id {
			return t
		}
	}
	return nil
}

// NewTurn registers a turn the session submitted and returns its handle.
// Turns are tracked in submission order; the oldest unfinished turn is the
// current one.
func (r *Runtime) NewTurn(id string) (*agent.Turn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.newTurnLocked(id)
}

func (r *Runtime) newTurnLocked(id string) (*agent.Turn, error) {
	if r.finished {
		return nil, r.closedErrLocked()
	}
	if t := r.turnLocked(id); t != nil {
		return t.turn, nil
	}
	if i := slices.IndexFunc(r.recent, func(t *agent.Turn) bool { return t.ID() == id }); i >= 0 {
		return r.recent[i], nil
	}
	turn, complete := newTurn(id)
	r.turns = append(r.turns, &turnState{turn: turn, complete: complete, started: time.Now()})
	return turn, nil
}

// TrackTurn registers a turn the harness started on its own, such as a
// turn started by another client, so its items are collected. It returns
// the existing handle when the turn is already tracked, or nil once the
// session has ended.
func (r *Runtime) TrackTurn(id string) *agent.Turn {
	turn, _ := r.NewTurn(id)
	return turn
}

// CurrentTurnID returns the oldest unfinished turn, or "" when idle.
func (r *Runtime) CurrentTurnID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return ""
	}
	return r.turns[0].turn.ID()
}

// TurnIDs returns the unfinished turns in submission order.
func (r *Runtime) TurnIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, len(r.turns))
	for i, t := range r.turns {
		ids[i] = t.turn.ID()
	}
	return ids
}

// HasTurn reports whether turn id is unfinished.
func (r *Runtime) HasTurn(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnLocked(id) != nil
}

// Busy reports whether a turn is unfinished.
func (r *Runtime) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns) > 0
}

// FinishTurn completes a turn: it fills the result's TurnID, Items, and
// Duration when missing, emits TurnCompleted, and completes the turn
// handle. An empty id means the current turn. A turn that is not tracked
// still produces a TurnCompleted event. Finishing the turn that holds the
// running slot frees it, and queued input starts.
func (r *Runtime) FinishTurn(id string, res *agent.TurnResult) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	if id == "" && len(r.turns) > 0 {
		id = r.turns[0].turn.ID()
	}
	// The turn's answered or withdrawn requests resolve before it ends, so
	// their RequestResolved events stay inside the turn.
	for _, p := range slices.Clone(r.pending) {
		if p.Meta().TurnID == id && p.Outcome() != agent.RequestPending {
			r.resolveLocked(p)
		}
	}
	r.releaseLocked(id)
	state := r.removeTurnLocked(id)
	if state == nil && id != "" && slices.ContainsFunc(r.recent, func(t *agent.Turn) bool { return t.ID() == id }) {
		// The turn already ended; a second report must not complete it again.
		r.mu.Unlock()
		return
	}
	if res.TurnID == "" {
		res.TurnID = id
	}
	agentlink.NormalizeTurnResult(res)
	if state != nil {
		r.rememberLocked(state.turn)
		if res.Items == nil {
			res.Items = state.items
		}
		if res.Duration == 0 {
			res.Duration = time.Since(state.started)
		}
	}
	ev := &agent.TurnCompleted{EventMeta: agent.EventMeta{TurnID: res.TurnID, Raw: res.Raw}, Result: res}
	r.stampLocked(ev)
	if state != nil {
		addTurnEvent(state.turn, ev)
	}
	r.deliverLocked(ev)
	r.mu.Unlock()
	if state != nil {
		state.complete(res, nil)
	}
}

// AbandonTurn completes a tracked turn with an error and no result, for
// input the harness rejected. No event is emitted. It frees the running
// slot when id holds it, even when id is not tracked.
func (r *Runtime) AbandonTurn(id string, err error) {
	r.mu.Lock()
	r.releaseLocked(id)
	state := r.removeTurnLocked(id)
	if state != nil {
		r.rememberLocked(state.turn)
	}
	r.mu.Unlock()
	if state != nil {
		state.complete(nil, err)
	}
}

// removeTurnLocked stops tracking turn id and returns its state, or nil.
func (r *Runtime) removeTurnLocked(id string) *turnState {
	i := slices.IndexFunc(r.turns, func(t *turnState) bool { return t.turn.ID() == id })
	if i < 0 {
		return nil
	}
	state := r.turns[i]
	r.turns = slices.Delete(r.turns, i, i+1)
	return state
}

func (r *Runtime) rememberLocked(t *agent.Turn) {
	r.recent = append(r.recent, t)
	if len(r.recent) > recentTurns {
		r.recent = slices.Delete(r.recent, 0, len(r.recent)-recentTurns)
	}
}

// WithdrawAll withdraws every unanswered request, such as when the harness
// process that asked them is replaced.
func (r *Runtime) WithdrawAll() {
	for _, req := range r.PendingRequests() {
		r.WithdrawRequest(req.RequestID())
	}
}

// Request registers a request that needs an answer. When a matching
// handler is configured it answers the request; otherwise the request is
// delivered as an event. Either way a RequestResolved event follows once the
// request is answered or withdrawn.
func (r *Runtime) Request(req agent.Request) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		WithdrawRequest(req)
		return
	}
	if r.withdrawn[req.RequestID()] {
		// The harness withdrew the request before it was registered.
		delete(r.withdrawn, req.RequestID())
		r.mu.Unlock()
		WithdrawRequest(req)
		return
	}
	r.stampLocked(req)
	r.pending = append(r.pending, req)
	handled := r.dispatchHandler(req)
	if !handled {
		if t := r.turnLocked(req.Meta().TurnID); t != nil {
			addTurnEvent(t.turn, req)
		}
		// Requests are delivered even during startup, because the harness
		// may wait for an answer before startup can finish.
		r.queue.push(req)
	}
	r.mu.Unlock()
	go r.watchRequest(req)
}

func (r *Runtime) watchRequest(req agent.Request) {
	select {
	case <-req.Done():
	case <-r.done:
		WithdrawRequest(req)
	}
	r.mu.Lock()
	r.resolveLocked(req)
	r.mu.Unlock()
}

// resolveLocked removes a request that stopped being pending and emits its
// RequestResolved event, once. The caller holds r.mu.
func (r *Runtime) resolveLocked(req agent.Request) {
	i := slices.Index(r.pending, req)
	if i < 0 {
		return
	}
	r.pending = slices.Delete(r.pending, i, i+1)
	if r.finished {
		return
	}
	r.emitLocked(&agent.RequestResolved{
		EventMeta: agent.EventMeta{TurnID: req.Meta().TurnID},
		Request:   req,
		Outcome:   req.Outcome(),
	})
}

// WithdrawRequest withdraws the pending request with the given ID, for
// harnesses that cancel a request themselves. A request withdrawn before
// it is registered is withdrawn as soon as it is.
func (r *Runtime) WithdrawRequest(id string) {
	r.mu.Lock()
	var found agent.Request
	for _, req := range r.pending {
		if req.RequestID() == id {
			found = req
			break
		}
	}
	if found == nil {
		if r.withdrawn == nil {
			r.withdrawn = make(map[string]bool)
		}
		if len(r.withdrawn) >= maxWithdrawn {
			clear(r.withdrawn)
		}
		r.withdrawn[id] = true
	}
	r.mu.Unlock()
	if found != nil {
		WithdrawRequest(found)
	}
}

// maxWithdrawn bounds the IDs remembered for requests withdrawn before
// they were registered.
const maxWithdrawn = 256

// PendingRequests lists unanswered requests in arrival order.
func (r *Runtime) PendingRequests() []agent.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A request answered a moment ago may not be resolved yet.
	return slices.DeleteFunc(slices.Clone(r.pending), func(p agent.Request) bool { return p.Outcome() != agent.RequestPending })
}

func (r *Runtime) dispatchHandler(req agent.Request) bool {
	switch req := req.(type) {
	case *agent.PermissionRequest:
		if h := r.handlers.Permission; h != nil {
			go r.runHandler(req, func(ctx context.Context) error { return req.Respond(r.ctx, h(ctx, req)) })
			return true
		}
	case *agent.UserInputRequest:
		if h := r.handlers.UserInput; h != nil {
			go r.runHandler(req, func(ctx context.Context) error { return req.Respond(r.ctx, h(ctx, req)) })
			return true
		}
	case *agent.ElicitationRequest:
		if h := r.handlers.Elicitation; h != nil {
			go r.runHandler(req, func(ctx context.Context) error { return req.Respond(r.ctx, h(ctx, req)) })
			return true
		}
	}
	return false
}

func (r *Runtime) runHandler(req agent.Request, run func(ctx context.Context) error) {
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	go func() {
		select {
		case <-req.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		if p := recover(); p != nil {
			r.logger.Error("request handler panicked; cancelling request", "request", req.RequestID(), "panic", fmt.Sprint(p))
			_ = req.Cancel(r.ctx)
		}
	}()
	if err := run(ctx); err != nil && ctx.Err() == nil {
		r.logger.Warn("answering request failed", "request", req.RequestID(), "error", err)
		if req.Outcome() == agent.RequestPending {
			// A pending request would stall the turn; refuse it instead.
			if p, ok := req.(*agent.PermissionRequest); ok {
				_ = p.Deny(r.ctx, "The host's answer could not be used.")
			} else {
				_ = req.Cancel(r.ctx)
			}
		}
	}
}

// Finish ends the session: pending requests are withdrawn, unfinished turns
// fail with err (or agent.ErrClosed when err is nil), and the event channel
// closes after the queued events are delivered. Only the first call has an
// effect.
func (r *Runtime) Finish(err error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.err = err
	// Events held back during a startup that never completed are dropped
	// with the session; once started, nothing is held back.
	r.early = nil
	turns := r.turns
	r.turns = nil
	r.inputs = nil
	pending := r.pending
	r.mu.Unlock()

	turnErr := err
	if turnErr == nil {
		turnErr = agent.ErrClosed
	}
	for _, t := range turns {
		t.complete(nil, turnErr)
	}
	for _, req := range pending {
		WithdrawRequest(req)
	}
	r.cancel()
	r.queue.close()
	close(r.done)
}

// Done is closed after Finish.
func (r *Runtime) Done() <-chan struct{} { return r.done }

// Err returns the error passed to Finish.
func (r *Runtime) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Finished reports whether Finish was called.
func (r *Runtime) Finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// ClosedErr returns the error to report for calls on an ended session.
func (r *Runtime) ClosedErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closedErrLocked()
}

func (r *Runtime) closedErrLocked() error {
	if r.err != nil {
		return fmt.Errorf("%w: %w", agent.ErrClosed, r.err)
	}
	return agent.ErrClosed
}

// Run sends input on s and waits for the turn. When ctx ends before the turn
// finishes, Run stops that turn, through agent.TurnInterrupter when s
// implements it and else with Interrupt, and returns ctx.Err().
func Run(ctx context.Context, s agent.Session, in agent.Input) (*agent.TurnResult, error) {
	turn, err := s.Send(ctx, in)
	if err != nil {
		return nil, err
	}
	res, err := turn.Wait(ctx)
	if err != nil && ctx.Err() != nil {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if ti, ok := s.(agent.TurnInterrupter); ok {
			_ = ti.InterruptTurn(stop, turn.ID())
		} else {
			_ = s.Interrupt(stop)
		}
	}
	return res, err
}
