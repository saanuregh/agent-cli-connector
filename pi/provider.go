package pi

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Provider starts Pi sessions.
type Provider struct {
	opts Options
}

var (
	_ agent.Provider       = (*Provider)(nil)
	_ agent.ModelLister    = (*Provider)(nil)
	_ agent.SessionLister  = (*Provider)(nil)
	_ agent.HistoryReader  = (*Provider)(nil)
	_ agent.Checker        = (*Provider)(nil)
	_ agent.SubagentLister = (*Provider)(nil)
	_ agent.SessionDeleter = (*Provider)(nil)
)

// New returns a provider.
func New(opts Options) *Provider {
	return &Provider{opts: opts}
}

// Name returns "pi".
func (p *Provider) Name() string { return Name }

// Capabilities reports the supported uniform features.
func (p *Provider) Capabilities() agent.Capabilities {
	bridge := !p.opts.DisableBridge
	caps := agent.Capabilities{
		Streaming:       true,
		ReasoningStream: true,
		Images:          true,
		Steer:           true,
		Queue:           true,
		Interrupt:       true,
		SetModel:        true,
		SetEffort:       true,
		UserQuestions:   true,
		Resume:          true,
		Fork:            true,
		SystemPrompt:    true,
		CostReporting:   true,
		ForkAtTurn:      true,
		// Subagent extensions, such as Pi's example subagent tool, report
		// their runs in tool results.
		Subagents: true,
		Tasks:     true,
		// With a todo extension, such as Pi's example todo tool.
		Plans: true,
	}
	if bridge {
		caps.PermissionModes = []agent.PermissionMode{
			agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionPlan,
			agent.PermissionReadOnly, agent.PermissionDontAsk, agent.PermissionBypass,
		}
		caps.PermissionPrompt = true
		caps.HostTools = true
		caps.MCPServers = true
		caps.OutputSchema = true
	}
	return caps
}

// Start launches a session.
func (p *Provider) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	return p.StartSession(ctx, cfg)
}

// StartSession launches a session and returns the concrete type.
func (p *Provider) StartSession(ctx context.Context, cfg agent.Config) (*Session, error) {
	opts, err := providerkit.SessionOptions[SessionOptions](Name, cfg.ProviderOptions)
	if err != nil {
		return nil, err
	}
	return p.startSession(ctx, cfg, opts)
}

func (p *Provider) path() string {
	if p.opts.Path != "" {
		return p.opts.Path
	}
	return "pi"
}

func (p *Provider) startupTimeout() time.Duration {
	if p.opts.StartupTimeout > 0 {
		return p.opts.StartupTimeout
	}
	return 60 * time.Second
}

func (p *Provider) shutdownGrace() time.Duration {
	if p.opts.ShutdownGrace > 0 {
		return p.opts.ShutdownGrace
	}
	return 5 * time.Second
}

// Check runs "pi --version" and counts the models Pi can use: Pi has no
// account, so having at least one model with credentials counts as signed
// in.
func (p *Provider) Check(ctx context.Context) (*agent.ProviderStatus, error) {
	st, err := providerkit.VersionStatus(ctx, p.opts.Spawner, p.path(), providerkit.EnvList(p.opts.Env))
	if err != nil || !st.Installed || st.Err != nil {
		return st, err
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		return providerkit.CheckFailed(ctx, st, err)
	}
	st.Auth = &agent.AuthStatus{Authenticated: len(models) > 0}
	return st, nil
}

