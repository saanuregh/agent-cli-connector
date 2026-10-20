package claude

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

var _ agent.SubagentLister = (*Provider)(nil)

// ListSubagents lists the subagents and workflows a persisted session ran,
// from the files the CLI writes beside the transcript:
// subagents/**/agent-<id>.jsonl with its agent-<id>.meta.json sidecar, and
// workflows/<run>.json run summaries. Subagent tasks are linked to the
// spawning tool call through the sidecar; a workflow's members have the
// workflow's run ID as ParentID. Unreadable files are skipped. The files
// are undocumented CLI internals, so fields can be missing.
func (p *Provider) ListSubagents(ctx context.Context, sessionID string, opts agent.ListSubagentsOptions) ([]agent.Task, error) {
	fsys := p.fileSystem()
	path, err := findTranscript(fsys, p.configDir(agent.Config{}), sessionID, opts.Cwd)
	if err != nil {
		return nil, err
	}
	g, err := readTranscriptGraph(ctx, fsys, path)
	if err != nil {
		return nil, err
	}
	calls := parentCalls(g)
	dir := strings.TrimSuffix(path, ".jsonl")
	var out []agent.Task
	subRoot := filepath.Join(dir, "subagents")
	_ = providerkit.WalkDir(fsys, subRoot, func(file string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if d.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		task := subagentTask(fsys, file, id, calls)
		// Workflow members live in subagents/workflows/<run>/.
		if rel, err := filepath.Rel(subRoot, file); err == nil {
			if parts := strings.Split(rel, string(filepath.Separator)); len(parts) >= 3 && parts[0] == "workflows" {
				task.ParentID = parts[1]
			}
		}
		out = append(out, task)
		return nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, _ := fsys.ReadDir(filepath.Join(dir, "workflows"))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if task, ok := workflowTask(fsys, filepath.Join(dir, "workflows", e.Name()), calls); ok {
			out = append(out, task)
		}
	}
	return out, nil
}

// parentCall is what a transcript records about a tool call that started
// a subagent or workflow.
type parentCall struct {
	name, subagentType, description string
	done, failed                    bool
	output                          string
}

// parentLinks are the subagent and workflow calls of a transcript, by tool
// call ID, and the workflow calls by run ID.
type parentLinks struct {
	byID  map[string]*parentCall
	byRun map[string]string
}

var workflowRunID = regexp.MustCompile(`(?:^|\n)Run ID:\s*(wf_[A-Za-z0-9-]+)`)

func parentCalls(g *transcriptGraph) parentLinks {
	links := parentLinks{byID: map[string]*parentCall{}, byRun: map[string]string{}}
	for _, e := range g.entries {
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		var blocks []contentBlock
		if json.Unmarshal(e.Message, &m) != nil || json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			switch {
			case b.Type == "tool_use" && (b.Name == "Agent" || b.Name == "Task" || b.Name == "Workflow"):
				var in struct {
					Name         string `json:"name"`
					SubagentType string `json:"subagent_type"`
					Description  string `json:"description"`
				}
				_ = json.Unmarshal(b.Input, &in)
				links.byID[b.ID] = &parentCall{name: in.Name, subagentType: in.SubagentType, description: in.Description}
			case b.Type == "tool_result":
				c, ok := links.byID[b.ToolUseID]
				if !ok {
					continue
				}
				c.done, c.failed = true, b.IsError
				c.output, _ = toolResultContent(b.Content)
				if m := workflowRunID.FindStringSubmatch(c.output); m != nil {
					links.byRun[m[1]] = b.ToolUseID
				}
			}
		}
	}
	return links
}

func subagentTask(fsys agent.FileSystem, file, id string, links parentLinks) agent.Task {
	task := agent.Task{ID: id, Kind: agent.TaskSubagent, NativeKind: "local_agent", Status: agent.TaskRunning, Subagent: &agent.Subagent{}}
	var meta struct {
		AgentType    string `json:"agentType"`
		Description  string `json:"description"`
		ToolUseID    string `json:"toolUseId"`
		SpawnDepth   int    `json:"spawnDepth"`
		RequestShape string `json:"requestShape"`
	}
	if data, err := providerkit.ReadFile(fsys, strings.TrimSuffix(file, ".jsonl")+".meta.json"); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	task.ToolCallID = meta.ToolUseID
	task.Subagent.Type = meta.AgentType
	task.Subagent.Depth = meta.SpawnDepth
	task.Subagent.Background = meta.RequestShape == "background"
	task.Description = meta.Description
	if c, ok := links.byID[meta.ToolUseID]; ok && meta.ToolUseID != "" {
		task.Subagent.Name = c.name
		task.Subagent.Type = cmp.Or(c.subagentType, task.Subagent.Type)
		task.Description = cmp.Or(c.description, task.Description)
		if c.done && !task.Subagent.Background {
			task.Status = agent.TaskCompleted
			if c.failed {
				task.Status = agent.TaskFailed
				task.Error = c.output
			}
		}
	}
	readSubagentRun(fsys, file, &task)
	return task
}

