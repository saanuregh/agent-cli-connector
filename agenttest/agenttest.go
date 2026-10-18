// Package agenttest provides a scripted fake agent.Provider for testing code
// that uses this library, without a harness CLI or a model.
//
// A Script plays each turn: it streams text, reports tool calls, and asks
// for permissions or answers through the same events, handlers, and turn
// results a real provider produces.
//
//	p := agenttest.New(func(t *agenttest.Turn) error {
//		if t.Permission(agent.PermissionRequest{ToolName: "bash", Command: "ls"}).Decision != agent.DecisionAllow {
//			t.Text("Not allowed.")
//			return nil
//		}
//		t.ToolCall(agent.ToolShell, "bash", `{"command":"ls"}`, "main.go")
//		t.Text("There is one file.")
//		return nil
//	})
//	sess, err := p.Start(ctx, agent.Config{})
//
// Sessions behave like the real ones: input sent while a turn runs is queued
// (DeliverSteer is treated as queueing, and DeliverInterrupt interrupts the
// running turn first), Interrupt cancels the running script's context, and
// Close ends the session.
package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Name is the provider name of a Provider.
const Name = "agenttest"

// Script plays one turn. Its error fails the turn with that message; a
// script that returns after its context was cancelled reports the turn
// interrupted.
type Script func(t *Turn) error

// Provider is a fake agent.Provider whose sessions play Script.
type Provider struct {
	script Script

	mu       sync.Mutex
	sessions []*Session
}

var _ agent.Provider = (*Provider)(nil)

// New returns a provider whose sessions play script for every turn.
func New(script Script) *Provider { return &Provider{script: script} }

// Name returns "agenttest".
func (p *Provider) Name() string { return Name }

// Capabilities reports what the fake supports.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:        true,
		ReasoningStream:  true,
		Images:           true,
		Queue:            true,
		Interrupt:        true,
		SetModel:         true,
		SetEffort:        true,
		PermissionPrompt: true,
		UserQuestions:    true,
		PermissionModes: []agent.PermissionMode{
			agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionPlan,
			agent.PermissionReadOnly, agent.PermissionDontAsk, agent.PermissionBypass,
		},
	}
}

// Start returns a started session. cfg.Resume becomes the session ID.
func (p *Provider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	return p.StartSession(ctx, cfg)
}

// StartSession returns a started session as its concrete type.
func (p *Provider) StartSession(ctx context.Context, cfg agent.Config) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &Session{provider: p, rt: providerkit.NewRuntime(Name, cfg)}
	id := cfg.Resume
	if id == "" || cfg.Fork {
		id = cfg.SessionID
	}
	if id == "" {
		id = providerutil.NewUUID()
	}
	s.info = agent.SessionInfo{ID: id, Provider: Name, Cwd: providerutil.AbsDir(cfg.Cwd), Model: cfg.Model,
		Effort: cfg.Effort, PermissionMode: cfg.PermissionMode, Title: cfg.Title}
	s.rt.SetSessionID(id)
	s.rt.SetStarter(s.start)
	s.rt.Started(&agent.SessionStarted{Info: s.Info(), Resumed: cfg.Resume != "" && !cfg.Fork})
	p.mu.Lock()
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
	return s, nil
}

// Sessions returns the sessions started so far, oldest first.
func (p *Provider) Sessions() []*Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Session(nil), p.sessions...)
}

// Session is a fake agent.Session.
type Session struct {
	provider *Provider
	rt       *providerkit.Runtime

	mu      sync.Mutex
	info    agent.SessionInfo
	inputs  []agent.Input
	cancels map[string]context.CancelFunc
	closed  atomic.Bool
}

var (
	_ agent.Session         = (*Session)(nil)
	_ agent.TurnInterrupter = (*Session)(nil)
)

// ID returns the session ID.
func (s *Session) ID() string { return s.rt.SessionID() }

// Provider returns "agenttest".
func (s *Session) Provider() string { return Name }

// Info returns the session metadata, updated by SetModel, SetEffort, and
// SetPermissionMode.
func (s *Session) Info() agent.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Capabilities reports the provider's capabilities.
func (s *Session) Capabilities() agent.Capabilities { return s.provider.Capabilities() }

// Events returns the event stream.
func (s *Session) Events() <-chan agent.Event { return s.rt.Events() }

// Inputs returns the inputs the session received, in order.
func (s *Session) Inputs() []agent.Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Input(nil), s.inputs...)
}

