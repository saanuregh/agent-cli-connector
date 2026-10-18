package claude

import (
	"encoding/json"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Claude-specific events. They are delivered on Session.Events next to the
// uniform events.

// CommandLifecycle reports a submitted message moving through the CLI's
// command queue: "queued", "started", and later states.
type CommandLifecycle struct {
	agent.EventMeta
	CommandUUID string
	State       string
}

// PromptSuggestion is a predicted next user prompt, emitted after a turn
// when SessionOptions.PromptSuggestions is set.
type PromptSuggestion struct {
	agent.EventMeta
	Suggestion string
}

// ToolProgress is a heartbeat for a long-running tool call.
type ToolProgress struct {
	agent.EventMeta
	ToolUseID string
	ToolName  string
	// ParentToolUseID is the subagent's tool call when a subagent runs the
	// tool.
	ParentToolUseID string
	Elapsed         time.Duration
	TaskID          string
}

// ToolUseSummary is a generated one-line summary of preceding tool calls.
type ToolUseSummary struct {
	agent.EventMeta
	Summary    string
	ToolUseIDs []string
}

// BackgroundTasksChanged carries the full set of live background tasks.
// Replace any cached set with Tasks.
type BackgroundTasksChanged struct {
	agent.EventMeta
	Tasks []BackgroundTask
}

// BackgroundTask is one live background task.
type BackgroundTask struct {
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	TaskType     string `json:"task_type"`
	SubagentType string `json:"subagent_type"`
	Description  string `json:"description"`
	ParentTaskID string `json:"parent_task_id"`
	Ambient      bool   `json:"ambient"`
}

// DeferredToolUse reports the tool call a PreToolUse hook deferred with
// permissionDecision "defer". It arrives just before the TurnCompleted
// event of the turn it ended, whose StopReason is "tool_deferred". The call
// did not run.
type DeferredToolUse struct {
	agent.EventMeta
	ToolUseID string
	ToolName  string
	Input     json.RawMessage
}
