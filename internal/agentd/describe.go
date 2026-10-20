package agentd

import (
	"encoding/json"
	"reflect"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
)

// ProtocolDescription lists the protocol's messages with their Go types,
// for the schema generator.
type ProtocolDescription struct {
	// Methods are the requests the client sends, by method name.
	Methods map[string]MessageDescription
	// Notifications are the notifications agentd sends, by method name.
	Notifications map[string]MessageDescription
	// ClientNotifications are the notifications agentd accepts from the
	// client, by method name.
	ClientNotifications map[string]MessageDescription
	// ReverseRequests are the requests agentd sends to the client, by
	// method name. Result is the type of the client's reply.
	ReverseRequests map[string]MessageDescription
}

// MessageDescription gives a message's params and result types. A nil
// Result means the reply is an empty object; notifications have none.
type MessageDescription struct {
	Params reflect.Type
	Result reflect.Type
}

// CancelRequestParams are the params of $/cancelRequest, which agentd
// sends when it stops waiting for a reverse request, and which the client
// sends to stop one of its own requests.
type CancelRequestParams struct {
	// ID is the JSON-RPC ID of the abandoned request.
	ID int64 `json:"id"`
}

// Protocol describes every method, notification, and reverse request.
// TestProtocolMethodsMatchDispatch keeps Methods in step with the
// dispatch table.
func Protocol() ProtocolDescription {
	return ProtocolDescription{
		Methods: map[string]MessageDescription{
			"initialize":                describe[InitializeParams, InitializeResult](),
			pingMethod:                  describeNoResult[struct{}](),
			"provider/check":            describe[ProviderParams, ProviderStatusWire](),
			"provider/listModels":       describe[ProviderParams, []agent.Model](),
			"provider/listSessions":     describe[ListSessionsParams, []agent.SessionSummary](),
			"provider/readHistory":      describe[ReadHistoryParams, []agent.Item](),
			"provider/deleteSession":    describeNoResult[DeleteSessionParams](),
			"provider/listSubagents":    describe[ListSubagentsParams, []agent.Task](),
			"session/start":             describe[StartParams, StartResult](),
			"session/fork":              describe[ForkParams, StartResult](),
			"session/send":              describe[SendParams, SendResult](),
			"session/info":              describe[SessionParams, agent.SessionInfo](),
			"session/interrupt":         describeNoResult[SessionParams](),
			"session/interruptTurn":     describeNoResult[TurnParams](),
			"session/setModel":          describeNoResult[ValueParams](),
			"session/setEffort":         describeNoResult[ValueParams](),
			"session/setPermissionMode": describeNoResult[ValueParams](),
			"session/setTitle":          describeNoResult[ValueParams](),
			"session/compact":           describeNoResult[CompactParams](),
			"session/rewind":            describeNoResult[TurnParams](),
			"session/stopTask":          describeNoResult[StopTaskParams](),
			"session/rateLimits":        describe[SessionParams, []agent.RateLimit](),
			"session/pendingRequests":   describe[SessionParams, []PendingRequest](),
			"session/close":             describeNoResult[SessionParams](),
			"request/respond":           describeNoResult[RespondParams](),
			"request/cancel":            describeNoResult[RequestParams](),
		},
		Notifications: map[string]MessageDescription{
			"session/event":  describeNoResult[EventNotification](),
			"turn/result":    describeNoResult[TurnResultNotification](),
			"session/closed": describeNoResult[SessionClosedNotification](),
			"wire/tap":       describeNoResult[WireTapNotification](),
			cancelMethod:     describeNoResult[CancelRequestParams](),
		},
		ClientNotifications: map[string]MessageDescription{
			cancelMethod: describeNoResult[CancelRequestParams](),
		},
		ReverseRequests: map[string]MessageDescription{
			"handler/permission":      describe[PermissionCall, agent.PermissionResponse](),
			"handler/userInput":       describe[UserInputCall, agent.UserInputResponse](),
			"handler/elicitation":     describe[ElicitationCall, agent.ElicitationResponse](),
			"tool/call":               describe[ToolCall, agent.ToolResult](),
			"claude/userDialog":       describe[UserDialogCall, json.RawMessage](),
			"claude/controlRequest":   describe[ControlRequestCall, json.RawMessage](),
			"codex/authTokensRefresh": describe[AuthTokensRefreshCall, json.RawMessage](),
			"claude/hook":             describe[HookCall, claude.HookOutput](),
		},
	}
}

func describe[P, R any]() MessageDescription {
	return MessageDescription{Params: reflect.TypeFor[P](), Result: reflect.TypeFor[R]()}
}

func describeNoResult[P any]() MessageDescription {
	return MessageDescription{Params: reflect.TypeFor[P]()}
}
