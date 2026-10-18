package claude

import (
	"context"
	"encoding/json"
	"time"
)

// HookEvent names a Claude Code hook event.
type HookEvent string

// Hook events. Claude Code may add more; any string is accepted.
const (
	HookPreToolUse         HookEvent = "PreToolUse"
	HookPostToolUse        HookEvent = "PostToolUse"
	HookPostToolUseFailure HookEvent = "PostToolUseFailure"
	HookPostToolBatch      HookEvent = "PostToolBatch"
	HookNotification       HookEvent = "Notification"
	HookUserPromptSubmit   HookEvent = "UserPromptSubmit"
	HookUserPromptExpand   HookEvent = "UserPromptExpansion"
	HookSessionStart       HookEvent = "SessionStart"
	HookSessionEnd         HookEvent = "SessionEnd"
	HookStop               HookEvent = "Stop"
	HookStopFailure        HookEvent = "StopFailure"
	HookSubagentStart      HookEvent = "SubagentStart"
	HookSubagentStop       HookEvent = "SubagentStop"
	HookPreCompact         HookEvent = "PreCompact"
	HookPostCompact        HookEvent = "PostCompact"
	HookPreModelSwitch     HookEvent = "PreModelSwitch"
	HookPostModelSwitch    HookEvent = "PostModelSwitch"
	HookPermissionRequest  HookEvent = "PermissionRequest"
	HookPermissionDenied   HookEvent = "PermissionDenied"
	HookSetup              HookEvent = "Setup"
	HookTeammateIdle       HookEvent = "TeammateIdle"
	HookTaskCreated        HookEvent = "TaskCreated"
	HookTaskCompleted      HookEvent = "TaskCompleted"
	HookElicitation        HookEvent = "Elicitation"
	HookElicitationResult  HookEvent = "ElicitationResult"
	HookConfigChange       HookEvent = "ConfigChange"
	HookWorktreeCreate     HookEvent = "WorktreeCreate"
	HookWorktreeRemove     HookEvent = "WorktreeRemove"
	HookInstructionsLoaded HookEvent = "InstructionsLoaded"
	HookCwdChanged         HookEvent = "CwdChanged"
	HookFileChanged        HookEvent = "FileChanged"
	HookDirectoryAdded     HookEvent = "DirectoryAdded"
	HookMessageDisplay     HookEvent = "MessageDisplay"
)

// HookMatcher registers callbacks for one hook event.
type HookMatcher struct {
	// Matcher filters by tool name or other event-specific key, such as
	// "Bash" or "Edit|Write". Empty matches everything.
	Matcher string
	// Timeout bounds each callback. Zero means the CLI default.
	Timeout time.Duration
	// Hooks run in order.
	Hooks []HookFunc
}

// HookFunc handles one hook invocation.
type HookFunc func(ctx context.Context, in HookInput) (HookOutput, error)

// HookInput is the payload of a hook invocation. The common fields are
// decoded; Raw holds everything, including event-specific fields.
type HookInput struct {
	Event          HookEvent       `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	AgentID        string          `json:"agent_id,omitempty"`
	AgentType      string          `json:"agent_type,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage `json:"tool_response,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	// Effort is the effort level the agent runs at, after any downgrade
	// for the model.
	Effort *HookEffort `json:"effort,omitempty"`
	// ToolUseIDArg is the tool_use ID the CLI passed beside the input.
	ToolUseIDArg string          `json:"-"`
	Raw          json.RawMessage `json:"-"`
}

// HookEffort is the effort a hook input reports.
type HookEffort struct {
	Level string `json:"level"`
}

// Field decodes one event-specific field of the raw input into v.
func (in HookInput) Field(name string, v any) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(in.Raw, &m); err != nil {
		return err
	}
	raw, ok := m[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// HookOutput is a hook's answer. The zero value continues normally.
type HookOutput struct {
	// Async tells the CLI the hook continues in the background.
	Async        bool `json:"async,omitempty"`
	AsyncTimeout int  `json:"asyncTimeout,omitempty"`

	// Continue false stops the session after the hook; StopReason says
	// why.
	Continue       *bool  `json:"continue,omitempty"`
	StopReason     string `json:"stopReason,omitempty"`
	SuppressOutput bool   `json:"suppressOutput,omitempty"`
	// Decision is "approve" or "block" for events that accept one.
	Decision      string `json:"decision,omitempty"`
	Reason        string `json:"reason,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
	// Specific is the event-specific output, such as
	// PreToolUseOutput. Its hookEventName is filled in.
	Specific any `json:"hookSpecificOutput,omitempty"`
}

// PreToolUseOutput is the event-specific output of a PreToolUse hook.
type PreToolUseOutput struct {
	HookEventName string `json:"hookEventName"`
	// PermissionDecision is "allow", "deny", "ask", or "defer".
	PermissionDecision       string          `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             json.RawMessage `json:"updatedInput,omitempty"`
	AdditionalContext        string          `json:"additionalContext,omitempty"`
}

// ContextOutput is the event-specific output of hooks that add context,
// such as UserPromptSubmit, SessionStart, and PostToolUse.
type ContextOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

func (o HookOutput) encode(event HookEvent) (json.RawMessage, error) {
	switch s := o.Specific.(type) {
	case PreToolUseOutput:
		if s.HookEventName == "" {
			s.HookEventName = string(event)
		}
		o.Specific = s
	case ContextOutput:
		if s.HookEventName == "" {
			s.HookEventName = string(event)
		}
		o.Specific = s
	case map[string]any:
		if _, ok := s["hookEventName"]; !ok {
			s["hookEventName"] = string(event)
		}
	}
	return json.Marshal(o)
}