// Send submits input; see the package doc for queueing.
func (s *Session) Send(ctx context.Context, in agent.Input) (*agent.Turn, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	if err := providerkit.CheckInput(Name, in); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.inputs = append(s.inputs, in)
	s.mu.Unlock()
	id := providerutil.NewUUID()
	turn, started, err := s.rt.StartIdle(ctx, id, func(ctx context.Context) error { return s.start(ctx, id, in) })
	if started {
		return turn, err
	}
	current, _ := s.rt.Running()
	if turn, err = s.rt.Enqueue(id, in); err != nil {
		return nil, err
	}
	if in.Delivery == agent.DeliverInterrupt && current != "" {
		err = s.InterruptTurn(ctx, current)
	}
	return turn, err
}

// Run sends input and waits for its turn.
func (s *Session) Run(ctx context.Context, in agent.Input) (*agent.TurnResult, error) {
	return providerkit.Run(ctx, s, in)
}

// start runs the script for turn id in the background.
func (s *Session) start(_ context.Context, id string, in agent.Input) error {
	ctx, cancel := context.WithCancel(s.rt.Context())
	s.mu.Lock()
	if s.cancels == nil {
		s.cancels = map[string]context.CancelFunc{}
	}
	s.cancels[id] = cancel
	s.mu.Unlock()
	s.rt.Emit(&agent.TurnStarted{EventMeta: agent.EventMeta{TurnID: id}})
	s.rt.Emit(&agent.StatusChanged{EventMeta: agent.EventMeta{TurnID: id}, Status: agent.StatusRunning})
	t := &Turn{ctx: ctx, session: s, id: id, input: in}
	s.rt.Emit(&agent.ItemCompleted{EventMeta: agent.EventMeta{TurnID: id}, Item: agent.Item{ID: id + "-input",
		Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: in.PlainText(), ClientID: in.ClientID, TurnID: id}})
	go s.play(t, cancel)
	return nil
}

func (s *Session) play(t *Turn, cancel context.CancelFunc) {
	defer cancel()
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("agenttest: script panicked: %v", p)
			}
		}()
		return s.provider.script(t)
	}()
	s.mu.Lock()
	delete(s.cancels, t.id)
	s.mu.Unlock()
	if s.closed.Load() {
		return
	}
	res := &agent.TurnResult{Status: agent.TurnSucceeded, Text: t.text, Usage: t.usage, StopReason: "end_turn"}
	switch {
	case t.ctx.Err() != nil:
		res.Status = agent.TurnInterrupted
		res.Err = &agent.TurnError{Code: "aborted", Err: agent.ErrInterrupted}
		res.StopReason = "aborted"
	case err != nil:
		res.Status = agent.TurnFailed
		res.Err = &agent.TurnError{Code: "error", Message: err.Error()}
		res.StopReason = "error"
	}
	s.rt.FinishTurn(t.id, res)
	if s.rt.Idle() {
		s.rt.Emit(&agent.StatusChanged{Status: agent.StatusIdle})
	}
}

// Interrupt cancels the running turn's script.
func (s *Session) Interrupt(ctx context.Context) error { return s.InterruptTurn(ctx, "") }

// InterruptTurn drops queued input id, or cancels the script of running turn
// id; an empty id means whatever runs.
func (s *Session) InterruptTurn(ctx context.Context, id string) error {
	if id != "" && s.rt.CancelQueued(id) {
		return nil
	}
	current, _ := s.rt.Interrupt(id)
	if current == "" {
		return nil
	}
	s.mu.Lock()
	cancel := s.cancels[current]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// SetModel records the model.
func (s *Session) SetModel(ctx context.Context, model string) error {
	return s.update(func(i *agent.SessionInfo) { i.Model = model })
}

// SetEffort records the effort.
func (s *Session) SetEffort(ctx context.Context, effort agent.Effort) error {
	return s.update(func(i *agent.SessionInfo) { i.Effort = effort })
}

// SetPermissionMode records the mode.
func (s *Session) SetPermissionMode(ctx context.Context, mode agent.PermissionMode) error {
	return s.update(func(i *agent.SessionInfo) { i.PermissionMode = mode })
}

func (s *Session) update(f func(*agent.SessionInfo)) error {
	if s.rt.Finished() {
		return s.rt.ClosedErr()
	}
	s.mu.Lock()
	f(&s.info)
	info := s.info
	s.mu.Unlock()
	s.rt.Emit(&agent.SessionUpdated{Info: info})
	return nil
}

// PendingRequests lists unanswered requests.
func (s *Session) PendingRequests() []agent.Request { return s.rt.PendingRequests() }

// Close ends the session: running scripts are cancelled and unfinished
// turns fail with agent.ErrClosed.
func (s *Session) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.mu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	s.rt.Finish(nil)
	return nil
}

// Done is closed after the session ended.
func (s *Session) Done() <-chan struct{} { return s.rt.Done() }

// Err returns the reason the session ended.
func (s *Session) Err() error { return s.rt.Err() }