// ListModels lists the configured models, as "provider/id". Extensions
// load, since they can register model providers.
func (p *Provider) ListModels(ctx context.Context) ([]agent.Model, error) {
	dir, err := os.MkdirTemp("", "agent-pi-models-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	s, err := p.StartSession(ctx, agent.Config{Cwd: dir, Ephemeral: true, ProviderOptions: &SessionOptions{DisableBridge: true, NoSkills: true, NoPromptTemplates: true, NoContextFiles: true, NoMCP: true}})
	if err != nil {
		return nil, err
	}
	defer s.Close()
	raws, err := s.AvailableModels(ctx)
	if err != nil {
		return nil, err
	}
	info := s.Info()
	current := info.Model
	out := make([]agent.Model, 0, len(raws))
	for _, raw := range raws {
		var m modelInfo
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		id := m.Provider + "/" + m.ID
		model := agent.Model{ID: id, DisplayName: m.Name, Provider: m.Provider, ContextWindow: m.ContextWindow, IsDefault: id == current, Raw: raw}
		model.InputModalities = m.Input
		model.Efforts = m.efforts()
		model.DefaultEffort = clampEffort(info.Effort, model.Efforts)
		out = append(out, model)
	}
	return out, nil
}

func (p *Provider) env(name string) string {
	if v := p.opts.Env[name]; v != "" {
		return v
	}
	return os.Getenv(name)
}

// agentDir returns Pi's agent directory.
func (p *Provider) agentDir() string {
	if d := p.env("PI_CODING_AGENT_DIR"); d != "" {
		return expandHome(d)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent")
}

// sessionsDir returns the directory Pi stores cwd's sessions under, in
// Pi's order of precedence. perCwd reports Pi's default layout, which
// keeps one subdirectory per working directory; a custom directory holds
// the sessions of every working directory. Settings files are read from
// fsys.
func (p *Provider) sessionsDir(fsys agent.FileSystem, cwd string) (dir string, perCwd bool) {
	agentDir := p.agentDir()
	def := filepath.Join(agentDir, "sessions")
	for _, d := range []string{
		p.opts.SessionDir,
		p.env("PI_CODING_AGENT_SESSION_DIR"),
		settingsSessionDir(fsys, filepath.Join(cwd, ".pi", "settings.json")),
		settingsSessionDir(fsys, filepath.Join(agentDir, "settings.json")),
	} {
		if d == "" {
			continue
		}
		d = expandHome(d)
		if !filepath.IsAbs(d) {
			d = filepath.Join(cwd, d)
		}
		// Pi treats any configured directory as flat, even the default.
		return filepath.Clean(d), false
	}
	return def, true
}

// settingsSessionDir reads the sessionDir setting of a Pi settings file.
func settingsSessionDir(fsys agent.FileSystem, path string) string {
	data, err := providerkit.ReadFile(fsys, path)
	if err != nil {
		return ""
	}
	var s struct {
		SessionDir string `json:"sessionDir"`
	}
	_ = json.Unmarshal(data, &s)
	return s.SessionDir
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// cwdDirName returns the directory Pi stores a working directory's
// sessions in.
func cwdDirName(cwd string) string {
	trimmed := strings.TrimLeft(cwd, `/\`)
	r := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return "--" + r.Replace(trimmed) + "--"
}

// sameDir reports whether two directory paths name the same directory.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// sessionFiles lists the session files under root in fsys, at any depth.
func sessionFiles(fsys agent.FileSystem, root string) []providerkit.SessionFile {
	var files []providerkit.SessionFile
	_ = providerkit.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, providerkit.SessionFile{Path: path, ModTime: info.ModTime()})
		}
		return nil
	})
	return files
}

// ListSessions lists persisted sessions, newest first.
func (p *Provider) ListSessions(ctx context.Context, opts agent.ListSessionsOptions) ([]agent.SessionSummary, error) {
	fsys := agent.FileSystemOf(p.opts.Spawner)
	cwd := providerutil.AbsDir(opts.Cwd)
	root, perCwd := p.sessionsDir(fsys, cwd)
	scan := root
	if opts.Cwd != "" && perCwd {
		scan = filepath.Join(root, cwdDirName(cwd))
	}
	return providerkit.PageSessionFiles(ctx, sessionFiles(fsys, scan), opts.Limit, opts.Offset, func(path string) (agent.SessionSummary, bool) {
		sum, ok := summarize(fsys, path)
		return sum, ok && (opts.Cwd == "" || perCwd || sameDir(sum.Cwd, cwd))
	})
}

type sessionEntry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	ParentID      *string         `json:"parentId"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	Name          string          `json:"name"`
	Summary       string          `json:"summary"`
	Message       json.RawMessage `json:"message"`
	Provider      string          `json:"provider"`
	ModelID       string          `json:"modelId"`
	ThinkingLevel string          `json:"thinkingLevel"`
	Content       json.RawMessage `json:"content"`
	Display       *bool           `json:"display"`
}

// preview shortens a prompt for a session summary.
func preview(text string) string {
	if r := []rune(text); len(r) > 200 {
		return string(r[:200])
	}
	return text
}

func summarize(fsys agent.FileSystem, path string) (agent.SessionSummary, bool) {
	f, err := fsys.Open(path)
	if err != nil {
		return agent.SessionSummary{}, false
	}
	defer f.Close()
	sum := agent.SessionSummary{Provider: Name, Path: path}
	var name string
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e sessionEntry
			if json.Unmarshal(line, &e) == nil {
				ts := providerutil.ParseTime(e.Timestamp)
				switch e.Type {
				case "session":
					sum.ID = e.ID
					sum.Cwd = e.Cwd
					sum.CreatedAt = ts
				case "session_info":
					name = e.Name
				case "model_change":
					if e.ModelID != "" {
						sum.Model = strings.TrimPrefix(e.Provider+"/"+e.ModelID, "/")
					}
				case "thinking_level_change":
					if e.ThinkingLevel != "" {
						sum.Effort = effortOf(e.ThinkingLevel)
					}
				case "message":
					var m piMessage
					if json.Unmarshal(e.Message, &m) != nil {
						break
					}
					switch m.Role {
					case "user":
						if text := m.text(); text != "" {
							if sum.FirstPrompt == "" {
								sum.FirstPrompt = preview(text)
							}
							sum.LastPrompt = preview(text)
						}
					case "assistant":
						if m.Model != "" {
							sum.Model = strings.TrimPrefix(m.Provider+"/"+m.Model, "/")
						}
					}
				}
				if !ts.IsZero() {
					sum.UpdatedAt = ts
				}
			}
		}
		if err != nil {
			break
		}
	}
	if sum.ID == "" {
		return sum, false
	}
	sum.Title = cmp.Or(name, sum.FirstPrompt)
	return sum, true
}

// readBranch reads the active branch of a session file in fsys: the chain
// of entries from the newest entry back to the root, oldest first.
func readBranch(fsys agent.FileSystem, path string) ([]sessionEntry, error) {
	data, err := providerkit.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}
	byID := map[string]sessionEntry{}
	var leaf string
	n := 0
	for line := range bytes.Lines(data) {
		var e sessionEntry
		if json.Unmarshal(line, &e) != nil || e.Type == "session" {
			continue
		}
		if e.ID == "" {
			// Version 1 files, which Pi migrates when it next opens them,
			// are a linear list without IDs.
			e.ID = "v1-" + strconv.Itoa(n)
			if leaf != "" {
				parent := leaf
				e.ParentID = &parent
			}
		}
		n++
		byID[e.ID] = e
		leaf = e.ID
	}
	var chain []sessionEntry
	for id := leaf; id != ""; {
		e, ok := byID[id]
		if !ok {
			break
		}
		chain = append(chain, e)
		if e.ParentID == nil {
			break
		}
		id = *e.ParentID
	}
	slices.Reverse(chain)
	return chain, nil
}

// ReadHistory reads the active branch of a persisted session: the chain of
// entries from the newest entry back to the root. Items carry the user
// entry ID that started their turn as TurnID, which Rewind and
// ForkAtTurn accept. With opts.SubagentID, it reads the session file a
// subagent extension recorded for that task (see ListSubagents).
func (p *Provider) ReadHistory(ctx context.Context, sessionID string, opts agent.ReadHistoryOptions) ([]agent.Item, error) {
	fsys := agent.FileSystemOf(p.opts.Spawner)
	path, err := p.findSession(fsys, sessionID, opts.Cwd)
	if err != nil {
		return nil, err
	}
	chain, err := readBranch(fsys, path)
	if err != nil {
		return nil, err
	}
	if opts.SubagentID != "" {
		_, files := branchSubagents(chain)
		file := files[opts.SubagentID]
		if file == "" {
			return nil, fmt.Errorf("pi: subagent %s of session %s has no session file", opts.SubagentID, sessionID)
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(filepath.Dir(path), file)
		}
		if chain, err = readBranch(fsys, file); err != nil {
			return nil, err
		}
	}
	return providerutil.LastPage(historyItems(chain), opts.Limit, opts.Offset), nil
}

func historyItems(chain []sessionEntry) []agent.Item {
	var items []agent.Item
	// tools holds the tool calls awaiting their results, by call ID.
	tools := map[string]int{}
	// cancelPending ends the calls whose results never came, as the live
	// session does when the run settles; a call still awaiting its result
	// at the end of the branch may still be running.
	cancelPending := func() {
		for id, idx := range tools {
			items[idx].Status = agent.ItemCancelled
			delete(tools, id)
		}
	}
	var turn string
	for _, e := range chain {
		start := len(items)
		switch e.Type {
		case "compaction", "branch_summary":
			cancelPending()
			items = append(items, agent.Item{ID: e.ID, Kind: agent.ItemCompaction, Status: agent.ItemDone, Text: e.Summary})
		case "custom_message":
			if e.Display != nil && *e.Display {
				text := piMessage{Content: e.Content}.text()
				items = append(items, agent.Item{ID: e.ID, Kind: agent.ItemOther, Status: agent.ItemDone, Text: text})
			}
		case "message":
			var m piMessage
			if json.Unmarshal(e.Message, &m) != nil {
				continue
			}
			if m.Role != "toolResult" {
				cancelPending()
			}
			if m.Role == "user" {
				turn = e.ID
			}
			items = appendHistory(items, tools, e.ID, m, e.Message)
		}
		at := providerutil.ParseTime(e.Timestamp)
		for i := start; i < len(items); i++ {
			items[i].TurnID = turn
			items[i].Time = at
		}
	}
	return items
}

func appendHistory(items []agent.Item, tools map[string]int, id string, m piMessage, raw json.RawMessage) []agent.Item {
	switch m.Role {
	case "user":
		return append(items, agent.Item{ID: id, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: m.text(), Raw: raw})
	case "assistant":
		for i, b := range m.blocks() {
			bid := id + "/" + strconv.Itoa(i)
			switch b.Type {
			case "text":
				items = append(items, agent.Item{ID: bid, Kind: agent.ItemAssistantMessage, Status: agent.ItemDone, Text: b.Text})
			case "thinking":
				items = append(items, agent.Item{ID: bid, Kind: agent.ItemReasoning, Status: agent.ItemDone, Text: b.Thinking})
			case "toolCall":
				call := &agent.ToolCall{Name: b.Name, Kind: toolKind(b.Name, nil), Input: b.Arguments}
				fillToolDetails(call, b.Arguments)
				status := agent.ItemRunning
				if m.StopReason == "aborted" || m.StopReason == "error" {
					// Pi runs no tool call of a message that failed.
					status = agent.ItemCancelled
				} else {
					tools[b.ID] = len(items)
				}
				items = append(items, agent.Item{ID: b.ID, Kind: agent.ItemToolCall, Status: status, Tool: call})
			}
		}
	case "toolResult":
		var tr struct {
			ToolCallID string `json:"toolCallId"`
			IsError    bool   `json:"isError"`
		}
		_ = json.Unmarshal(raw, &tr)
		if idx, ok := tools[tr.ToolCallID]; ok {
			delete(tools, tr.ToolCallID)
			it := &items[idx]
			it.Tool.Output, it.Tool.Images = resultContent(raw)
			it.Tool.IsError = tr.IsError
			applyToolResult(it.Tool, raw)
			it.Status = agent.ItemDone
			if tr.IsError {
				it.Status = agent.ItemFailed
			}
		}
	case "custom", "hookMessage": // hookMessage is the version 2 name
		if m.Display != nil && *m.Display {
			items = append(items, agent.Item{ID: id, Kind: agent.ItemOther, Status: agent.ItemDone, Text: m.text(), Raw: raw})
		}
	case "bashExecution":
		input, _ := json.Marshal(map[string]any{"command": m.Command})
		item := agent.Item{ID: id, Kind: agent.ItemToolCall, Tool: &agent.ToolCall{Name: "bash", Kind: agent.ToolShell, Command: m.Command, Input: input}, Raw: raw}
		bashOutcome(&item, m.Output, m.ExitCode, m.Cancelled)
		items = append(items, item)
	}
	return items
}

// branchSubagents collects the subagent tasks that "subagent" tool results
// on a session branch report, and their session files by task ID.
func branchSubagents(chain []sessionEntry) ([]agent.Task, map[string]string) {
	args := map[string]json.RawMessage{}
	files := map[string]string{}
	var tasks []agent.Task
	for _, e := range chain {
		if e.Type != "message" {
			continue
		}
		var m struct {
			piMessage
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		if json.Unmarshal(e.Message, &m) != nil {
			continue
		}
		switch m.Role {
		case "assistant":
			for _, b := range m.blocks() {
				if b.Type == "toolCall" && b.Name == "subagent" {
					args[b.ID] = b.Arguments
				}
			}
		case "toolResult":
			if m.ToolName != "subagent" {
				continue
			}
			found, paths := subagentTasks(m.ToolCallID, e.Message, args[m.ToolCallID], true)
			for _, task := range found {
				if i := slices.IndexFunc(tasks, func(t agent.Task) bool { return t.ID == task.ID }); i >= 0 {
					tasks[i] = task
				} else {
					tasks = append(tasks, task)
				}
			}
			maps.Copy(files, paths)
		}
	}
	return tasks, files
}

// ListSubagents lists the subagent tasks that "subagent" tool calls on the
// session's active branch report, from Pi's example subagent extension,
// pi-subagents, and @gotgenes/pi-subagents. Subagents that only report
// through the extension event bus, such as @tintinweb/pi-subagents, are
// not recorded in the session file.
func (p *Provider) ListSubagents(ctx context.Context, sessionID string, opts agent.ListSubagentsOptions) ([]agent.Task, error) {
	fsys := agent.FileSystemOf(p.opts.Spawner)
	path, err := p.findSession(fsys, sessionID, opts.Cwd)
	if err != nil {
		return nil, err
	}
	chain, err := readBranch(fsys, path)
	if err != nil {
		return nil, err
	}
	tasks, _ := branchSubagents(chain)
	return tasks, nil
}

// DeleteSession removes a persisted session's file. sessionID may also be
// a session file path, as for Config.Resume. Session files that subagent
// extensions recorded for its tasks are kept. It cannot be undone. It is
// unsupported when the Spawner reads files on another machine, since it
// can only remove files on this one.
func (p *Provider) DeleteSession(ctx context.Context, sessionID, cwd string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// IDs become glob patterns; paths are used as they are.
	isPath := isSessionPath(sessionID)
	if sessionID == "" || !isPath && strings.ContainsAny(sessionID, "*?[") {
		return fmt.Errorf("pi: invalid session ID %q", sessionID)
	}
	fsys := agent.FileSystemOf(p.opts.Spawner)
	if fsys != agent.LocalFileSystem {
		return providerkit.Unsupported(Name, "deleting a session on another machine")
	}
	path, err := p.findSession(fsys, sessionID, cwd)
	if err != nil {
		return err
	}
	// A path ID could name any file; only remove one that starts with a
	// Pi session header.
	if !isSessionFile(fsys, path) {
		return fmt.Errorf("pi: %s is not a Pi session file", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("pi: delete session: %w", err)
	}
	return nil
}

// isSessionFile reports whether path is a .jsonl file in fsys whose first
// line is a session header.
func isSessionFile(fsys agent.FileSystem, path string) bool {
	if filepath.Ext(path) != ".jsonl" {
		return false
	}
	e, ok := readSessionHeader(fsys, path)
	return ok && e.ID != ""
}

// readSessionHeader returns the session header on the first line of path
// in fsys; ok is false when the line is not one.
func readSessionHeader(fsys agent.FileSystem, path string) (e sessionEntry, ok bool) {
	f, err := fsys.Open(path)
	if err != nil {
		return sessionEntry{}, false
	}
	defer f.Close()
	line, _ := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if json.Unmarshal(line, &e) != nil || e.Type != "session" {
		return sessionEntry{}, false
	}
	return e, true
}

// isSessionPath reports whether a session ID is a session file path
// rather than an ID.
func isSessionPath(id string) bool {
	return strings.HasSuffix(id, ".jsonl") || strings.ContainsAny(id, `/\`)
}

// findSession returns the session file in fsys for a session ID or path.
func (p *Provider) findSession(fsys agent.FileSystem, sessionID, cwd string) (string, error) {
	if isSessionPath(sessionID) {
		if info, err := fsys.Stat(sessionID); err == nil && !info.IsDir() {
			return sessionID, nil
		}
	}
	abs := providerutil.AbsDir(cwd)
	root, perCwd := p.sessionsDir(fsys, abs)
	// Pi names files "<timestamp>_<id>.jsonl" and IDs may contain "_", so
	// the name only narrows the search: the header must carry the ID.
	suffix := "_" + sessionID + ".jsonl"
	matches := func(path string) bool {
		if !strings.HasSuffix(filepath.Base(path), suffix) {
			return false
		}
		e, ok := readSessionHeader(fsys, path)
		return ok && e.ID == sessionID
	}
	if cwd != "" && perCwd {
		candidates, _ := providerkit.Glob(fsys, filepath.Join(root, cwdDirName(abs), "*"+suffix))
		for _, path := range candidates {
			if matches(path) {
				return path, nil
			}
		}
	}
	for _, f := range sessionFiles(fsys, root) {
		if matches(f.Path) {
			return f.Path, nil
		}
	}
	return "", fmt.Errorf("pi: session %s: %w", sessionID, agent.ErrSessionNotFound)
}
