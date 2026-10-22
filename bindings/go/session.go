package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"time"
)

// Session is one live conversation with a harness. All methods are safe
// for concurrent use. Methods that talk to the harness take a context that
// bounds only that call.
type Session struct {
	client   *Client
	handle   string
	provider string
	caps     Capabilities
	opts     sessionOptions
	tools    map[string]*Tool

	mu   sync.Mutex
	info SessionInfo
	// infoReported is set once an event reported info, which is then newer
	// than the start result.
	infoReported bool
	// buffering is set by the first Events call; only then are events
	// queued for it.
	buffering bool
	// sessionStarted is the session_started event received before the
	// first Events call, which Events yields first.
	sessionStarted *Event
	queue          eventQueue
	finished       bool
	err            error
	done           chan struct{}
	// turnLogs holds the events of the turns Send started, by ID, from
	// before the request until the turn ends.
	turnLogs map[string]*turnLog

	eventsOnce sync.Once
	events     chan Event

	// listeners is replaced, never modified, so delivery reads it without
	// copying.
	listenersMu  sync.Mutex
	listeners    []listenerEntry
	nextListener uint64
	// listening is set by the first Subscribe; from then on events are
	// queued in listenQueue for the goroutine that calls listeners.
	listening   bool
	listenQueue eventQueue
}

type listenerEntry struct {
	id uint64
	fn func(Event)
}

// eventQueue is a FIFO guarded by the owning Session's mu; wake tells its
// drain goroutine that an event arrived or the session finished.
type eventQueue struct {
	events []Event
	wake   chan struct{}
}

func newEventQueue() eventQueue { return eventQueue{wake: make(chan struct{}, 1)} }