// Turn is the running turn a Script plays.
type Turn struct {
	ctx     context.Context
	session *Session
	id      string
	input   agent.Input
	text    string
	usage   agent.Usage
	items   int
}

// Context is cancelled when the turn is interrupted or the session closes.
func (t *Turn) Context() context.Context { return t.ctx }

// ID returns the turn ID.
func (t *Turn) ID() string { return t.id }

// Input returns the input that started the turn.
func (t *Turn) Input() agent.Input { return t.input }

func (t *Turn) meta() agent.EventMeta { return agent.EventMeta{TurnID: t.id} }

func (t *Turn) nextID(kind string) string {
	t.items++
	return t.id + "-" + kind + "-" + strconv.Itoa(t.items)
}

// Text streams text as one assistant message, a TextDelta per call, and
// adds it to the turn's final text.
func (t *Turn) Text(text string) {
	id := t.nextID("text")
	t.session.rt.Emit(&agent.TextDelta{EventMeta: t.meta(), ItemID: id, Delta: text})
	t.session.rt.Emit(&agent.ItemCompleted{EventMeta: t.meta(), Item: agent.Item{ID: id, Kind: agent.ItemAssistantMessage,
		Status: agent.ItemDone, Text: text, TurnID: t.id}})
	t.text += text
}

// Reasoning streams reasoning text.
func (t *Turn) Reasoning(text string) {
	id := t.nextID("reasoning")
	t.session.rt.Emit(&agent.ReasoningDelta{EventMeta: t.meta(), ItemID: id, Delta: text})
	t.session.rt.Emit(&agent.ItemCompleted{EventMeta: t.meta(), Item: agent.Item{ID: id, Kind: agent.ItemReasoning,
		Status: agent.ItemDone, Text: text, TurnID: t.id}})
}

// ToolCall reports a finished tool call with its JSON input and output.
func (t *Turn) ToolCall(kind agent.ToolKind, name, input, output string) {
	id := t.nextID("tool")
	call := &agent.ToolCall{Name: name, Kind: kind, Input: json.RawMessage(input)}
	t.session.rt.Emit(&agent.ItemStarted{EventMeta: t.meta(), Item: agent.Item{ID: id, Kind: agent.ItemToolCall,
		Status: agent.ItemRunning, Tool: call, TurnID: t.id}})
	done := *call
	done.Output = output
	t.session.rt.Emit(&agent.ItemCompleted{EventMeta: t.meta(), Item: agent.Item{ID: id, Kind: agent.ItemToolCall,
		Status: agent.ItemDone, Tool: &done, TurnID: t.id}})
}

// Usage adds token usage to the turn's result.
func (t *Turn) Usage(u agent.Usage) {
	t.usage.Add(u)
	t.session.rt.Emit(&agent.UsageUpdated{EventMeta: t.meta(), Usage: t.usage})
}

// Emit delivers any event for this turn.
func (t *Turn) Emit(ev agent.Event) {
	if m := ev.Meta(); m.TurnID == "" {
		m.TurnID = t.id
	}
	t.session.rt.Emit(ev)
}

// Permission asks for permission and waits for the answer, from
// Config.Handlers or from PermissionRequest.Respond. A withdrawn request
// reads as a denial with the message "request withdrawn". ID defaults to
// a new one.
func (t *Turn) Permission(req agent.PermissionRequest) agent.PermissionResponse {
	if req.ID == "" {
		req.ID = t.nextID("permission")
	}
	answer := make(chan agent.PermissionResponse, 1)
	r := providerkit.NewPermissionRequest(req, func(_ context.Context, resp agent.PermissionResponse) error {
		answer <- resp
		return nil
	})
	return waitAnswer(t, r, answer, agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "request withdrawn"})
}

// Ask asks the user questions and waits for the answer. A withdrawn
// request reads as cancelled.
func (t *Turn) Ask(questions ...agent.Question) agent.UserInputResponse {
	answer := make(chan agent.UserInputResponse, 1)
	r := providerkit.NewUserInputRequest(agent.UserInputRequest{ID: t.nextID("question"), Questions: questions},
		func(_ context.Context, resp agent.UserInputResponse) error {
			answer <- resp
			return nil
		})
	return waitAnswer(t, r, answer, agent.UserInputResponse{Cancelled: true})
}

func waitAnswer[T any](t *Turn, r agent.Request, answer <-chan T, withdrawn T) T {
	r.Meta().TurnID = t.id
	t.session.rt.Request(r)
	select {
	case resp := <-answer:
		return resp
	case <-t.ctx.Done():
		t.session.rt.WithdrawRequest(r.RequestID())
		return withdrawn
	case <-r.Done():
		select {
		case resp := <-answer:
			return resp
		default:
			return withdrawn
		}
	}
}
