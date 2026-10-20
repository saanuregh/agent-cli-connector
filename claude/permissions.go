package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// PermissionUpdate is a native permission rule change, as offered in
// permission suggestions and accepted in PermissionResult.
type PermissionUpdate struct {
	// Type is "addRules", "replaceRules", "removeRules", "setMode",
	// "addDirectories", or "removeDirectories".
	Type        string           `json:"type"`
	Rules       []PermissionRule `json:"rules,omitempty"`
	Behavior    string           `json:"behavior,omitempty"`
	Mode        string           `json:"mode,omitempty"`
	Directories []string         `json:"directories,omitempty"`
	// Destination is "userSettings", "projectSettings", "localSettings",
	// "session", or "cliArg".
	Destination string `json:"destination"`
}

// PermissionRule names a tool and optional rule content, such as
// {"Bash", "npm run test:*"}.
type PermissionRule struct {
	ToolName    string `json:"toolName"`
	RuleContent string `json:"ruleContent,omitempty"`
}

// PermissionResult is the native answer to a permission request. Pass it
// in agent.PermissionResponse.ProviderResponse for full control.
type PermissionResult struct {
	// Behavior is "allow" or "deny".
	Behavior           string             `json:"behavior"`
	UpdatedInput       json.RawMessage    `json:"updatedInput,omitempty"`
	UpdatedPermissions []PermissionUpdate `json:"updatedPermissions,omitempty"`
	Message            string             `json:"message,omitempty"`
	Interrupt          bool               `json:"interrupt,omitempty"`
	// DecisionClassification is "user_temporary", "user_permanent", or
	// "user_reject".
	DecisionClassification string `json:"decisionClassification,omitempty"`
	ToolUseID              string `json:"toolUseID,omitempty"`
}

