package agent

import (
	"encoding/json"
	"reflect"
	"time"
)

// Event is a value delivered by Session.Events. Switch on the concrete type:
//
//	for ev := range sess.Events() {
//		switch ev := ev.(type) {
//		case *agent.TextDelta:
//			fmt.Print(ev.Delta)
//		case *agent.PermissionRequest:
//			ev.Allow(ctx)
//		case *agent.TurnCompleted:
//			fmt.Println(ev.Result.Status)
//		}
//	}
//
// Provider packages define more event types for native features without a
// uniform meaning. Any type that embeds EventMeta is an Event.
type Event interface {
	Meta() *EventMeta
}

// EventMeta holds the fields every event carries.
type EventMeta struct {
	// Provider is the provider name, such as "claude".
	Provider string
	// SessionID is the native session ID.
	SessionID string
	// TurnID is the turn the event belongs to, or empty for events outside
	// a turn.
	TurnID string
	// Time is when the library received or produced the event.
	Time time.Time
	// Raw is the native message the event was produced from, or nil for
	// events the library produced itself.
	Raw json.RawMessage
}

// Meta returns m. It makes every type embedding EventMeta an Event.
func (m *EventMeta) Meta() *EventMeta { return m }

// EventType returns the name of ev's concrete type without its package,
// such as "TaskUpdated", for logging and serialization. It works for the
// event types of provider packages too, and returns "" for a nil ev.
func EventType(ev Event) string {
	t := reflect.TypeOf(ev)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return ""
	}
	return t.Name()
}

// SessionStarted is the first event of a session.
type SessionStarted struct {
	EventMeta
	Info SessionInfo
	// Resumed reports that the session continues a persisted one.
	Resumed bool
}

// SessionUpdated reports changed session metadata such as the model or
// permission mode. Info is a full snapshot.
type SessionUpdated struct {
	EventMeta
	Info SessionInfo
}

// SessionStatus is the activity state of a session.
type SessionStatus string

// Session states.
const (
	StatusIdle       SessionStatus = "idle"
	StatusRunning    SessionStatus = "running"
	StatusCompacting SessionStatus = "compacting"
	// StatusWaiting means the harness waits for an answer to a request.
	StatusWaiting SessionStatus = "waiting"
)

// StatusChanged reports a change of session activity.
type StatusChanged struct {
	EventMeta
	Status SessionStatus
}

// TurnStarted reports that the harness started working on input.
type TurnStarted struct {
	EventMeta
}

// TurnCompleted reports the outcome of a turn. It is the last event of the
// turn, except for informational events such as TaskUpdated.
type TurnCompleted struct {
	EventMeta
	Result *TurnResult
}

// ItemStarted reports a new transcript item.
type ItemStarted struct {
	EventMeta
	Item Item
}

// ItemUpdated reports a changed transcript item, such as a tool call that
// received its full arguments.
type ItemUpdated struct {
	EventMeta
	Item Item
}

// ItemCompleted reports a finished transcript item. Its Item is
// authoritative and replaces anything assembled from deltas.
type ItemCompleted struct {
	EventMeta
	Item Item
}

// TextDelta is a piece of assistant text.
type TextDelta struct {
	EventMeta
	ItemID string
	// ParentID and SubagentID are set when a subagent produces the text;
	// see Item.
	ParentID   string
	SubagentID string
	Delta      string
}

// ReasoningDelta is a piece of model reasoning.
type ReasoningDelta struct {
	EventMeta
	ItemID     string
	ParentID   string
	SubagentID string
	Delta      string
	// Summary reports that the text is a reasoning summary rather than
	// raw reasoning.
	Summary bool
}

// ToolInputDelta is a piece of a tool call's JSON arguments while the model
// writes them.
type ToolInputDelta struct {
	EventMeta
	ItemID     string
	ParentID   string
	SubagentID string
	Delta      string
}

// ToolOutputDelta is a piece of a running tool's output.
type ToolOutputDelta struct {
	EventMeta
	ItemID string
	Delta  string
}

// PlanUpdated reports the agent's current task list.
type PlanUpdated struct {
	EventMeta
	Steps []PlanStep
	// Explanation is an optional note about the change.
	Explanation string
	// ItemID is the tool call that changed the plan, when there is one.
	ItemID string
}

// UsageUpdated reports token use while a turn runs.
type UsageUpdated struct {
	EventMeta
	Usage Usage
}

// TaskUpdated reports a background task or subagent changing state. It
// can arrive outside a turn: a background subagent often finishes after
// the turn that started it.
type TaskUpdated struct {
	EventMeta
	Task Task
}

// Compaction reports conversation compaction. Done is false when
// compaction starts and true when it ends.
type Compaction struct {
	EventMeta
	Done bool
	// Trigger is "manual", "auto", or a harness-specific reason.
	Trigger      string
	TokensBefore int64
	TokensAfter  int64
	Summary      string
	// Error is set when compaction failed.
	Error string
}

