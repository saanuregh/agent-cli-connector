package claude

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
)

// envelope holds the fields shared by every stdout message.
type envelope struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	UUID            string          `json:"uuid"`
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"`
	Response        json.RawMessage `json:"response"`
}

type controlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response"`
	Error     string          `json:"error"`
	ErrorCode string          `json:"error_code"`
	// PendingPermissionRequests and PendingUserDialogRequests are prompts
	// the CLI delivers again on an initialize response, as full
	// control_request messages.
	PendingPermissionRequests []json.RawMessage `json:"pending_permission_requests"`
	PendingUserDialogRequests []json.RawMessage `json:"pending_user_dialog_requests"`
}

type controlRequestHeader struct {
	Subtype string `json:"subtype"`
}

type contentBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
	Signature  string          `json:"signature,omitempty"`
	Data       string          `json:"data,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	ServerName string          `json:"server_name,omitempty"`
	Source     *imageSource    `json:"source,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type assistantFrame struct {
	Message struct {
		ID         string         `json:"id"`
		Model      string         `json:"model"`
		Content    []contentBlock `json:"content"`
		StopReason *string        `json:"stop_reason"`
		Usage      *apiUsage      `json:"usage"`
	} `json:"message"`
	ParentToolUseID *string  `json:"parent_tool_use_id"`
	Error           string   `json:"error"`
	UUID            string   `json:"uuid"`
	AgentID         string   `json:"agent_id"`
	Aborted         bool     `json:"aborted"`
	Supersedes      []string `json:"supersedes"`
	ToolUseMeta     []struct {
		ID                string `json:"id"`
		DisplayName       string `json:"display_name"`
		ServerDisplayName string `json:"server_display_name"`
		IconURL           string `json:"icon_url"`
	} `json:"tool_use_meta"`
}

type userFrame struct {
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ParentToolUseID           *string         `json:"parent_tool_use_id"`
	IsSynthetic               bool            `json:"isSynthetic"`
	IsReplay                  bool            `json:"isReplay"`
	IsCompactSummary          bool            `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool            `json:"isVisibleInTranscriptOnly"`
	ToolUseResult             json.RawMessage `json:"tool_use_result"`
	UUID                      string          `json:"uuid"`
	AgentID                   string          `json:"agent_id"`
	ToolResultMeta            []struct {
		ID               string `json:"id"`
		NonExecutionKind string `json:"non_execution_kind"`
	} `json:"tool_result_meta"`
}

type streamFrame struct {
	Event struct {
		Type         string       `json:"type"`
		Index        int          `json:"index"`
		ContentBlock contentBlock `json:"content_block"`
		Delta        struct {
			Type        string  `json:"type"`
			Text        string  `json:"text"`
			Thinking    string  `json:"thinking"`
			PartialJSON string  `json:"partial_json"`
			StopReason  *string `json:"stop_reason"`
		} `json:"delta"`
		Message struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
	} `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
}

type modelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	ThinkingTokens           int64   `json:"thinkingTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int64   `json:"contextWindow"`
}

type resultFrame struct {
	Subtype          string                `json:"subtype"`
	IsError          bool                  `json:"is_error"`
	DurationMS       int64                 `json:"duration_ms"`
	NumTurns         int                   `json:"num_turns"`
	Result           string                `json:"result"`
	StopReason       *string               `json:"stop_reason"`
	TotalCostUSD     float64               `json:"total_cost_usd"`
	Usage            apiUsage              `json:"usage"`
	ModelUsage       map[string]modelUsage `json:"modelUsage"`
	Errors           []string              `json:"errors"`
	StructuredOutput json.RawMessage       `json:"structured_output"`
	TerminalReason   string                `json:"terminal_reason"`
	UserMessageUUID  string                `json:"user_message_uuid"`
	UserMessageUUIDs []string              `json:"user_message_uuids"`
	APIErrorStatus   *int                  `json:"api_error_status"`
	StartupFailure   string                `json:"startup_failure_reason"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	DeferredToolUse *struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"deferred_tool_use"`
	PermissionDenials []struct {
		ToolName  string          `json:"tool_name"`
		ToolUseID string          `json:"tool_use_id"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
}