// readSubagentRun fills in what a subagent's own transcript tells: its
// prompt, model, effort, final answer, and the context size of its last
// request. The final answer is the report of its last SubagentHandback
// call, as in the live stream, else its last text.
func readSubagentRun(fsys agent.FileSystem, file string, task *agent.Task) {
	var last, handback string
	var lastStop string
	err := eachTranscriptEntry(fsys, file, isNotAPIRequestLine, func(e transcriptEntry) error {
		var m struct {
			Model      string          `json:"model"`
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
			Usage      *requestUsage   `json:"usage"`
		}
		_ = json.Unmarshal(e.Message, &m)
		switch e.Type {
		case "user":
			if task.Subagent.Prompt == "" && !e.IsMeta {
				task.Subagent.Prompt, _ = toolResultContent(m.Content)
			}
		case "assistant":
			if m.Model != "" && m.Model != "<synthetic>" {
				task.Subagent.Model = m.Model
			}
			if e.Effort != "" {
				task.Subagent.Effort = agent.Effort(e.Effort)
			}
			if m.Usage != nil && m.Usage.total() > 0 {
				task.Usage = &agent.Usage{TotalTokens: m.Usage.total()}
			}
			var blocks []contentBlock
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" && b.Name == handbackTool {
						var in struct {
							Message string `json:"message"`
						}
						if json.Unmarshal(b.Input, &in) == nil && strings.TrimSpace(in.Message) != "" {
							handback = in.Message
						}
					}
				}
			}
			if t, _ := toolResultContent(m.Content); t != "" {
				last = t
			}
			lastStop = m.StopReason
		}
		return nil
	})
	if err != nil {
		return
	}
	task.Output = cmp.Or(handback, last)
	if task.Status == agent.TaskRunning && lastStop == "end_turn" {
		task.Status = agent.TaskCompleted
	}
}

func workflowTask(fsys agent.FileSystem, file string, links parentLinks) (agent.Task, bool) {
	data, err := providerkit.ReadFile(fsys, file)
	if err != nil {
		return agent.Task{}, false
	}
	var run struct {
		RunID        string          `json:"runId"`
		Summary      string          `json:"summary"`
		WorkflowName string          `json:"workflowName"`
		Status       string          `json:"status"`
		StartTime    int64           `json:"startTime"`
		TotalTokens  int64           `json:"totalTokens"`
		Timestamp    string          `json:"timestamp"`
		Result       json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &run) != nil || run.RunID == "" {
		return agent.Task{}, false
	}
	task := agent.Task{
		ID:          run.RunID,
		Kind:        agent.TaskWorkflow,
		NativeKind:  "local_workflow",
		Description: cmp.Or(strings.TrimSpace(run.Summary), strings.TrimSpace(run.WorkflowName)),
		ToolCallID:  links.byRun[run.RunID],
		Output:      formatWorkflowResult(run.Result),
	}
	// A run summary that is not terminal can no longer finish.
	switch strings.ToLower(strings.TrimSpace(run.Status)) {
	case "completed":
		task.Status = agent.TaskCompleted
	case "canceled", "cancelled", "killed", "stopped":
		task.Status = agent.TaskStopped
	default:
		task.Status = agent.TaskFailed
	}
	if run.TotalTokens > 0 {
		task.Usage = &agent.Usage{TotalTokens: run.TotalTokens}
	}
	if end := providerutil.ParseTime(run.Timestamp).UnixMilli(); end > 0 && run.StartTime > 0 && end > run.StartTime {
		task.Duration = time.Duration(end-run.StartTime) * time.Millisecond
	}
	return task, true
}

// maxWorkflowOutput caps the workflow output file read for Task.Output.
const maxWorkflowOutput = 1 << 20

// readWorkflowOutputFrom returns the result in a workflow's output file in
// fsys, or "" when the file is missing, too large, or holds no result.
func readWorkflowOutputFrom(fsys agent.FileSystem, path string) string {
	f, err := fsys.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxWorkflowOutput+1))
	if err != nil || len(data) > maxWorkflowOutput {
		return ""
	}
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &out) != nil {
		return ""
	}
	return formatWorkflowResult(out.Result)
}

// formatWorkflowResult renders a workflow result as text: objects with a
// single key are unwrapped, strings, numbers, and booleans are printed as
// is, and anything else becomes an indented JSON block.
func formatWorkflowResult(raw json.RawMessage) string {
	for range 8 {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || len(obj) != 1 {
			break
		}
		for _, v := range obj {
			raw = v
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return strings.TrimSpace(str)
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		return num.String()
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return strconv.FormatBool(b)
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return ""
	}
	return "```json\n" + buf.String() + "\n```"
}
