package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Request is an event that needs an answer from the host. The concrete
// types are *PermissionRequest, *UserInputRequest, and *ElicitationRequest.
//
// A request is answered once. Answering again returns ErrRequestDone. The
// harness may withdraw a request, for example when the turn is interrupted;
// Done is then closed without an answer.
type Request interface {
	Event
	// RequestID identifies the request within the session.
	RequestID() string
	// Done is closed after the request was answered or withdrawn.
	Done() <-chan struct{}
	// Cancel refuses the request: DecisionCancel for a permission, which
	// also stops the turn, and a cancellation for questions and
	// elicitations.
	Cancel(ctx context.Context) error
	// Outcome reports whether the request was answered or withdrawn, or
	// RequestPending while it waits.
	Outcome() RequestOutcome
}

// RequestOutcome tells how a request stopped being pending.
type RequestOutcome string

// Request outcomes.
const (
	RequestPending   RequestOutcome = ""
	RequestAnswered  RequestOutcome = "answered"
	RequestWithdrawn RequestOutcome = "withdrawn"
)

// Handlers answer requests through callbacks. A request with a matching
// handler is not delivered as an event; the handler runs on its own
// goroutine and its return value is sent to the harness. The context is
// cancelled when the harness withdraws the request or the session ends.
// A RequestResolved event is still emitted after the answer. When the
// response cannot be sent, such as one with an empty or unknown decision or
// action, the request is denied (permissions) or cancelled instead, so a
// mistake never allows an action or stalls the turn.
type Handlers struct {
	Permission  func(ctx context.Context, req *PermissionRequest) PermissionResponse
	UserInput   func(ctx context.Context, req *UserInputRequest) UserInputResponse
	Elicitation func(ctx context.Context, req *ElicitationRequest) ElicitationResponse
}

// requestState tracks whether a request was answered.
type requestState struct {
	mu      sync.Mutex
	done    chan struct{}
	outcome RequestOutcome
}

func newRequestState() *requestState {
	return &requestState{done: make(chan struct{})}
}

// answer runs send once. If send fails the request stays open so the
// caller can try again.
func (s *requestState) answer(send func() error) error {
	if s == nil {
		return ErrRequestDone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome != RequestPending {
		return ErrRequestDone
	}
	if err := send(); err != nil {
		return err
	}
	s.finishLocked(RequestAnswered)
	return nil
}

func (s *requestState) withdraw() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcome == RequestPending {
		s.finishLocked(RequestWithdrawn)
	}
}

func (s *requestState) finishLocked(outcome RequestOutcome) {
	s.outcome = outcome
	close(s.done)
}

var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Done is closed once the request is answered or withdrawn.
func (s *requestState) Done() <-chan struct{} {
	if s == nil {
		return closedChan
	}
	return s.done
}

// Outcome reads the outcome without s.mu, which an answer holds while it is
// sent: outcome is written before done closes, so it is safe to read once
// done is closed.
func (s *requestState) Outcome() RequestOutcome {
	if s == nil {
		return RequestPending
	}
	select {
	case <-s.done:
		return s.outcome
	default:
		return RequestPending
	}
}

// withdrawRequest marks a request as withdrawn by the harness without
// sending an answer.
type withdrawable interface{ withdraw() }

func withdrawRequest(r Request) {
	if r, ok := r.(withdrawable); ok {
		r.withdraw()
	}
}

// PermissionKind classifies what a permission request asks for.
type PermissionKind string

// Permission request kinds.
const (
	// PermissionTool asks to run a tool call.
	PermissionTool PermissionKind = "tool"
	// PermissionCommand asks to run a shell command.
	PermissionCommand PermissionKind = "command"
	// PermissionFileChange asks to apply file changes.
	PermissionFileChange PermissionKind = "file_change"
	// PermissionGrant asks for extra sandbox access such as network or
	// write access to more paths.
	PermissionGrant PermissionKind = "grant"
	// PermissionPlanApproval asks to approve a plan and leave plan mode.
	PermissionPlanApproval PermissionKind = "plan_approval"
)

// PermissionDecision is the answer to a permission request.
type PermissionDecision string

// Permission decisions. Harnesses without session or persistent grants
// treat AllowSession and AllowAlways as Allow.
const (
	// DecisionAllow allows this one call.
	DecisionAllow PermissionDecision = "allow"
	// DecisionAllowSession allows this call and similar calls for the rest
	// of the session.
	DecisionAllowSession PermissionDecision = "allow_session"
	// DecisionAllowAlways allows this call and stores a rule so similar
	// calls are allowed in later sessions too, where the harness supports
	// it.
	DecisionAllowAlways PermissionDecision = "allow_always"
	// DecisionDeny refuses the call; the model is told and continues.
	DecisionDeny PermissionDecision = "deny"
	// DecisionCancel refuses the call and stops the turn.
	DecisionCancel PermissionDecision = "cancel"
)

