package agentd

import (
	"encoding/json"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
)

// ProtocolVersion is the version initialize reports. It changes when a
// change would break an existing client.
const ProtocolVersion = 1

// The protocol uses Go field names, the names the library's types encode
// with, so the envelope types below have no json tags either.

// InitializeParams are the params of initialize, which must be the first
// request.
type InitializeParams struct {
	ProtocolVersion int
	// IncludeRaw keeps the native Raw messages in events and results.
	IncludeRaw bool
	// Providers configures the providers to offer, by name. Empty offers
	// every provider with its default options.
	Providers map[string]ProviderInit
}

// ProviderInit configures one provider.
type ProviderInit struct {
	// Options is the provider's Options, such as claude.Options, as JSON.
	Options json.RawMessage
	// OnAuthTokensRefresh forwards Codex's auth token refresh requests to
	// the client as codex/authTokensRefresh.
	OnAuthTokensRefresh bool
	// WireTap sends the provider's protocol traffic as wire/tap
	// notifications.
	WireTap bool
	// Spawner chooses where the harness runs; nil runs it locally.
	Spawner *SpawnerConfig
}

// InitializeResult is the result of initialize.
type InitializeResult struct {
	ProtocolVersion int
	Providers       []ProviderInfo
}

// ProviderInfo describes an offered provider.
type ProviderInfo struct {
	Name         string
	Capabilities agent.Capabilities
	// Methods lists the provider/* methods this provider implements; the
	// others fail with Kind unsupported. A listed method can still fail,
	// such as for a session the harness does not have.
	Methods []string
}

// ProviderParams name a provider.
type ProviderParams struct {
	Provider string
}

// ListSessionsParams are the params of provider/listSessions.
type ListSessionsParams struct {
	Provider string
	Options  agent.ListSessionsOptions
}

// ReadHistoryParams are the params of provider/readHistory.
type ReadHistoryParams struct {
	Provider  string
	SessionID string
	Options   agent.ReadHistoryOptions
}

// DeleteSessionParams are the params of provider/deleteSession.
type DeleteSessionParams struct {
	Provider  string
	SessionID string
	Cwd       string
}

// ListSubagentsParams are the params of provider/listSubagents.
type ListSubagentsParams struct {
	Provider  string
	SessionID string
	Options   agent.ListSubagentsOptions
}

// StartParams are the params of session/start.
type StartParams struct {
	Provider string
	// Config is the session config. Its Tools arrive without handlers:
	// agentd runs each through tool/call. Config.ProviderOptions must be
	// empty; use ProviderOptions.
	Config agent.Config
	// ProviderOptions is the provider's SessionOptions, such as
	// claude.SessionOptions, as JSON.
	ProviderOptions json.RawMessage
	// Handlers names the requests the client answers through handler/*
	// requests. The others arrive as session/event notifications and are
	// answered with request/respond.
	Handlers HandlerFlags
	// WireTap sends the session's protocol traffic as wire/tap
	// notifications.
	WireTap bool
	// OnUserDialog and OnControlRequest forward Claude's dialogs and
	// unknown control requests to the client as claude/userDialog and
	// claude/controlRequest.
	OnUserDialog     bool
	OnControlRequest bool
}

// HandlerFlags names the request kinds the client handles.
type HandlerFlags struct {
	Permission  bool
	UserInput   bool
	Elicitation bool
}

// StartResult is the result of session/start and session/fork.
type StartResult struct {
	// Session is agentd's handle for the session, used by every
	// session/* method. It stays the same when the native ID changes.
	Session string
	Info    agent.SessionInfo
	// Methods lists the session/* and request/* methods this session
	// implements; the others fail with Kind unsupported. A listed method
	// can still be refused for this session, such as forking an ephemeral
	// one.
	Methods []string
}

// ForkParams are the params of session/fork. Start.Provider is ignored.
type ForkParams struct {
	Session string
	Start   StartParams
}

// SessionParams name a session.
type SessionParams struct {
	Session string
}

// SendParams are the params of session/send.
type SendParams struct {
	Session string
	Input   agent.Input
	// ProviderOptions is the provider's TurnOptions, such as
	// claude.TurnOptions, as JSON.
	ProviderOptions json.RawMessage
}

// SendResult is the result of session/send.
type SendResult struct {
	TurnID string
}

// TurnParams name a turn of a session.
type TurnParams struct {
	Session string
	TurnID  string
}

// ValueParams set one session setting, such as the model.
type ValueParams struct {
	Session string
	Value   string
}

// CompactParams are the params of session/compact.
type CompactParams struct {
	Session      string
	Instructions string
}

// StopTaskParams are the params of session/stopTask.
type StopTaskParams struct {
	Session string
	TaskID  string
}

// RespondParams are the params of request/respond. Exactly one response
// is set, matching the request's type.
type RespondParams struct {
	Session     string
	RequestID   string
	Permission  *agent.PermissionResponse
	UserInput   *agent.UserInputResponse
	Elicitation *agent.ElicitationResponse
}

