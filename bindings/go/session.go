package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
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
	queue        []Event
	wake         chan struct{}
	finished     bool
	err          error
	done         chan struct{}
	// turnLogs holds the events of the turns Send started, by ID, from
	// before the request until the turn ends.
	turnLogs map[string]*turnLog

	eventsOnce sync.Once
	events     chan Event
}

// Handlers answer requests with callbacks instead of events. A request
// with a handler does not appear in Session.Events or Turn.Events. Each
// call runs on its own goroutine; its context ends when the client closes.
type Handlers struct {
	Permission  func(ctx context.Context, req *PermissionRequest) PermissionResponse
	UserInput   func(ctx context.Context, req *UserInputRequest) UserInputResponse
	Elicitation func(ctx context.Context, req *ElicitationRequest) ElicitationResponse
}

func newSession(c *Client, handle string, o sessionOptions) *Session {
	s := &Session{
		client:   c,
		handle:   handle,
		opts:     o,
		tools:    map[string]*Tool{},
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		turnLogs: map[string]*turnLog{},
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

// Events returns the session's events in arrival order. The same channel
// is returned on every call; it is closed after the session ended and the
// last event was read. Events are queued without limit until read (set
// Config.EventBufferLimit to cap them in the library), so read the channel
// until it closes once you call Events.
func (s *Session) Events() <-chan Event {
	s.eventsOnce.Do(func() {
		s.events = make(chan Event)
		go s.pump()
	})
	return s.events
}

func (s *Session) pump() {
	defer close(s.events)
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue[0] = Event{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
			s.events <- ev
			continue
		}
		finished := s.finished
		s.mu.Unlock()
		if finished {
			return
		}
		<-s.wake
	}
}

func (s *Session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// deliver takes one event from the reader goroutine. It never blocks.
func (s *Session) deliver(ev Event) {
	switch p := ev.Payload.(type) {
	case SessionStarted:
		s.setInfo(p.Info)
	case SessionUpdated:
		s.setInfo(p.Info)
	case PermissionRequest:
		if h := s.opts.handlers.Permission; h != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h(ctx, &p) }, PermissionResponse{Decision: PermissionDecisionDeny})
			return
		}
	case UserInputRequest:
		if h := s.opts.handlers.UserInput; h != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h(ctx, &p) }, UserInputResponse{Cancelled: true})
			return
		}
	case ElicitationRequest:
		if h := s.opts.handlers.Elicitation; h != nil {
			go s.runHandler(p.ID, func(ctx context.Context) Response { return h(ctx, &p) }, ElicitationResponse{Action: ElicitationResponseActionDecline})
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if log := s.turnLogs[ev.TurnID]; log != nil {
		log.add(ev)
		if tc, ok := ev.Payload.(TurnCompleted); ok && (tc.Result.TurnID == "" || tc.Result.TurnID == ev.TurnID) {
			log.end()
			delete(s.turnLogs, ev.TurnID)
		}
	}
	s.queue = append(s.queue, ev)
	s.signal()
}

// runHandler answers a request with a handler; a panic gives fallback.
func (s *Session) runHandler(requestID string, run func(context.Context) Response, fallback Response) {
	resp := fallback
	func() {
		defer func() { _ = recover() }()
		resp = run(s.client.ctx)
	}()
	_ = s.Respond(s.client.ctx, requestID, resp)
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
// UserInputResponse, or ElicitationResponse.
type Response interface{ isResponse() }

func (PermissionResponse) isResponse()  {}
func (UserInputResponse) isResponse()   {}
func (ElicitationResponse) isResponse() {}

// Respond answers request requestID, the ID of a PermissionRequest,
// UserInputRequest, or ElicitationRequest.
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