// PermissionResponse answers a PermissionRequest.
type PermissionResponse struct {
	Decision PermissionDecision
	// Message tells the model why a call was refused.
	Message string
	// UpdatedInput replaces the tool arguments of an allowed call, where
	// the harness supports it (Claude, Pi). Other harnesses refuse an
	// allowing answer that sets it rather than run the original call.
	UpdatedInput json.RawMessage
	// ProviderResponse replaces the whole native answer when set, such as
	// claude.PermissionResult or a Codex decision value. Use it for native
	// options the uniform fields cannot express.
	ProviderResponse any
}

// PermissionRequest asks the host to allow or refuse an action.
type PermissionRequest struct {
	EventMeta
	ID   string
	Kind PermissionKind
	// ToolName is the native tool name.
	ToolName string
	// ToolCallID is the transcript item of the call, when known.
	ToolCallID string
	// Input is the tool arguments as JSON.
	Input json.RawMessage
	// Title is a one-line prompt such as "Claude wants to run npm test".
	Title string
	// Description adds detail to Title.
	Description string
	// Reason is the harness's explanation of why it asks.
	Reason string
	// Command and Cwd describe command requests.
	Command string
	Cwd     string
	// Changes describes file change requests.
	Changes []FileChange
	// Paths lists the paths the request is about, such as a blocked path
	// or a root to grant write access to.
	Paths []string
	// Plan is the proposed plan of a plan approval.
	Plan string
	// Decisions lists the decisions the harness accepts for this request.
	Decisions []PermissionDecision
	// SubagentID is the subagent that made the call, if any.
	SubagentID string

	*requestState
	respond func(context.Context, PermissionResponse) error
}

// newPermissionRequest attaches a responder to req.
func newPermissionRequest(req PermissionRequest, respond func(context.Context, PermissionResponse) error) *PermissionRequest {
	req.requestState = newRequestState()
	req.respond = respond
	return &req
}

// RequestID returns r.ID.
func (r *PermissionRequest) RequestID() string { return r.ID }

// Respond sends resp to the harness.
func (r *PermissionRequest) Respond(ctx context.Context, resp PermissionResponse) error {
	if resp.ProviderResponse == nil {
		switch resp.Decision {
		case DecisionAllow, DecisionAllowSession, DecisionAllowAlways, DecisionDeny, DecisionCancel:
		default:
			return fmt.Errorf("%w: unknown permission decision %q", ErrInvalidAnswer, resp.Decision)
		}
	}
	return r.answer(func() error { return r.respond(ctx, resp) })
}

// Allow allows this one call.
func (r *PermissionRequest) Allow(ctx context.Context) error {
	return r.Respond(ctx, PermissionResponse{Decision: DecisionAllow})
}

// AllowForSession allows this call and similar calls for the session.
func (r *PermissionRequest) AllowForSession(ctx context.Context) error {
	return r.Respond(ctx, PermissionResponse{Decision: DecisionAllowSession})
}

// AllowAlways allows this call and stores a rule so similar calls are
// allowed in later sessions too, where the harness supports it.
func (r *PermissionRequest) AllowAlways(ctx context.Context) error {
	return r.Respond(ctx, PermissionResponse{Decision: DecisionAllowAlways})
}

// Deny refuses the call and tells the model why.
func (r *PermissionRequest) Deny(ctx context.Context, message string) error {
	return r.Respond(ctx, PermissionResponse{Decision: DecisionDeny, Message: message})
}

// Cancel refuses the call and stops the turn.
func (r *PermissionRequest) Cancel(ctx context.Context) error {
	return r.Respond(ctx, PermissionResponse{Decision: DecisionCancel})
}

// QuestionOption is one choice of a Question.
type QuestionOption struct {
	Label       string
	Description string
	// Preview is optional rich content shown when the option is focused.
	Preview string
}

// Question is one question of a UserInputRequest.
type Question struct {
	// ID keys the answer in UserInputResponse.Answers.
	ID string
	// Header is a short label for the question.
	Header string
	Text   string
	// Options are the offered choices. A question without options takes
	// free text.
	Options []QuestionOption
	// MultiSelect allows choosing several options.
	MultiSelect bool
	// AllowOther allows a free-text answer besides the options.
	AllowOther bool
	// Secret asks the UI to hide the typed answer.
	Secret bool
	// Multiline asks for a multi-line editor.
	Multiline bool
	// Placeholder or prefilled text for free-text answers.
	Placeholder string
	Default     string
}