// Retry reports that the harness retries a failed model request.
type Retry struct {
	EventMeta
	Attempt     int
	MaxAttempts int
	Delay       time.Duration
	Message     string
	// Code is the error class being retried, such as "rate_limit" or
	// "overloaded"; HTTPStatus is the failed response status. Both are
	// empty when unknown.
	Code       string
	HTTPStatus int
	// Done reports that retrying finished and the request went through.
	Done bool
}

// NoticeLevel is the severity of a Notice.
type NoticeLevel string

// Notice severities.
const (
	NoticeInfo    NoticeLevel = "info"
	NoticeWarning NoticeLevel = "warning"
	NoticeError   NoticeLevel = "error"
)

// Notice is an informational message from the harness, such as a warning,
// a rate-limit notice, or a non-fatal error.
type Notice struct {
	EventMeta
	Level NoticeLevel
	// Code is a harness-specific identifier, when there is one.
	Code    string
	Message string
}

// RequestResolved reports that a request is no longer pending, because it
// was answered or the harness withdrew it.
type RequestResolved struct {
	EventMeta
	Request Request
	// Outcome is RequestAnswered or RequestWithdrawn.
	Outcome RequestOutcome
}

// RateLimitStatus summarizes a rate-limit window.
type RateLimitStatus string

// Rate-limit states.
const (
	RateLimitOK       RateLimitStatus = "ok"
	RateLimitWarning  RateLimitStatus = "warning"
	RateLimitExceeded RateLimitStatus = "exceeded"
)

// RateLimit is one usage window of the account's plan.
type RateLimit struct {
	// Name identifies the window, such as "five_hour", "seven_day",
	// "primary", or "secondary".
	Name string
	// UsedPercent is the share of the window used, 0-100.
	UsedPercent float64
	// Window is the window length when known.
	Window time.Duration
	// ResetsAt is when the window resets; zero when unknown.
	ResetsAt time.Time
	Status   RateLimitStatus
	// LimitID names the allowance the window belongs to when an account
	// has several, such as a model-specific one.
	LimitID string
	// Overage reports that usage beyond the plan is being drawn on.
	Overage bool
}

// RateLimitUpdated reports the account's plan usage windows.
type RateLimitUpdated struct {
	EventMeta
	Limits []RateLimit
}

// HookRun reports a configured harness hook starting, making progress, or
// finishing.
type HookRun struct {
	EventMeta
	ID string
	// Event is the hook event, such as "PreToolUse" or "preToolUse".
	Event string
	Name  string
	// Done is false when the hook starts or reports progress.
	Done bool
	// Status is the native outcome once done, such as "success",
	// "completed", "failed", or "blocked".
	Status string
	// Outcome is the normalized outcome once done. It is empty while the
	// hook runs or when the native Status is unknown.
	Outcome HookOutcome
	Output  string
}

// HookOutcome is the normalized result of a finished hook.
type HookOutcome string

// Hook outcomes.
const (
	HookSucceeded HookOutcome = "succeeded"
	HookFailed    HookOutcome = "failed"
	// HookBlocked means the hook stopped the action it ran for.
	HookBlocked HookOutcome = "blocked"
)

// Goal is a standing objective the agent works toward across turns.
type Goal struct {
	Objective string
	// Status is the native goal state, such as "active" or "complete".
	Status      string
	TokensUsed  int64
	TokenBudget int64
	// Iterations counts turns spent on the goal, when reported.
	Iterations int
	// TimeUsed is the time spent on the goal, when reported.
	TimeUsed time.Duration
}

// GoalUpdated reports the session goal changing. Goal is nil when the
// goal was cleared.
type GoalUpdated struct {
	EventMeta
	Goal *Goal
}

// ConversationReset reports that the harness started over with an empty
// conversation, such as after a /clear command. Earlier items no longer
// belong to the conversation; Transcript drops them.
type ConversationReset struct {
	EventMeta
	// Trigger is the native reason, such as "clear".
	Trigger string
	// NewSessionID is the session's new ID when it changed.
	NewSessionID string
}

// PlanDelta streams the text of a plan item, such as a plan written in
// plan mode.
type PlanDelta struct {
	EventMeta
	ItemID string
	Delta  string
}

// ItemsRemoved reports that the harness withdrew items it emitted, such as
// a reply retracted when the model fell back to another model.
type ItemsRemoved struct {
	EventMeta
	IDs []string
}

// EventsDropped reports that unread events were discarded because
// Config.EventBufferLimit was reached.
type EventsDropped struct {
	EventMeta
	Count int
}

// RawEvent carries a native message without a uniform meaning. Type is the
// native message type, such as "system/hook_started" or
// "thread/goal/updated"; the message is in Raw.
type RawEvent struct {
	EventMeta
	Type string
}