// RequestParams name a pending request of a session.
type RequestParams struct {
	Session   string
	RequestID string
}

// PendingRequest is one request that waits for an answer, as
// session/pendingRequests returns it.
type PendingRequest struct {
	// Type is the request's event type name, such as "PermissionRequest".
	Type    string
	Request json.RawMessage
}

// EventNotification is a session/event notification.
type EventNotification struct {
	Session string
	// Seq numbers the session's events from 1.
	Seq int64
	// Type is the event's type name, as agent.EventType returns it.
	Type  string
	Event json.RawMessage
}

// TurnResultNotification is a turn/result notification, sent when a turn
// sent with session/send ends.
type TurnResultNotification struct {
	Session string
	// TurnID is the ID session/send returned. Result.TurnID is the final
	// one, which differs when the harness renamed the turn.
	TurnID string
	Result *TurnResultWire
	// Error is set when the turn has no result, such as when the session
	// closed first.
	Error *ErrorInfo
}

// SessionClosedNotification is a session/closed notification.
type SessionClosedNotification struct {
	Session string
	Error   *ErrorInfo
}

// WireTapNotification is a wire/tap notification.
type WireTapNotification struct {
	// Session is empty for provider-level traffic, such as a shared Codex
	// app server's.
	Session   string
	Provider  string
	Direction string
	Line      string
}

// ErrorInfo describes an error. It is the data of every JSON-RPC error
// agentd returns.
type ErrorInfo struct {
	Message string
	// Kind classifies the error: unsupported, not_found, invalid_params,
	// not_initialized, unknown_method, parse_error, protocol, exit, closed,
	// interrupted, cancelled, session_not_found, request_done,
	// invalid_answer, steer_rejected, or empty.
	Kind string `json:",omitempty"`
	// Turn holds the details of a turn's failure.
	Turn *TurnErrorWire `json:",omitempty"`
	// Protocol holds the harness's error answer, such as its error code.
	Protocol *agent.ProtocolError `json:",omitempty"`
	// Exit describes a harness process that ended unexpectedly.
	Exit *ExitErrorWire `json:",omitempty"`
}

// ExitErrorWire is agent.ExitError with Err as an ErrorInfo.
type ExitErrorWire struct {
	*agent.ExitError
	Err *ErrorInfo
}

// TurnResultWire is agent.TurnResult with Err as an ErrorInfo.
type TurnResultWire struct {
	*agent.TurnResult
	Err *ErrorInfo
}

// TurnErrorWire is agent.TurnError with Err as an ErrorInfo.
type TurnErrorWire struct {
	*agent.TurnError
	Err *ErrorInfo
}

// TurnCompletedWire is agent.TurnCompleted with its result in wire form.
type TurnCompletedWire struct {
	*agent.TurnCompleted
	Result *TurnResultWire
}

// ProviderStatusWire is agent.ProviderStatus with Err as an ErrorInfo.
type ProviderStatusWire struct {
	*agent.ProviderStatus
	Err *ErrorInfo
}

// Reverse requests, from agentd to the client.

// PermissionCall is the params of handler/permission; the result is an
// agent.PermissionResponse.
type PermissionCall struct {
	Session string
	Request *agent.PermissionRequest
}

// UserInputCall is the params of handler/userInput; the result is an
// agent.UserInputResponse.
type UserInputCall struct {
	Session string
	Request *agent.UserInputRequest
}

// ElicitationCall is the params of handler/elicitation; the result is an
// agent.ElicitationResponse.
type ElicitationCall struct {
	Session string
	Request *agent.ElicitationRequest
}

// ToolCall is the params of tool/call; the result is an agent.ToolResult.
type ToolCall struct {
	Session string
	Tool    string
	Call    agent.ToolCallRequest
}

// UserDialogCall is the params of claude/userDialog; the result is the
// dialog's answer as JSON.
type UserDialogCall struct {
	Session string
	Dialog  claude.UserDialog
}

// ControlRequestCall is the params of claude/controlRequest; the result is
// the answer as JSON.
type ControlRequestCall struct {
	Session string
	Subtype string
	Request json.RawMessage
}

// HookCall is the params of claude/hook, sent for each matcher in Claude's
// SessionOptions.Hooks; the result is a claude.HookOutput. An error reply
// is no decision.
type HookCall struct {
	Session string
	Event   claude.HookEvent
	// Matcher and Index identify the matcher: Hooks[Event][Index].
	Matcher string
	Index   int
	// Payload is the hook's native input as Claude Code sends it, with
	// snake_case fields such as tool_name and tool_input; see
	// claude.HookInput for the common ones.
	Payload json.RawMessage
	// ToolUseID is the tool_use ID the CLI passed beside the input.
	ToolUseID string
}

// AuthTokensRefreshCall is the params of codex/authTokensRefresh; the
// result is the answer as JSON.
type AuthTokensRefreshCall struct {
	Provider string
	Params   json.RawMessage
}