// push must be called with Session.mu held.
func (q *eventQueue) push(ev Event) {
	q.events = append(q.events, ev)
	q.signal()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// drain calls deliver with each event of q in order until the session ended
// and q is empty.
func (s *Session) drain(q *eventQueue, deliver func(Event)) {
	for {
		s.mu.Lock()
		if len(q.events) > 0 {
			ev := q.events[0]
			q.events[0] = Event{}
			q.events = q.events[1:]
			s.mu.Unlock()
			deliver(ev)
			continue
		}
		finished := s.finished
		s.mu.Unlock()
		if finished {
			return
		}
		<-q.wake
	}
}

// Handlers answer requests with callbacks. A request with a handler still
// appears in Session.Events, Turn.Events, and Subscribe listeners;
// Session.Handled reports it, so the reader does not answer it again. Each
// call runs on its own goroutine; its context ends when the client closes.
// A handler that panics fails closed: a permission is denied, user input
// and dialogs are cancelled, an elicitation is declined, and a native
// request gets error code -32603 with the panic text.
type Handlers struct {
	Permission  func(ctx context.Context, req *PermissionRequest) PermissionResponse
	UserInput   func(ctx context.Context, req *UserInputRequest) UserInputResponse
	Elicitation func(ctx context.Context, req *ElicitationRequest) ElicitationResponse
	Dialog      func(ctx context.Context, req *DialogRequest) DialogResponse
	Native      func(ctx context.Context, req *NativeRequest) NativeResponse
}

func newSession(c *Client, handle string, o sessionOptions) *Session {
	s := &Session{
		client:      c,
		handle:      handle,
		opts:        o,
		tools:       map[string]*Tool{},
		queue:       newEventQueue(),
		listenQueue: newEventQueue(),
		done:        make(chan struct{}),
		turnLogs:    map[string]*turnLog{},
	}
	for i := range o.tools {
		s.tools[o.tools[i].Name] = &o.tools[i]
	}
	return s
}

// started records the session/start or session/fork result, before the
// session is returned to the caller.
func (s *Session) started(res StartResult) {
	s.provider = res.Info.Provider
	s.caps = res.Capabilities
	s.mu.Lock()
	if !s.infoReported {
		s.info = res.Info
	}
	s.mu.Unlock()
}

// Handle returns the library's handle for the session. It stays the same
// when the native ID changes.
func (s *Session) Handle() string { return s.handle }

// ID returns the native session ID, the value for Config.ResumeID.
func (s *Session) ID() string { return s.Info().ID }

// Provider returns the provider name, such as "claude".
func (s *Session) Provider() string { return s.provider }

// Capabilities reports which uniform features the session supports.
func (s *Session) Capabilities() Capabilities { return s.caps }

// Info returns the latest session metadata the session reported. FetchInfo
// asks the session.
func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Done is closed after the session ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended, or nil while it runs and after a
// clean close.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Events returns the session's events in arrival order, from the first
// call on; of the earlier events only session_started is kept, and it
// comes first (Subscribe sees every event as it arrives). The same
// channel is returned on every call; it is closed after the session ended
// and the last event was read. Events are queued without
// limit until read (set Config.EventBufferLimit to cap them in the
// library), so read the channel until it closes once you call Events.
func (s *Session) Events() <-chan Event {
	s.eventsOnce.Do(func() {
		s.mu.Lock()
		s.buffering = true
		if s.sessionStarted != nil {
			s.queue.events = append(s.queue.events, *s.sessionStarted)
			s.sessionStarted = nil
		}
		s.mu.Unlock()
		s.events = make(chan Event)
		go s.pump()
	})
	return s.events
}

func (s *Session) pump() {
	defer close(s.events)
	s.drain(&s.queue, func(ev Event) { s.events <- ev })
}

// signal wakes both drain goroutines after the session finished.
func (s *Session) signal() {
	s.queue.signal()
	s.listenQueue.signal()
}

// deliver takes one event from the reader goroutine. It never blocks.
func (s *Session) deliver(ev Event) {
	h := s.opts.handlers
	switch p := ev.Payload.(type) {
	case SessionStarted:
		s.setInfo(p.Info)
	case SessionUpdated:
		s.setInfo(p.Info)
	case PermissionRequest:
		if h.Permission != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h.Permission(ctx, &p) },
				func(any) Response { return PermissionResponse{Decision: PermissionDecisionDeny} })
		}
	case UserInputRequest:
		if h.UserInput != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h.UserInput(ctx, &p) },
				func(any) Response { return UserInputResponse{Cancelled: true} })
		}
	case ElicitationRequest:
		if h.Elicitation != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h.Elicitation(ctx, &p) },
				func(any) Response { return ElicitationResponse{Action: ElicitationResponseActionDecline} })
		}
	case DialogRequest:
		if h.Dialog != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h.Dialog(ctx, &p) },
				func(any) Response { return DialogResponse{Behavior: DialogResponseBehaviorCancelled} })
		}
	case NativeRequest:
		if h.Native != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h.Native(ctx, &p) },
				func(failure any) Response {
					return NativeResponse{Error: &NativeResponseError{Code: -32603, Message: fmt.Sprint(failure)}}
				})
		}
	}

	s.mu.Lock()
	if log := s.turnLogs[ev.TurnID]; log != nil {
		log.add(ev)
		if tc, ok := ev.Payload.(TurnCompleted); ok && (tc.Result.TurnID == "" || tc.Result.TurnID == ev.TurnID) {
			log.end()
			delete(s.turnLogs, ev.TurnID)
		}
	}
	if s.buffering {
		s.queue.push(ev)
	} else if ev.Type == EventTypeSessionStarted {
		s.sessionStarted = &ev
	}
	if s.listening {
		s.listenQueue.push(ev)
	}
	s.mu.Unlock()
}

// notifyListeners calls the Subscribe listeners with each queued event, in
// order, until the session ended and the queue is empty.
func (s *Session) notifyListeners() {
	s.drain(&s.listenQueue, func(ev Event) {
		s.listenersMu.Lock()
		listeners := s.listeners
		s.listenersMu.Unlock()
		for _, l := range listeners {
			func() {
				defer func() { _ = recover() }()
				l.fn(ev)
			}()
		}
	})
}

// runHandler answers a request with a handler; a panic answers with
// fallback(panic value).
func (s *Session) runHandler(requestID string, run func(context.Context) Response, fallback func(failure any) Response) {
	var resp Response
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				resp = fallback(failure)
			}
		}()
		resp = run(s.client.ctx)
	}()
	_ = s.Respond(s.client.ctx, requestID, resp)
}