// PermissionDetails is the Claude-specific part of a permission request,
// available as the request's Raw control request and through
// PermissionDetailsOf.
type PermissionDetails struct {
	ToolName            string             `json:"tool_name"`
	Input               json.RawMessage    `json:"input"`
	Suggestions         []PermissionUpdate `json:"permission_suggestions"`
	BlockedPath         string             `json:"blocked_path"`
	DecisionReason      string             `json:"decision_reason"`
	DecisionReasonType  string             `json:"decision_reason_type"`
	Title               string             `json:"title"`
	DisplayName         string             `json:"display_name"`
	Description         string             `json:"description"`
	DefaultToNo         bool               `json:"default_to_no"`
	SuppressAlwaysAllow bool               `json:"suppress_always_allow_rule"`
	ToolUseID           string             `json:"tool_use_id"`
	AgentID             string             `json:"agent_id"`
	RequiresInteraction *bool              `json:"requires_user_interaction"`
	MCPServer           *struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"mcp_server"`
}

// PermissionDetailsOf decodes the Claude-specific fields of a permission
// request produced by this package.
func PermissionDetailsOf(req *agent.PermissionRequest) (*PermissionDetails, error) {
	var envelope struct {
		Request PermissionDetails `json:"request"`
	}
	if err := json.Unmarshal(req.Raw, &envelope); err != nil {
		return nil, err
	}
	return &envelope.Request, nil
}

func (s *Session) handlePermission(ctx context.Context, requestID string, raw json.RawMessage, line []byte) {
	var d PermissionDetails
	if err := json.Unmarshal(raw, &d); err != nil {
		_ = s.respond(requestID, nil, err)
		return
	}
	if len(d.Input) == 0 || string(d.Input) == "null" {
		d.Input = json.RawMessage("{}")
	}
	meta := agent.EventMeta{Raw: line}
	switch d.ToolName {
	case "AskUserQuestion":
		s.askUserQuestion(requestID, d, meta)
		return
	}

	kind := agent.PermissionTool
	var plan, command string
	var changes []agent.FileChange
	switch toolKind(d.ToolName, s.toolServerName()) {
	case "shell":
		kind = agent.PermissionCommand
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(d.Input, &in)
		command = in.Command
	case "edit":
		kind = agent.PermissionFileChange
		changes = editChanges(d.ToolName, d.Input, nil)
	case "plan":
		if d.ToolName == "ExitPlanMode" {
			kind = agent.PermissionPlanApproval
			var in struct {
				Plan string `json:"plan"`
			}
			_ = json.Unmarshal(d.Input, &in)
			plan = in.Plan
		}
	}
	title := cmp.Or(d.Title, d.DisplayName)
	var paths []string
	if d.BlockedPath != "" {
		paths = append(paths, d.BlockedPath)
	}
	decisions := []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionDeny, agent.DecisionCancel}
	if d.offersRules() {
		decisions = []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionAllowAlways, agent.DecisionDeny, agent.DecisionCancel}
	}
	req := providerkit.NewPermissionRequest(agent.PermissionRequest{
		EventMeta:   meta,
		ID:          requestID,
		Kind:        kind,
		ToolName:    d.ToolName,
		ToolCallID:  d.ToolUseID,
		Input:       d.Input,
		Title:       title,
		Description: d.Description,
		Reason:      d.DecisionReason,
		Command:     command,
		Changes:     changes,
		Paths:       paths,
		Plan:        plan,
		Decisions:   decisions,
		SubagentID:  d.AgentID,
	}, func(ctx context.Context, resp agent.PermissionResponse) error {
		return s.respond(requestID, permissionResult(d, resp), nil)
	})
	s.rt.Request(req)
}

// offersRules reports whether the request can be allowed with a rule:
// only the CLI's own suggestions are scoped to this call (such as one
// command prefix), and suppress_always_allow_rule forbids rules.
func (d PermissionDetails) offersRules() bool {
	return len(d.Suggestions) > 0 && !d.SuppressAlwaysAllow
}

// permissionResult answers a permission request. AllowSession and
// AllowAlways without rules to offer allow the one call, because a rule
// made up here would allow every call of the tool.
func permissionResult(d PermissionDetails, resp agent.PermissionResponse) any {
	if resp.ProviderResponse != nil {
		return resp.ProviderResponse
	}
	//exhaustive:enforce
	switch resp.Decision {
	case agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionAllowAlways:
		input := d.Input
		if len(resp.UpdatedInput) > 0 {
			input = resp.UpdatedInput
		}
		r := PermissionResult{Behavior: "allow", UpdatedInput: input, ToolUseID: d.ToolUseID, DecisionClassification: "user_temporary"}
		if !d.offersRules() {
			return r
		}
		switch resp.Decision {
		case agent.DecisionAllowSession:
			r.UpdatedPermissions = withDestination(d.Suggestions, "session")
			r.DecisionClassification = "user_permanent"
		case agent.DecisionAllowAlways:
			r.UpdatedPermissions = d.Suggestions
			r.DecisionClassification = "user_permanent"
		}
		return r
	case agent.DecisionDeny, agent.DecisionCancel:
		fallthrough
	default:
		msg := resp.Message
		if msg == "" {
			msg = "The user denied this action."
		}
		return PermissionResult{
			Behavior:               "deny",
			Message:                msg,
			Interrupt:              resp.Decision == agent.DecisionCancel,
			ToolUseID:              d.ToolUseID,
			DecisionClassification: "user_reject",
		}
	}
}

// withDestination rewrites persistent suggestions to apply only to the
// session.
func withDestination(updates []PermissionUpdate, dest string) []PermissionUpdate {
	out := make([]PermissionUpdate, len(updates))
	for i, u := range updates {
		u.Destination = dest
		out[i] = u
	}
	return out
}

type askQuestion struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
		Preview     string `json:"preview"`
	} `json:"options"`
}

// askUserQuestion turns the AskUserQuestion tool's permission prompt into a
// UserInputRequest. The answers go back as the tool's updated input.
func (s *Session) askUserQuestion(requestID string, d PermissionDetails, meta agent.EventMeta) {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	_ = json.Unmarshal(d.Input, &in)
	questions := make([]agent.Question, 0, len(in.Questions))
	for _, q := range in.Questions {
		opts := make([]agent.QuestionOption, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, agent.QuestionOption{Label: o.Label, Description: o.Description, Preview: o.Preview})
		}
		questions = append(questions, agent.Question{
			ID:          q.Question,
			Header:      q.Header,
			Text:        q.Question,
			Options:     opts,
			MultiSelect: q.MultiSelect,
			AllowOther:  true,
		})
	}
	req := providerkit.NewUserInputRequest(agent.UserInputRequest{
		EventMeta:  meta,
		ID:         requestID,
		Questions:  questions,
		ToolCallID: d.ToolUseID,
		SubagentID: d.AgentID,
	}, func(ctx context.Context, resp agent.UserInputResponse) error {
		if resp.Cancelled {
			return s.respond(requestID, PermissionResult{
				Behavior:  "deny",
				Message:   "The user declined to answer the questions.",
				ToolUseID: d.ToolUseID,
			}, nil)
		}
		var input map[string]any
		_ = json.Unmarshal(d.Input, &input)
		if input == nil {
			input = map[string]any{}
		}
		answers := make(map[string]string, len(resp.Answers))
		for id, vals := range resp.Answers {
			answers[id] = strings.Join(vals, ", ")
		}
		input["answers"] = answers
		updated, _ := json.Marshal(input)
		return s.respond(requestID, PermissionResult{Behavior: "allow", UpdatedInput: updated, ToolUseID: d.ToolUseID}, nil)
	})
	s.rt.Request(req)
}

// handleElicitation turns an MCP elicitation into an ElicitationRequest.
// Schema keeps the raw JSON, so the server's property order survives and
// the request's property_order list is not needed.
func (s *Session) handleElicitation(ctx context.Context, requestID string, raw json.RawMessage, line []byte) {
	var r struct {
		ServerName      string          `json:"mcp_server_name"`
		Message         string          `json:"message"`
		Mode            string          `json:"mode"`
		URL             string          `json:"url"`
		ElicitationID   string          `json:"elicitation_id"`
		RequestedSchema json.RawMessage `json:"requested_schema"`
		Title           string          `json:"title"`
		DisplayName     string          `json:"display_name"`
		Description     string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		_ = s.respond(requestID, nil, err)
		return
	}
	mode := r.Mode
	if mode == "" {
		mode = "form"
	}
	req := providerkit.NewElicitationRequest(agent.ElicitationRequest{
		EventMeta:     agent.EventMeta{Raw: line},
		ID:            requestID,
		Server:        r.ServerName,
		Message:       r.Message,
		Mode:          mode,
		Schema:        r.RequestedSchema,
		URL:           r.URL,
		ElicitationID: r.ElicitationID,
		Title:         cmp.Or(r.Title, r.DisplayName),
		Description:   r.Description,
	}, func(ctx context.Context, resp agent.ElicitationResponse) error {
		out := map[string]any{"action": string(resp.Action)}
		if len(resp.Content) > 0 {
			out["content"] = resp.Content
		}
		return s.respond(requestID, out, nil)
	})
	s.rt.Request(req)
}