type initFrame struct {
	Cwd            string   `json:"cwd"`
	Tools          []string `json:"tools"`
	Model          string   `json:"model"`
	PermissionMode string   `json:"permissionMode"`
	SlashCommands  []string `json:"slash_commands"`
	Version        string   `json:"claude_code_version"`
	MCPServers     []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Effort           *string  `json:"effort"`
	Capabilities     []string `json:"capabilities"`
	UserMessageUUIDs []string `json:"user_message_uuids"`
	PluginErrors     []struct {
		Plugin  string `json:"plugin"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Path    string `json:"path"`
	} `json:"plugin_errors"`
}

// legacyToolNames maps former Claude Code tool names to their current
// names, from the Agent SDK's LEGACY_TOOL_NAME_ALIASES.
var legacyToolNames = map[string]string{
	"Task":               "Agent",
	"KillShell":          "TaskStop",
	"KillBash":           "TaskStop",
	"ListPeers":          "ListAgents",
	"Brief":              "SendUserMessage",
	"ListMcpResources":   "ListMcpResourcesTool",
	"ReadMcpResource":    "ReadMcpResourceTool",
	"ReadMcpResourceDir": "ReadMcpResourceDirTool",
}

// toolKind classifies Claude Code tool names. Legacy names count as the
// tools that replaced them. TaskStop and Workflow manage background agents
// and tasks, so they count as subagent tools; the MCP resource tools count
// as MCP calls; SendUserMessage only shows the user a message and stays
// "other".
func toolKind(name, hostServer string) string {
	if current, ok := legacyToolNames[name]; ok {
		name = current
	}
	switch name {
	case "Bash", "BashOutput", "PowerShell", "Monitor":
		return "shell"
	case "Read", "NotebookRead":
		return "read"
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return "edit"
	case "Grep", "Glob", "LS", "ToolSearch", "LSP":
		return "search"
	case "WebFetch", "WebSearch", "web_search", "web_fetch":
		return "web"
	case "Agent", "SendMessage", "ListAgents", "TaskStop", "Workflow":
		return "subagent"
	case "ListMcpResourcesTool", "ReadMcpResourceTool", "ReadMcpResourceDirTool":
		return "mcp"
	case "TodoWrite", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet":
		return "todo"
	case "AskUserQuestion":
		return "question"
	case "ExitPlanMode", "EnterPlanMode":
		return "plan"
	}
	if strings.HasPrefix(name, "mcp__") {
		if hostServer != "" && strings.HasPrefix(name, "mcp__"+hostServer+"__") {
			return "host"
		}
		return "mcp"
	}
	return "other"
}

// mcpServerOf returns the server part of an "mcp__server__tool" name.
func mcpServerOf(name string) string {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return ""
	}
	server, _, _ := strings.Cut(rest, "__")
	return server
}

// requestUsage holds the input counters of one model request; nil means
// the counter was absent.
type requestUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
}

// merge replaces the counters next carries.
func (u *requestUsage) merge(next requestUsage) {
	for _, f := range [][2]**int64{{&u.InputTokens, &next.InputTokens}, {&u.CacheReadInputTokens, &next.CacheReadInputTokens},
		{&u.CacheCreationInputTokens, &next.CacheCreationInputTokens}, {&u.OutputTokens, &next.OutputTokens}} {
		if *f[1] != nil {
			*f[0] = *f[1]
		}
	}
}

// total is the context the request used: its input plus its output.
func (u requestUsage) total() int64 {
	var n int64
	for _, p := range []*int64{u.InputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.OutputTokens} {
		if p != nil {
			n += *p
		}
	}
	return n
}

// toolResultContent returns the text and images of tool_result content,
// decoding it once.
func toolResultContent(raw json.RawMessage) (string, []agent.Image) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}
	return textOfBlocks(blocks), imagesOfBlocks(blocks)
}

func imagesOfBlocks(blocks []contentBlock) []agent.Image {
	var out []agent.Image
	for _, b := range blocks {
		if b.Type != "image" || b.Source == nil {
			continue
		}
		switch b.Source.Type {
		case "base64":
			data, err := base64.StdEncoding.DecodeString(b.Source.Data)
			if err == nil {
				out = append(out, agent.Image{MediaType: b.Source.MediaType, Data: data})
			}
		case "url":
			out = append(out, agent.Image{MediaType: b.Source.MediaType, URL: b.Source.URL})
		}
	}
	return out
}

// textOf flattens tool_result or message content to text.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	return textOfBlocks(blocks)
}
