package agent

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"
)

// ItemKind names a kind of transcript item.
type ItemKind string

// Transcript item kinds.
const (
	ItemUserMessage      ItemKind = "user_message"
	ItemAssistantMessage ItemKind = "assistant_message"
	ItemReasoning        ItemKind = "reasoning"
	ItemToolCall         ItemKind = "tool_call"
	ItemPlan             ItemKind = "plan"
	ItemCompaction       ItemKind = "compaction"
	ItemError            ItemKind = "error"
	// ItemOther covers native items without a uniform kind; read Raw.
	ItemOther ItemKind = "other"
)

// ItemStatus is the lifecycle state of an item.
type ItemStatus string

// Item states.
const (
	ItemRunning ItemStatus = "in_progress"
	ItemDone    ItemStatus = "completed"
	ItemFailed  ItemStatus = "failed"
	// ItemDeclined marks a tool call the user or a policy refused.
	ItemDeclined ItemStatus = "declined"
	// ItemCancelled marks an item stopped by an interrupt.
	ItemCancelled ItemStatus = "cancelled"
)

// Item is one entry of a conversation transcript: a message, a reasoning
// block, a tool call, a plan, and so on. Items are reported through
// ItemStarted, ItemUpdated, and ItemCompleted events; the completed item is
// authoritative.
type Item struct {
	// ID identifies the item within the session.
	ID     string
	Kind   ItemKind
	Status ItemStatus
	// ParentID is the item that spawned the subagent producing this item,
	// such as the Agent tool call, or empty for the main conversation.
	ParentID string
	// SubagentID is the subagent producing this item: the ID of its Task.
	// It is empty for the main conversation.
	SubagentID string
	// Text holds the content of message, reasoning, plan, and error items.
	Text string
	// Tool describes a tool call item.
	Tool *ToolCall
	// TurnID is the turn that produced the item, when known; history
	// items carry it so a resumed session can Rewind to them.
	TurnID string
	// Time is when the item was recorded, when known.
	Time time.Time
	// ClientID echoes Input.ClientID on the user message it produced.
	ClientID string
	// Synthetic marks a message the harness injected rather than the
	// user, such as a background-task notification prompt.
	Synthetic bool
	// Raw holds the native item.
	Raw json.RawMessage
}

// ToolKind classifies a tool call so a UI can render it without knowing
// every harness's tool names.
type ToolKind string

// Tool call classes.
const (
	ToolShell    ToolKind = "shell"    // runs a command
	ToolRead     ToolKind = "read"     // reads a file
	ToolEdit     ToolKind = "edit"     // changes files
	ToolSearch   ToolKind = "search"   // searches files or content
	ToolWeb      ToolKind = "web"      // searches or fetches the web
	ToolMCP      ToolKind = "mcp"      // calls an MCP server tool
	ToolSubagent ToolKind = "subagent" // delegates to another agent
	ToolTodo     ToolKind = "todo"     // updates a task list
	ToolQuestion ToolKind = "question" // asks the user
	ToolPlan     ToolKind = "plan"     // proposes a plan
	ToolHost     ToolKind = "host"     // calls a host-defined Tool
	ToolOther    ToolKind = "other"
)

// ToolCall describes a tool invocation and its outcome.
type ToolCall struct {
	// Name is the native tool name, such as "Bash", "commandExecution",
	// or "mcp__server__tool".
	Name string
	Kind ToolKind
	// Input is the tool arguments as JSON.
	Input json.RawMessage
	// Output is the tool result as text, when finished.
	Output string
	// OutputData holds a structured tool result when the harness provides
	// one.
	OutputData json.RawMessage
	// IsError reports a failed tool call.
	IsError bool

	// Command, Cwd, and ExitCode describe shell tools.
	Command  string
	Cwd      string
	ExitCode *int
	// Changes lists file changes of edit tools.
	Changes []FileChange
	// Path is the main file of read and edit tools.
	Path string
	// Server is the MCP server of MCP tools.
	Server string
	// Query is the search text of search and web tools.
	Query string
	// URL is the address of web fetch tools.
	URL string
	// Duration is the run time when the harness reports it.
	Duration time.Duration
	// DisplayName, ServerDisplayName, and IconURL are presentation hints
	// the harness provides, such as for MCP connector tools.
	DisplayName       string
	ServerDisplayName string
	IconURL           string
	// Images are image outputs of the tool, such as MCP image content or a
	// generated image.
	Images []Image
}

// Image is an image produced by a tool. One of Data, URL, or Path is set.
type Image struct {
	MediaType string
	Data      []byte
	URL       string
	Path      string
}

// FileChangeKind names a kind of file change.
type FileChangeKind string

// File change kinds.
const (
	FileAdded    FileChangeKind = "add"
	FileDeleted  FileChangeKind = "delete"
	FileModified FileChangeKind = "update"
)

// FileChange is one file changed by an edit tool.
type FileChange struct {
	Path string
	Kind FileChangeKind
	// MovePath is the new path when the file was renamed.
	MovePath string
	// Diff is a unified diff when the harness provides one.
	Diff string
}

