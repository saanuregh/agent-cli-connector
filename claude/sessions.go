package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// projectDirLimit is the longest project directory name before the SDK
// truncates it and appends a hash.
const projectDirLimit = 200

// projectDirName returns the directory Claude Code stores a working
// directory's sessions in, ported from the Agent SDK: every UTF-16 code
// unit other than an ASCII letter or digit becomes "-", and a name longer
// than projectDirLimit is cut there and suffixed with a hash of cwd. The
// SDK's NFC normalization on macOS is not applied.
func projectDirName(cwd string) string {
	units := utf16.Encode([]rune(cwd))
	b := make([]byte, len(units))
	for i, c := range units {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b[i] = byte(c)
		} else {
			b[i] = '-'
		}
	}
	if len(b) <= projectDirLimit {
		return string(b)
	}
	// The SDK's hash: a 32-bit Java-style string hash in base 36.
	var h int32
	for _, c := range units {
		h = h<<5 - h + int32(c)
	}
	abs := int64(h)
	if abs < 0 {
		abs = -abs
	}
	return string(b[:projectDirLimit]) + "-" + strconv.FormatInt(abs, 36)
}

// projectDirs returns the directories that may hold cwd's sessions: the
// one for the symlink-resolved path the CLI uses, then the one for the
// path as given when it differs.
func projectDirs(configDir, cwd string) []string {
	abs := providerutil.AbsDir(cwd)
	root := filepath.Join(configDir, "projects")
	dirs := []string{filepath.Join(root, projectDirName(abs))}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		dirs = append([]string{filepath.Join(root, projectDirName(real))}, dirs...)
	}
	return dirs
}

// latestSessionID returns the most recently modified session of cwd.
func latestSessionID(fsys agent.FileSystem, configDir, cwd string) (string, error) {
	var files []providerkit.SessionFile
	var err error
	for _, dir := range projectDirs(configDir, cwd) {
		if files, err = transcriptFiles(fsys, dir); err == nil {
			break
		}
	}
	if err != nil || len(files) == 0 {
		return "", err
	}
	best := slices.MaxFunc(files, func(a, b providerkit.SessionFile) int { return a.ModTime.Compare(b.ModTime) })
	return strings.TrimSuffix(filepath.Base(best.Path), ".jsonl"), nil
}

// transcriptFiles lists the session transcripts in dir with their mtimes.
func transcriptFiles(fsys agent.FileSystem, dir string) ([]providerkit.SessionFile, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []providerkit.SessionFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, providerkit.SessionFile{Path: filepath.Join(dir, e.Name()), ModTime: info.ModTime()})
	}
	return files, nil
}

// historyItemID names the history item of block of the entry uuid.
func historyItemID(uuid string, block int) string {
	if block == 0 {
		return uuid
	}
	return uuid + "/" + strconv.Itoa(block)
}

func toolResultCount(blocks []contentBlock) int {
	n := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			n++
		}
	}
	return n
}