// UserInputResponse answers a UserInputRequest.
type UserInputResponse struct {
	// Answers maps Question.ID to the chosen option labels or typed text.
	Answers map[string][]string
	// Cancelled declines to answer.
	Cancelled bool
}

// UserInputRequest asks the user one or more questions.
type UserInputRequest struct {
	EventMeta
	ID        string
	Title     string
	Questions []Question
	// ToolCallID is the tool call asking, when known.
	ToolCallID string
	// Async reports questions the agent asked without waiting, such as
	// ones in a finished reply; the answer arrives as a new message.
	Async bool
	// SubagentID is the subagent asking, if any.
	SubagentID string

	*requestState
	respond func(context.Context, UserInputResponse) error
}

// newUserInputRequest attaches a responder to req.
func newUserInputRequest(req UserInputRequest, respond func(context.Context, UserInputResponse) error) *UserInputRequest {
	req.requestState = newRequestState()
	req.respond = respond
	return &req
}

// RequestID returns r.ID.
func (r *UserInputRequest) RequestID() string { return r.ID }

// Respond sends resp to the harness.
func (r *UserInputRequest) Respond(ctx context.Context, resp UserInputResponse) error {
	return r.answer(func() error { return r.respond(ctx, resp) })
}

// Answer answers the questions. Keys are Question.ID values.
func (r *UserInputRequest) Answer(ctx context.Context, answers map[string][]string) error {
	return r.Respond(ctx, UserInputResponse{Answers: answers})
}

// Cancel declines to answer.
func (r *UserInputRequest) Cancel(ctx context.Context) error {
	return r.Respond(ctx, UserInputResponse{Cancelled: true})
}

// ElicitationAction is the answer to an MCP elicitation.
type ElicitationAction string

// Elicitation actions.
const (
	ElicitationAccept  ElicitationAction = "accept"
	ElicitationDecline ElicitationAction = "decline"
	ElicitationCancel  ElicitationAction = "cancel"
)

// ElicitationResponse answers an ElicitationRequest.
type ElicitationResponse struct {
	Action ElicitationAction
	// Content holds form values matching the requested schema.
	Content json.RawMessage
	// MCPMeta is the MCP _meta object of the reply, such as a request to
	// remember the answer.
	MCPMeta json.RawMessage
}

// ElicitationRequest is an MCP server asking the user for input, either a
// form described by a JSON Schema or a URL to open.
type ElicitationRequest struct {
	EventMeta
	ID string
	// Server is the MCP server asking.
	Server  string
	Message string
	// Mode is "form" or "url", or a harness-specific mode.
	Mode string
	// Schema is the requested form's JSON Schema in form mode.
	Schema json.RawMessage
	// URL is the page to open in url mode.
	URL string
	// ElicitationID identifies a url-mode elicitation.
	ElicitationID string
	// MCPMeta is the MCP _meta object of the request.
	MCPMeta json.RawMessage
	// Title and Description label the request for display, when the
	// harness provides them.
	Title       string
	Description string
	// SubagentID is the subagent whose MCP call asked, if any and known.
	SubagentID string

	*requestState
	respond func(context.Context, ElicitationResponse) error
}

// newElicitationRequest attaches a responder to req.
func newElicitationRequest(req ElicitationRequest, respond func(context.Context, ElicitationResponse) error) *ElicitationRequest {
	req.requestState = newRequestState()
	req.respond = respond
	return &req
}

// RequestID returns r.ID.
func (r *ElicitationRequest) RequestID() string { return r.ID }

// Respond sends resp to the harness.
func (r *ElicitationRequest) Respond(ctx context.Context, resp ElicitationResponse) error {
	switch resp.Action {
	case ElicitationAccept, ElicitationDecline, ElicitationCancel:
	default:
		return fmt.Errorf("%w: unknown elicitation action %q", ErrInvalidAnswer, resp.Action)
	}
	return r.answer(func() error { return r.respond(ctx, resp) })
}

// Accept submits form content, or confirms a url-mode elicitation with nil
// content.
func (r *ElicitationRequest) Accept(ctx context.Context, content json.RawMessage) error {
	return r.Respond(ctx, ElicitationResponse{Action: ElicitationAccept, Content: content})
}

// Decline refuses the elicitation.
func (r *ElicitationRequest) Decline(ctx context.Context) error {
	return r.Respond(ctx, ElicitationResponse{Action: ElicitationDecline})
}

// Cancel dismisses the elicitation.
func (r *ElicitationRequest) Cancel(ctx context.Context) error {
	return r.Respond(ctx, ElicitationResponse{Action: ElicitationCancel})
}