// Handled reports whether ev is a request that one of the session's
// Handlers answers, so a reader of the event stream must not answer it.
func (s *Session) Handled(ev Event) bool {
	h := s.opts.handlers
	switch ev.Payload.(type) {
	case PermissionRequest:
		return h.Permission != nil
	case UserInputRequest:
		return h.UserInput != nil
	case ElicitationRequest:
		return h.Elicitation != nil
	case DialogRequest:
		return h.Dialog != nil
	case NativeRequest:
		return h.Native != nil
	}
	return false
}

// Subscribe calls listener with every later session event. Listeners run
// in order on one goroutine per session, apart from the goroutine that
// reads the library's events, so a listener may call session methods; a
// slow listener delays only later listener calls of this session. A panic
// in a listener is ignored. Call the returned function to stop; an event
// already being delivered may still reach the listener. It is idempotent.
func (s *Session) Subscribe(listener func(Event)) (unsubscribe func()) {
	s.listenersMu.Lock()
	s.nextListener++
	id := s.nextListener
	s.listeners = append(slices.Clone(s.listeners), listenerEntry{id: id, fn: listener})
	s.listenersMu.Unlock()
	s.mu.Lock()
	if !s.listening {
		s.listening = true
		go s.notifyListeners()
	}
	s.mu.Unlock()
	return func() {
		s.listenersMu.Lock()
		s.listeners = slices.DeleteFunc(slices.Clone(s.listeners), func(x listenerEntry) bool { return x.id == id })
		s.listenersMu.Unlock()
	}
}

func (s *Session) setInfo(info SessionInfo) {
	s.mu.Lock()
	s.info = info
	s.infoReported = true
	s.mu.Unlock()
}

// finish marks the session ended. It runs on the reader goroutine.
func (s *Session) finish(errInfo *ErrorInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	if errInfo != nil {
		s.err = &Error{ErrorInfo: *errInfo}
	}
	for _, log := range s.turnLogs {
		log.end()
	}
	close(s.done)
	s.signal()
}

// ---------------------------------------------------------------- turns

// Send submits input. When the session is idle a new turn starts; when a
// turn runs, Input.Delivery decides whether the input is queued, steered
// into it, or interrupts it.
func (s *Session) Send(ctx context.Context, in Input) (*Turn, error) {
	id := newUUID()
	log := &turnLog{}
	s.mu.Lock()
	if s.finished {
		log.end()
	} else {
		s.turnLogs[id] = log
	}
	s.mu.Unlock()
	if err := s.client.call(ctx, MethodSessionSend, SendParams{Session: s.handle, Input: in, TurnID: id}, nil); err != nil {
		s.dropTurnLog(id)
		return nil, err
	}
	t := &Turn{id: id, session: s, log: log, done: make(chan struct{})}
	go t.await()
	return t, nil
}

func (s *Session) dropTurnLog(id string) {
	s.mu.Lock()
	delete(s.turnLogs, id)
	s.mu.Unlock()
}

// Run sends input and waits for its turn to end. A turn that failed or
// was interrupted returns its result and an *Error. When ctx ends, the
// turn is interrupted.
func (s *Session) Run(ctx context.Context, in Input) (*TurnResult, error) {
	t, err := s.Send(ctx, in)
	if err != nil {
		return nil, err
	}
	res, err := t.Wait(ctx)
	if ctx.Err() != nil && res == nil {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.InterruptTurn(stop, t.id)
	}
	return res, err
}

// Wait waits for turn turnID to end. Turn.Wait is the usual way.
func (s *Session) Wait(ctx context.Context, turnID string) (*TurnResult, error) {
	var res TurnResult
	if err := s.client.call(ctx, MethodSessionWait, WaitParams{Session: s.handle, TurnID: turnID}, &res); err != nil {
		return nil, err
	}
	return &res, turnError(&res)
}

// turnError is the error of a turn that failed or was interrupted.
func turnError(res *TurnResult) error {
	if res.Status == TurnStatusCompleted || res.Status == "" {
		return nil
	}
	info := ErrorInfo{Kind: ErrorKindTurnFailed, Message: "turn failed"}
	if res.Status == TurnStatusInterrupted {
		info = ErrorInfo{Kind: ErrorKindInterrupted, Message: "turn interrupted"}
	}
	if res.Error != nil {
		info = *res.Error
	}
	return &Error{ErrorInfo: info}
}