// PlanStepStatus is the state of a plan step.
type PlanStepStatus string

// Plan step states.
const (
	StepPending    PlanStepStatus = "pending"
	StepInProgress PlanStepStatus = "in_progress"
	StepCompleted  PlanStepStatus = "completed"
)

// PlanStep is one entry of the agent's task list.
type PlanStep struct {
	// ID identifies the step when the harness assigns IDs.
	ID     string
	Text   string
	Status PlanStepStatus
}

// TaskStatus is the state of a background task or subagent.
type TaskStatus string

// Task states.
const (
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskStopped   TaskStatus = "stopped"
)

// TaskKind classifies a task.
type TaskKind string

// Task kinds.
const (
	// TaskSubagent is another agent working for this session.
	TaskSubagent TaskKind = "subagent"
	// TaskShell is a background shell command.
	TaskShell TaskKind = "shell"
	// TaskWorkflow is a scripted multi-step workflow.
	TaskWorkflow TaskKind = "workflow"
	// TaskOther covers native task types without a uniform kind.
	TaskOther TaskKind = "other"
)

// Task is a unit of work running beside the main conversation, such as a
// subagent or a background shell command. TaskUpdated events carry the full
// current state, not a patch.
type Task struct {
	// ID identifies the task. For subagents it is the value of
	// Item.SubagentID on the items the subagent produces.
	ID   string
	Kind TaskKind
	// NativeKind is the harness's task type, such as "local_agent".
	NativeKind  string
	Status      TaskStatus
	Description string
	// Summary is the latest progress note.
	Summary string
	// Output is the final result text once the task finished, and Error
	// the failure reason.
	Output string
	Error  string
	// ToolCallID is the tool call that started the task, when known.
	ToolCallID string
	// ParentID is the task that started this task, if any.
	ParentID string
	// Subagent describes a TaskSubagent.
	Subagent *Subagent
	// ToolUses counts tool calls the task made, LastTool names the latest.
	ToolUses int
	LastTool string
	Duration time.Duration
	Usage    *Usage
	// Workflow reports the phases and agents of a TaskWorkflow.
	Workflow *Workflow
	// Hidden marks housekeeping tasks the harness asks clients not to
	// show inline.
	Hidden bool
}

// Workflow is the progress of a multi-agent workflow task.
type Workflow struct {
	Phases []WorkflowPhase
	Agents []WorkflowAgent
}

// WorkflowPhase is one phase of a workflow.
type WorkflowPhase struct {
	Index int
	Title string
}

// WorkflowAgent is one agent run of a workflow.
type WorkflowAgent struct {
	Index int
	// Phase is the WorkflowPhase.Index the agent belongs to.
	Phase int
	Label string
	// State is the native state, such as "queued", "running", or "done".
	State string
	// AgentID identifies the agent's transcript, when known.
	AgentID   string
	Model     string
	Attempt   int
	Tokens    int64
	ToolCalls int
	Duration  time.Duration
	// Prompt and Result are previews of the agent's input and answer.
	Prompt string
	Result string
}

// Subagent describes an agent working for the session.
type Subagent struct {
	// Type is the agent type or name, such as "general-purpose" or
	// "explore".
	Type string
	// Name is the explicit name the agent was given, when any.
	Name   string
	Prompt string
	Model  string
	Effort Effort
	// Background reports that the parent continues without waiting.
	Background bool
	// Depth is 1 for an agent the main conversation started and grows by
	// one per nesting level; 0 when unknown.
	Depth int
}

// Usage reports token use and cost. Fields a harness does not report are
// zero.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// ReasoningTokens is included in OutputTokens.
	ReasoningTokens int64
	TotalTokens     int64
	CostUSD         float64
	// ContextWindow is the model's context size in tokens.
	ContextWindow int64
	// ContextTokens is the current context use in tokens.
	ContextTokens int64
}

// Clone returns a copy of the item that shares no ToolCall with it.
func (i Item) Clone() Item {
	if i.Tool != nil {
		call := *i.Tool
		i.Tool = &call
	}
	return i
}

// Clone returns a copy of the task that shares no Subagent, Usage, or
// Workflow with it.
func (t Task) Clone() Task {
	if t.Subagent != nil {
		sa := *t.Subagent
		t.Subagent = &sa
	}
	if t.Usage != nil {
		u := *t.Usage
		t.Usage = &u
	}
	if t.Workflow != nil {
		t.Workflow = &Workflow{Phases: slices.Clone(t.Workflow.Phases), Agents: slices.Clone(t.Workflow.Agents)}
	}
	return t
}

// Add adds o's token counts and cost to u. ContextWindow and
// ContextTokens are snapshots rather than counts, so they take o's values
// when those are non-zero.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.TotalTokens += o.TotalTokens
	u.CostUSD += o.CostUSD
	u.ContextWindow = cmp.Or(o.ContextWindow, u.ContextWindow)
	u.ContextTokens = cmp.Or(o.ContextTokens, u.ContextTokens)
}