// ListSessions lists persisted sessions, newest first, by reading the
// transcript files under the Claude config directory.
func (p *Provider) ListSessions(ctx context.Context, opts agent.ListSessionsOptions) ([]agent.SessionSummary, error) {
	fsys := p.fileSystem()
	configDir := p.configDir(agent.Config{})
	root := filepath.Join(configDir, "projects")
	var dirs []string
	if opts.Cwd != "" {
		dirs = projectDirs(configDir, opts.Cwd)
	} else {
		entries, err := fsys.ReadDir(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	var files []providerkit.SessionFile
	for _, dir := range dirs {
		dirFiles, _ := transcriptFiles(fsys, dir)
		files = append(files, dirFiles...)
	}
	return providerkit.PageSessionFiles(ctx, files, opts.Limit, opts.Offset, func(path string) (agent.SessionSummary, bool) {
		return summarize(fsys, path)
	})
}

type transcriptEntry struct {
	Type              string `json:"type"`
	UUID              string `json:"uuid"`
	ParentUUID        string `json:"parentUuid"`
	LogicalParentUUID string `json:"logicalParentUuid"`
	IsCompactSummary  bool   `json:"isCompactSummary"`
	Timestamp         string `json:"timestamp"`
	Cwd               string `json:"cwd"`
	IsSidechain       bool   `json:"isSidechain"`
	IsMeta            bool   `json:"isMeta"`
	IsSynthetic       bool   `json:"isSynthetic"`
	CustomTitle       string `json:"customTitle"`
	AITitle           string `json:"aiTitle"`
	Summary           string `json:"summary"`
	Subtype           string `json:"subtype"`
	CompactMetadata   *struct {
		Trigger   string `json:"trigger"`
		PreTokens int64  `json:"preTokens"`
	} `json:"compactMetadata"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Effort        string          `json:"effort"`
	Message       json.RawMessage `json:"message"`
}

func summarize(fsys agent.FileSystem, path string) (agent.SessionSummary, bool) {
	sum := agent.SessionSummary{
		ID:       strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Provider: Name,
		Path:     path,
	}
	var firstPrompt, customTitle, aiTitle string
	hasMessages := false
	err := eachTranscriptEntry(fsys, path, relevantLine, func(e transcriptEntry) error {
		switch e.Type {
		case "custom-title":
			customTitle = e.CustomTitle
		case "ai-title":
			aiTitle = e.AITitle
		case "summary":
			if aiTitle == "" {
				aiTitle = e.Summary
			}
		case "user", "assistant":
			hasMessages = true
			if sum.Cwd == "" {
				sum.Cwd = e.Cwd
			}
			if ts := providerutil.ParseTime(e.Timestamp); !ts.IsZero() {
				if sum.CreatedAt.IsZero() {
					sum.CreatedAt = ts
				}
				sum.UpdatedAt = ts
			}
			if e.IsSidechain {
				break
			}
			if e.Type == "assistant" {
				var m struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(e.Message, &m) == nil && m.Model != "" && m.Model != "<synthetic>" {
					sum.Model = m.Model
				}
				if e.Effort != "" {
					sum.Effort = agent.Effort(e.Effort)
				}
				break
			}
			if e.IsMeta || e.IsSynthetic || len(e.ToolUseResult) > 0 {
				break
			}
			if text := promptText(e.Message); text != "" {
				if firstPrompt == "" {
					firstPrompt = text
					sum.FirstPrompt = preview(text)
				}
				sum.LastPrompt = preview(text)
			}
		}
		return nil
	})
	if err != nil {
		return agent.SessionSummary{}, false
	}
	if !hasMessages {
		return sum, false
	}
	firstPrompt = cutAtRune(firstPrompt, 200)
	sum.Title = cmp.Or(customTitle, aiTitle, firstPrompt)
	return sum, true
}

// preview collapses whitespace and cuts text to previewLimit bytes.
func preview(text string) string {
	return cutAtRune(strings.Join(strings.Fields(text), " "), previewLimit)
}

// cutAtRune cuts text to at most limit bytes without splitting a rune.
func cutAtRune(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// previewLimit caps SessionSummary prompt previews.
const previewLimit = 160

// relevantLine skips the large API request and attachment records
// without decoding them.
func relevantLine(line []byte) bool {
	return !isAPIRequestLine(line) && !bytes.Contains(lineHead(line), []byte(`"type":"attachment"`))
}

func isNotAPIRequestLine(line []byte) bool {
	return !isAPIRequestLine(line)
}

// eachTranscriptEntry decodes each line of a transcript file that keep
// accepts and passes it to visit, stopping at the first error visit returns.
// Lines that do not decode are skipped.
func eachTranscriptEntry(fsys agent.FileSystem, path string, keep func([]byte) bool, visit func(transcriptEntry) error) error {
	f, err := fsys.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && keep(line) {
			var e transcriptEntry
			if json.Unmarshal(line, &e) == nil {
				if err := visit(e); err != nil {
					return err
				}
			}
		}
		if rerr != nil {
			return nil
		}
	}
}

func isAPIRequestLine(line []byte) bool {
	return bytes.Contains(lineHead(line), []byte(`"type":"api-request`))
}

func lineHead(line []byte) []byte {
	if len(line) > 200 {
		return line[:200]
	}
	return line
}

// promptText returns the first text of a user message that is a prompt:
// command wrappers become "/name args", and CLI noise, notifications, and
// other XML-wrapped text yield "".
func promptText(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	var texts []string
	var str string
	if json.Unmarshal(m.Content, &str) == nil {
		texts = []string{str}
	} else {
		var blocks []contentBlock
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
	}
	for _, t := range texts {
		t = strings.TrimSpace(normalizeCommand(t))
		if t == "" || isTranscriptNoise(t) || strings.HasPrefix(t, "<") {
			continue
		}
		return t
	}
	return ""
}

var (
	interruptPlaceholder = regexp.MustCompile(`^\[Request interrupted by user[^\]]*\]$`)
	localCommandOutput   = regexp.MustCompile(`^<local-command-(stdout|stderr)>[\s\S]*</local-command-(stdout|stderr)>$`)
	commandName          = regexp.MustCompile(`<command-name>([\s\S]*?)</command-name>`)
	commandMessage       = regexp.MustCompile(`<command-message>([\s\S]*?)</command-message>`)
	commandArgs          = regexp.MustCompile(`<command-args>([\s\S]*?)</command-args>`)
)

// isTranscriptNoise reports text the CLI records that is not conversation:
// interrupt placeholders, "No response requested.", and local command
// output.
func isTranscriptNoise(text string) bool {
	text = strings.TrimSpace(text)
	return interruptPlaceholder.MatchString(text) || text == "No response requested." || localCommandOutput.MatchString(text)
}

// normalizeCommand rewrites the CLI's slash-command wrapper to the
// "/name args" the user typed. Other text is returned unchanged.
func normalizeCommand(text string) string {
	m := commandName.FindStringSubmatch(text)
	if m == nil {
		m = commandMessage.FindStringSubmatch(text)
	}
	if m == nil {
		return text
	}
	name := strings.TrimSpace(m[1])
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if a := commandArgs.FindStringSubmatch(text); a != nil && strings.TrimSpace(a[1]) != "" {
		return name + " " + strings.TrimSpace(a[1])
	}
	return name
}

// ReadHistory reads a persisted session's transcript as items: the
// conversation along the parentUuid chain ending at the latest message, so
// rewound branches are left out, without meta and compact-summary
// messages or CLI noise such as interrupt placeholders. Compactions become
// ItemCompaction items and task list changes ItemPlan items whose Text is
// a checklist and whose Raw is the []agent.PlanStep JSON. Items carry the
// entry's Time and, as TurnID, the UUID of the prompt that started their
// turn. Slash-command wrappers read "/name args", and background-task
// notification prompts are Synthetic. A tool call without a result reads
// as cancelled once a later turn started. With opts.SubagentID it reads
// that subagent's transcript from the session's subagents directory
// instead.
//
// Tool call items have the IDs the live stream gives them (the tool_use
// ID). Message and reasoning items are named after their transcript entry
// UUID, unlike live items, which are named after the API message ID and
// block index because a streamed block starts before its entry UUID is
// known.
func (p *Provider) ReadHistory(ctx context.Context, sessionID string, opts agent.ReadHistoryOptions) ([]agent.Item, error) {
	fsys := p.fileSystem()
	path, err := findTranscript(fsys, p.configDir(agent.Config{}), sessionID, opts.Cwd)
	if err != nil {
		return nil, err
	}
	if opts.SubagentID != "" {
		if path, err = findSubagentTranscript(fsys, path, opts.SubagentID); err != nil {
			return nil, err
		}
	}
	g, err := readTranscriptGraph(ctx, fsys, path)
	if err != nil {
		return nil, err
	}
	h := &historyBuilder{tools: map[string]int{}}
	leaf := g.leaf()
	if opts.SubagentID != "" {
		// Subagent entries are all sidechain entries.
		for i := len(g.entries) - 1; i >= 0 && leaf < 0; i-- {
			if t := g.entries[i].Type; t == "user" || t == "assistant" {
				leaf = i
			}
		}
	}
	if leaf < 0 {
		return nil, nil
	}
	order := g.chain(leaf)
	if !g.linked() {
		order = make([]int, len(g.entries))
		for i := range order {
			order[i] = i
		}
	}
	for _, i := range order {
		e := g.entries[i]
		if e.IsSidechain && opts.SubagentID == "" {
			continue
		}
		if e.Type == "system" && e.Subtype == "compact_boundary" {
			h.compaction(e)
			continue
		}
		if (e.Type != "user" && e.Type != "assistant") || e.IsCompactSummary || e.IsMeta {
			continue
		}
		h.add(e)
	}
	h.endOrphanedCalls()
	return providerutil.LastPage(h.items, opts.Limit, opts.Offset), nil
}

// findSubagentTranscript returns <session>/subagents/[...]/agent-<id>.jsonl
// next to the session transcript.
func findSubagentTranscript(fsys agent.FileSystem, sessionPath, agentID string) (string, error) {
	root := filepath.Join(strings.TrimSuffix(sessionPath, ".jsonl"), "subagents")
	name := "agent-" + agentID + ".jsonl"
	var found string
	_ = providerkit.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", errors.New("claude: subagent " + agentID + " transcript not found")
	}
	return found, nil
}

// historyBuilder turns transcript entries into items.
type historyBuilder struct {
	items []agent.Item
	// tools maps a tool call waiting for its result to its item index.
	tools map[string]int
	// turn is the prompt UUID of the current turn, and plan the task list
	// TaskCreate, TaskUpdate, and TaskList maintain.
	turn string
	plan []agent.PlanStep
}

func (h *historyBuilder) item(e transcriptEntry, it agent.Item) {
	if it.Status == "" {
		it.Status = agent.ItemDone
	}
	it.TurnID = h.turn
	it.Time = providerutil.ParseTime(e.Timestamp)
	h.items = append(h.items, it)
}

func (h *historyBuilder) compaction(e transcriptEntry) {
	trigger := ""
	if e.CompactMetadata != nil {
		trigger = e.CompactMetadata.Trigger
	}
	raw, _ := json.Marshal(e)
	h.item(e, agent.Item{ID: e.UUID, Kind: agent.ItemCompaction, Text: trigger, Raw: raw})
}

func (h *historyBuilder) planItem(e transcriptEntry, id string, steps []agent.PlanStep) {
	h.item(e, agent.Item{ID: id + "/plan", Kind: agent.ItemPlan, Text: planText(steps), Raw: providerutil.Raw(steps)})
}

// planText renders steps as a Markdown checklist; "[-]" marks the step in
// progress.
func planText(steps []agent.PlanStep) string {
	var b strings.Builder
	for _, st := range steps {
		mark := " "
		switch st.Status {
		case agent.StepCompleted:
			mark = "x"
		case agent.StepInProgress:
			mark = "-"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", mark, st.Text)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (h *historyBuilder) add(e transcriptEntry) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &m) != nil {
		return
	}
	kind := agent.ItemUserMessage
	if e.Type == "assistant" {
		kind = agent.ItemAssistantMessage
	}
	var blocks []contentBlock
	plain := json.Unmarshal(m.Content, &blocks) != nil
	if plain {
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			return
		}
		blocks = []contentBlock{{Type: "text", Text: text}}
	}
	// toolUseResult belongs to the entry's tool result when it has one.
	structured := e.ToolUseResult
	if len(structured) == 0 || structured[0] != '{' ||
		toolResultCount(blocks) != 1 {
		structured = nil
	}
	if e.Type == "user" && slices.ContainsFunc(blocks, func(b contentBlock) bool { return b.Type != "tool_result" && !isTranscriptNoise(b.Text) }) {
		h.turn = e.UUID
	}
	for i, b := range blocks {
		id := historyItemID(e.UUID, i)
		switch b.Type {
		case "text":
			if b.Text == "" || isTranscriptNoise(b.Text) {
				continue
			}
			it := agent.Item{ID: id, Kind: kind, Text: b.Text, Raw: providerutil.Raw(b)}
			if e.Type == "user" {
				it.Text = normalizeCommand(b.Text)
				it.Synthetic = e.IsSynthetic || strings.HasPrefix(strings.TrimSpace(b.Text), "<task-notification>")
			}
			if plain {
				it.Raw = e.Message
			}
			h.item(e, it)
		case "thinking":
			h.item(e, agent.Item{ID: id, Kind: agent.ItemReasoning, Text: b.Thinking, Raw: providerutil.Raw(b)})
		case "tool_use", "server_tool_use", "mcp_tool_use":
			if b.Name == handbackTool {
				var in struct {
					Message string `json:"message"`
				}
				if json.Unmarshal(b.Input, &in) == nil && strings.TrimSpace(in.Message) != "" {
					h.item(e, agent.Item{ID: b.ID, Kind: agent.ItemAssistantMessage, Text: in.Message, Raw: providerutil.Raw(b)})
				}
				continue
			}
			call := toolCallOf(b, "")
			call.Input = b.Input
			fillToolDetails(call, b.Input)
			h.tools[b.ID] = len(h.items)
			h.item(e, agent.Item{ID: b.ID, Kind: agent.ItemToolCall, Status: agent.ItemRunning, Tool: call, Raw: providerutil.Raw(b)})
			if b.Name == "TodoWrite" {
				if steps, ok := todoSteps(b.Input); ok {
					h.planItem(e, b.ID, steps)
				}
			}
		case "tool_result":
			h.result(e, b.ToolUseID, b.Content, b.IsError, structured)
		default:
			if slices.Contains(serverToolResultTypes, b.Type) {
				h.result(e, b.ToolUseID, b.Content, b.IsError || isErrorContent(b.Content), nil)
			}
		}
	}
}

// result completes a tool call with its result the way the live stream
// does. Transcripts do not record why a call did not run, so only an
// interrupted shell call reads as cancelled. A background Agent launch
// reads as done, because its completion is a later prompt.
func (h *historyBuilder) result(e transcriptEntry, id string, content json.RawMessage, isError bool, structured json.RawMessage) {
	idx, ok := h.tools[id]
	if !ok {
		return
	}
	delete(h.tools, id)
	it := &h.items[idx]
	applyToolResult(it, content, isError, structured, "")
	if it.Status == agent.ItemRunning {
		it.Status = agent.ItemDone
	}
	if isError {
		return
	}
	// Like the live stream, the TaskCreate list is kept apart from
	// TodoWrite snapshots.
	if steps, ok := applyPlanTool(h.plan, it.Tool.Name, it.Tool.Input, structured); ok {
		h.plan = steps
		h.planItem(e, id, steps)
	}
}

// endOrphanedCalls cancels the tool calls that never got a result although
// a later turn started, such as after the CLI was stopped mid-call. Calls
// of the last turn may still be running.
func (h *historyBuilder) endOrphanedCalls() {
	for _, idx := range h.tools {
		if it := &h.items[idx]; it.TurnID != h.turn {
			it.Status = agent.ItemCancelled
		}
	}
}

func findTranscript(fsys agent.FileSystem, configDir, sessionID, cwd string) (string, error) {
	// Session IDs are UUIDs; anything else could name a path or a glob.
	if !isUUID(sessionID) {
		return "", fmt.Errorf("claude: session ID %q is not a UUID: %w", sessionID, agent.ErrSessionNotFound)
	}
	root := filepath.Join(configDir, "projects")
	name := sessionID + ".jsonl"
	if cwd != "" {
		for _, dir := range projectDirs(configDir, cwd) {
			path := filepath.Join(dir, name)
			if _, err := fsys.Stat(path); err == nil {
				return path, nil
			}
		}
	}
	matches, _ := providerkit.Glob(fsys, filepath.Join(root, "*", name))
	if len(matches) == 0 {
		return "", fmt.Errorf("claude: session %s: %w", sessionID, agent.ErrSessionNotFound)
	}
	return matches[0], nil
}

// SessionInfo returns the summary of one persisted session, reading only
// its transcript. cwd narrows the search to that working directory's
// project folder; empty searches every project. A session without
// messages reads as agent.ErrSessionNotFound.
func (p *Provider) SessionInfo(ctx context.Context, sessionID, cwd string) (*agent.SessionSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fsys := p.fileSystem()
	path, err := findTranscript(fsys, p.configDir(agent.Config{}), sessionID, cwd)
	if err != nil {
		return nil, err
	}
	sum, ok := summarize(fsys, path)
	if !ok {
		return nil, fmt.Errorf("claude: session %s has no messages: %w", sessionID, agent.ErrSessionNotFound)
	}
	if sum.UpdatedAt.IsZero() {
		if info, err := fsys.Stat(path); err == nil {
			sum.UpdatedAt = info.ModTime()
		}
	}
	return &sum, nil
}

// RenameSession sets a persisted session's title by appending a
// custom-title entry to its transcript, as the Agent SDK's renameSession
// does. Use SetTitle for a running session, whose CLI writes the same
// entry. cwd narrows the search as in SessionInfo. It is unsupported when
// Options.Spawner runs the CLI on another machine.
func (p *Provider) RenameSession(ctx context.Context, sessionID, cwd, title string) error {
	if !isUUID(sessionID) {
		return fmt.Errorf("claude: invalid session ID %q", sessionID)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("claude: title must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fsys := p.fileSystem()
	if fsys != agent.LocalFileSystem {
		return providerkit.Unsupported(Name, "RenameSession with a remote file system")
	}
	path, err := findTranscript(fsys, p.configDir(agent.Config{}), sessionID, cwd)
	if err != nil {
		return err
	}
	line, err := json.Marshal(map[string]any{"type": "custom-title", "customTitle": title, "sessionId": sessionID})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("claude: rename session: %w", err)
	}
	if info, err := f.Stat(); err == nil && info.Size() == 0 {
		f.Close()
		return fmt.Errorf("claude: session %s is empty: %w", sessionID, agent.ErrSessionNotFound)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("claude: rename session: %w", err)
	}
	return f.Close()
}

// DeleteSession removes a persisted session: its transcript and the
// folder next to it that holds subagent transcripts, as the Agent SDK's
// deleteSession does. It cannot be undone. cwd narrows the search as in
// SessionInfo. Do not delete a session a running process still writes. It
// is unsupported when Options.Spawner runs the CLI on another machine.
func (p *Provider) DeleteSession(ctx context.Context, sessionID, cwd string) error {
	if !isUUID(sessionID) {
		return fmt.Errorf("claude: invalid session ID %q", sessionID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fsys := p.fileSystem()
	if fsys != agent.LocalFileSystem {
		return providerkit.Unsupported(Name, "DeleteSession with a remote file system")
	}
	path, err := findTranscript(fsys, p.configDir(agent.Config{}), sessionID, cwd)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("claude: delete session: %w", err)
	}
	if err := os.RemoveAll(strings.TrimSuffix(path, ".jsonl")); err != nil {
		return fmt.Errorf("claude: delete session folder: %w", err)
	}
	return nil
}

// isUUID reports whether s has the canonical 8-4-4-4-12 hex form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