// Turn is input submitted with Send and the turn it runs as.
type Turn struct {
	id      string
	session *Session
	log     *turnLog

	done   chan struct{}
	result *TurnResult
	err    error
}

// ID returns the turn ID, as in TurnResult.TurnID and EventMeta.TurnID.
func (t *Turn) ID() string { return t.id }

// Done is closed after the turn ended.
func (t *Turn) Done() <-chan struct{} { return t.done }

func (t *Turn) await() {
	t.result, t.err = t.session.Wait(t.session.client.ctx, t.id)
	if t.result == nil {
		// A turn that never ran produces no turn_completed event.
		t.log.end()
		t.session.dropTurnLog(t.id)
	}
	close(t.done)
}

// Wait waits for the turn to end. A turn that failed or was interrupted
// returns its result and an *Error; input that never ran returns only an
// error, such as ErrCancelled.
func (t *Turn) Wait(ctx context.Context) (*TurnResult, error) {
	select {
	case <-t.done:
		return t.result, t.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Events yields the turn's events, from its first to its TurnCompleted.
// Every event also goes to Session.Events.
func (t *Turn) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		l := t.log
		for i := 0; ; {
			l.mu.Lock()
			if i < len(l.events) {
				ev := l.events[i]
				i++
				l.mu.Unlock()
				if !yield(ev) {
					return
				}
				continue
			}
			if l.ended {
				l.mu.Unlock()
				return
			}
			if l.notify == nil {
				l.notify = make(chan struct{})
			}
			ch := l.notify
			l.mu.Unlock()
			<-ch
		}
	}
}

// turnLog is the events of one turn, guarded by mu. notify exists only
// while a reader waits.
type turnLog struct {
	mu     sync.Mutex
	events []Event
	notify chan struct{}
	ended  bool
}

func (l *turnLog) add(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.events = append(l.events, ev)
	l.wake()
}

// wake releases waiting readers. The caller holds mu.
func (l *turnLog) wake() {
	if l.notify != nil {
		close(l.notify)
		l.notify = nil
	}
}

// end marks the log complete; later events are not added.
func (l *turnLog) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	l.wake()
}

// ---------------------------------------------------------------- requests

// Response is an answer to a request: PermissionResponse,
// UserInputResponse, ElicitationResponse, DialogResponse, or
// NativeResponse.
type Response interface{ isResponse() }

func (PermissionResponse) isResponse()  {}
func (UserInputResponse) isResponse()   {}
func (ElicitationResponse) isResponse() {}
func (DialogResponse) isResponse()      {}
func (NativeResponse) isResponse()      {}

// Respond answers request requestID, the ID of a PermissionRequest,
// UserInputRequest, ElicitationRequest, DialogRequest, or NativeRequest.
func (s *Session) Respond(ctx context.Context, requestID string, resp Response) error {
	p := RespondParams{Session: s.handle, RequestID: requestID}
	switch r := resp.(type) {
	case PermissionResponse:
		p.Permission = &r
	case *PermissionResponse:
		p.Permission = r
	case UserInputResponse:
		p.UserInput = &r
	case *UserInputResponse:
		p.UserInput = r
	case ElicitationResponse:
		p.Elicitation = &r
	case *ElicitationResponse:
		p.Elicitation = r
	case DialogResponse:
		p.Dialog = &r
	case *DialogResponse:
		p.Dialog = r
	case NativeResponse:
		p.Native = &r
	case *NativeResponse:
		p.Native = r
	default:
		return &Error{ErrorInfo: ErrorInfo{Kind: ErrorKindInvalidAnswer, Message: "unknown response type"}, Method: MethodRequestRespond}
	}
	return s.client.call(ctx, MethodRequestRespond, p, nil)
}

// Respond answers the request on session s.
func (r *PermissionRequest) Respond(ctx context.Context, s *Session, resp PermissionResponse) error {
	return s.Respond(ctx, r.ID, resp)
}

// Allow allows the request once.
func (r *PermissionRequest) Allow(ctx context.Context, s *Session) error {
	return s.Allow(ctx, r.ID)
}

// Deny refuses the request; message tells the model why.
func (r *PermissionRequest) Deny(ctx context.Context, s *Session, message string) error {
	return s.Deny(ctx, r.ID, message)
}

// Respond answers the request on session s.
func (r *UserInputRequest) Respond(ctx context.Context, s *Session, resp UserInputResponse) error {
	return s.Respond(ctx, r.ID, resp)
}

// Answer answers the questions: answers maps question IDs to the chosen
// option labels or free text.
func (r *UserInputRequest) Answer(ctx context.Context, s *Session, answers map[string][]string) error {
	return s.Answer(ctx, r.ID, answers)
}

// Respond answers the request on session s.
func (r *ElicitationRequest) Respond(ctx context.Context, s *Session, resp ElicitationResponse) error {
	return s.Respond(ctx, r.ID, resp)
}

// Accept accepts with form content (any JSON value; nil for none).
func (r *ElicitationRequest) Accept(ctx context.Context, s *Session, content any) error {
	return s.Accept(ctx, r.ID, content)
}

// Decline declines the request.
func (r *ElicitationRequest) Decline(ctx context.Context, s *Session) error {
	return s.Decline(ctx, r.ID)
}

// Respond answers the request on session s.
func (r *DialogRequest) Respond(ctx context.Context, s *Session, resp DialogResponse) error {
	return s.Respond(ctx, r.ID, resp)
}

// Complete completes the dialog with result (any JSON value; nil for none).
func (r *DialogRequest) Complete(ctx context.Context, s *Session, result any) error {
	resultJSON, err := marshalRaw(result)
	if err != nil {
		return err
	}
	return r.Respond(ctx, s, DialogResponse{Behavior: DialogResponseBehaviorCompleted, Result: resultJSON})
}

// Cancel cancels the dialog.
func (r *DialogRequest) Cancel(ctx context.Context, s *Session) error {
	return r.Respond(ctx, s, DialogResponse{Behavior: DialogResponseBehaviorCancelled})
}

// Respond answers the request on session s.
func (r *NativeRequest) Respond(ctx context.Context, s *Session, resp NativeResponse) error {
	return s.Respond(ctx, r.ID, resp)
}

// Reply answers the request with result (any JSON value; nil for none).
func (r *NativeRequest) Reply(ctx context.Context, s *Session, result any) error {
	resultJSON, err := marshalRaw(result)
	if err != nil {
		return err
	}
	return r.Respond(ctx, s, NativeResponse{Result: resultJSON})
}

// Fail answers the request with a JSON-RPC error.
func (r *NativeRequest) Fail(ctx context.Context, s *Session, code int64, message string) error {
	return r.Respond(ctx, s, NativeResponse{Error: &NativeResponseError{Code: code, Message: message}})
}

// ---------------------------------------------------------------- controls

// Fork copies the conversation into a new, independent session. cfg sets
// the new session; Config.ForkAtTurn keeps history only through one turn.
// Without options the fork has this session's tools and handlers.
func (s *Session) Fork(ctx context.Context, cfg Config, opts ...SessionOption) (*Session, error) {
	o := sessionOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if !o.toolsSet {
		o.tools = s.opts.tools
	}
	if !o.handlersSet {
		o.handlers = s.opts.handlers
	}
	return s.client.startSession(ctx, MethodSessionFork, func(cfg Config, handle string) any {
		return ForkParams{Session: s.handle, Config: cfg, Handle: handle}
	}, cfg, o)
}

// FetchInfo asks the session for its metadata.
func (s *Session) FetchInfo(ctx context.Context) (SessionInfo, error) {
	var info SessionInfo
	if err := s.client.call(ctx, MethodSessionInfo, SessionParams{Session: s.handle}, &info); err != nil {
		return info, err
	}
	s.setInfo(info)
	return info, nil
}

// marshalRaw encodes v as JSON; a nil v gives no value.
func marshalRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Close ends the session and waits until it ended. It is idempotent.
func (s *Session) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	err := s.client.call(context.Background(), MethodSessionClose, SessionParams{Session: s.handle}, nil)
	if err != nil && !errors.Is(err, ErrClosed) && !errors.Is(err, ErrSessionNotFound) {
		return err
	}
	// The session/closed notification, or the client closing, ends it.
	<-s.done
	return nil
}
