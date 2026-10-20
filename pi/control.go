package pi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// State is the get_state result.
type State struct {
	Model                 *modelInfo `json:"model"`
	ThinkingLevel         string     `json:"thinkingLevel"`
	IsStreaming           bool       `json:"isStreaming"`
	IsCompacting          bool       `json:"isCompacting"`
	SteeringMode          string     `json:"steeringMode"`
	FollowUpMode          string     `json:"followUpMode"`
	SessionFile           string     `json:"sessionFile"`
	SessionID             string     `json:"sessionId"`
	SessionName           string     `json:"sessionName"`
	AutoCompactionEnabled bool       `json:"autoCompactionEnabled"`
	MessageCount          int        `json:"messageCount"`
	PendingMessageCount   int        `json:"pendingMessageCount"`
	// Raw holds the full result.
	Raw json.RawMessage `json:"-"`
}

type modelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Reasoning     bool   `json:"reasoning"`
	ContextWindow int64  `json:"contextWindow"`
	MaxTokens     int64  `json:"maxTokens"`
	// Input lists accepted input kinds, such as "text" and "image".
	Input []string `json:"input"`
	// ThinkingLevelMap maps Pi thinking levels to provider values; null
	// marks a level the model lacks.
	ThinkingLevelMap map[string]*json.RawMessage `json:"thinkingLevelMap"`
}

// commandScope maps Pi's source scopes to the uniform "user" and
// "project".
func commandScope(scope string) string {
	switch scope {
	case "global", "personal", "user":
		return "user"
	case "workspace", "local", "project":
		return "project"
	}
	return scope
}

// thinkingLevels is Pi's thinking ladder, lowest first.
var thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// efforts lists the thinking levels a model accepts. xhigh and max are
// opt-in through the level map.
func (m *modelInfo) efforts() []agent.Effort {
	if !m.Reasoning {
		return nil
	}
	var out []agent.Effort
	for _, level := range thinkingLevels {
		mapped, listed := m.ThinkingLevelMap[level]
		if listed && mapped == nil {
			continue
		}
		if !listed && (level == "xhigh" || level == "max") {
			continue
		}
		out = append(out, effortOf(level))
	}
	return out
}

func effortOf(level string) agent.Effort {
	if level == "off" {
		return agent.EffortNone
	}
	return agent.Effort(level)
}

// clampEffort returns want if the model accepts it, else the nearest
// higher accepted level, else the nearest lower one, as Pi clamps
// thinking levels.
func clampEffort(want agent.Effort, accepted []agent.Effort) agent.Effort {
	if want == "" || len(accepted) == 0 {
		return ""
	}
	if slices.Contains(accepted, want) {
		return want
	}
	i := slices.Index(thinkingLevels, thinkingLevel(want))
	if i < 0 {
		return ""
	}
	for j := i + 1; j < len(thinkingLevels); j++ {
		if slices.Contains(accepted, effortOf(thinkingLevels[j])) {
			return effortOf(thinkingLevels[j])
		}
	}
	for j := i - 1; j >= 0; j-- {
		if slices.Contains(accepted, effortOf(thinkingLevels[j])) {
			return effortOf(thinkingLevels[j])
		}
	}
	return accepted[0]
}

// State returns the session state.
func (s *Session) State(ctx context.Context) (*State, error) {
	raw, err := s.Command(ctx, "get_state", nil)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("pi: decode get_state: %w", err)
	}
	st.Raw = raw
	return &st, nil
}

func (s *Session) applyState(st *State) {
	s.rt.SetSessionID(st.SessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.ID = st.SessionID
	s.info.Provider = Name
	if s.info.Cwd == "" {
		// Set at startup; refreshSwitched follows session switches.
		s.info.Cwd = providerutil.AbsDir(s.cfg.Cwd)
	}
	// Without a mode Pi runs every tool, as bypassPermissions does.
	s.info.PermissionMode = cmp.Or(s.mode, agent.PermissionBypass)
	s.info.Title = st.SessionName
	s.info.Effort = effortOf(st.ThinkingLevel)
	if st.Model != nil {
		s.provName = st.Model.Provider
		s.window = st.Model.ContextWindow
		s.modelInput = st.Model.Input
		s.info.Model = st.Model.Provider + "/" + st.Model.ID
	}
	s.info.Raw = st.Raw
	for _, t := range s.cfg.Tools {
		if !slices.Contains(s.info.Tools, t.Name) {
			s.info.Tools = append(s.info.Tools, t.Name)
		}
	}
}

// followSessionCwd sets Info.Cwd to the directory a session file's header
// records, which Pi runs the session in; without one Info.Cwd stays.
func (s *Session) followSessionCwd(file string) {
	if cwd := sessionFileCwd(agent.FileSystemOf(s.provider.opts.Spawner), file); cwd != "" {
		s.mu.Lock()
		s.info.Cwd = cwd
		s.mu.Unlock()
	}
}

// loadCommands reads the session's commands into Info.Commands. Commands
// also report whether the bridge is loaded, which in-place rewinds and
// gating permission modes need.
func (s *Session) loadCommands(ctx context.Context) {
	if cmds, err := s.Commands(ctx); err == nil {
		s.mu.Lock()
		s.info.Commands = cmds
		s.mu.Unlock()
	}
}

// sessionFileCwd returns the working directory a session file's header
// records, or "".
func sessionFileCwd(fsys agent.FileSystem, path string) string {
	if path == "" {
		return ""
	}
	e, _ := readSessionHeader(fsys, path)
	return e.Cwd
}

// refreshSwitched reloads state after Pi replaced the session. Pi reloads
// extensions, prompts, and skills for the new session from its own
// directory, so the working directory and commands are read again.
func (s *Session) refreshSwitched(ctx context.Context) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	s.applyState(st)
	s.followSessionCwd(st.SessionFile)
	s.loadCommands(ctx)
	s.rt.Emit(&agent.SessionUpdated{EventMeta: agent.EventMeta{Raw: st.Raw}, Info: s.Info()})
	if err := s.checkGate(s.permissionMode()); err != nil {
		s.rt.Emit(&agent.Notice{Level: agent.NoticeError, Code: "bridge_unloaded",
			Message: err.Error() + "; Send refuses turns until the bridge loads again or the mode gates nothing"})
	}
	return nil
}

// refresh reloads state after commands that change the session.
func (s *Session) refresh(ctx context.Context) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	s.applyState(st)
	s.rt.Emit(&agent.SessionUpdated{EventMeta: agent.EventMeta{Raw: st.Raw}, Info: s.Info()})
	return nil
}

// FollowUp queues a message in Pi's follow-up queue, delivered when the
// agent would otherwise stop. It runs within the current turn.
func (s *Session) FollowUp(ctx context.Context, message string) error {
	_, err := s.Command(ctx, "follow_up", map[string]any{"message": message})
	return err
}

// ClearQueue removes Pi's queued steering and follow-up messages and the
// library's queued input. It returns Pi's queued texts.
func (s *Session) ClearQueue(ctx context.Context) (steering, followUp []string, err error) {
	s.rt.ClearQueue()
	running, _ := s.rt.Running()
	s.mu.Lock()
	maps.DeleteFunc(s.builtins, func(id string, _ bool) bool { return id != running })
	s.mu.Unlock()
	raw, err := s.Command(ctx, "clear_queue", nil)
	if err != nil {
		return nil, nil, err
	}
	var data struct {
		Steering []string `json:"steering"`
		FollowUp []string `json:"followUp"`
	}
	err = json.Unmarshal(raw, &data)
	return data.Steering, data.FollowUp, err
}

// NewSession starts a fresh session in the same process. parentSession
// optionally records the session file it continues. It reports whether an
// extension cancelled the switch. Like every session switch, it fails while
// a turn runs or input is queued, and ctx bounds only the time before the
// command is sent.
func (s *Session) NewSession(ctx context.Context, parentSession string) (cancelled bool, err error) {
	payload := map[string]any{}
	if parentSession != "" {
		payload["parentSession"] = parentSession
	}
	return s.switchCommand(ctx, "new_session", payload)
}

// SwitchSession loads another session file. It fails while a turn runs or
// input is queued, and ctx bounds only the time before the command is sent.
func (s *Session) SwitchSession(ctx context.Context, sessionPath string) (cancelled bool, err error) {
	return s.switchCommand(ctx, "switch_session", map[string]any{"sessionPath": sessionPath})
}

// ForkAt creates a new session branching from a user message entry and
// switches this process to it. It returns the text of the message forked
// from. ForkSession instead leaves this session as it is. Like the other
// session switches, it fails while a turn runs or input is queued, and ctx
// bounds only the time before the command is sent.
func (s *Session) ForkAt(ctx context.Context, entryID string) (text string, cancelled bool, err error) {
	err = s.exclusive(ctx, "pi: cannot fork while a turn runs or input is queued", func(string) (string, agent.Usage, error) {
		var err error
		text, cancelled, err = s.forkAt(ctx, entryID)
		return "", agent.Usage{}, err
	})
	return text, cancelled, err
}

// forkAt runs the fork command; callers hold the slot or own the session.
func (s *Session) forkAt(ctx context.Context, entryID string) (text string, cancelled bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	// Once written, the fork goes ahead whatever happens to ctx, and the
	// slot must stay taken until Pi answered.
	ctx = context.WithoutCancel(ctx)
	raw, err := s.Command(ctx, "fork", map[string]any{"entryId": entryID})
	if err != nil {
		return "", false, err
	}
	var data struct {
		Text      string `json:"text"`
		Cancelled bool   `json:"cancelled"`
	}
	_ = json.Unmarshal(raw, &data)
	if !data.Cancelled {
		err = s.refreshSwitched(ctx)
	}
	return data.Text, data.Cancelled, err
}

// Clone duplicates the active branch into a new session and switches to it.
// It fails while a turn runs or input is queued, and ctx bounds only the
// time before the command is sent.
func (s *Session) Clone(ctx context.Context) (cancelled bool, err error) {
	return s.switchCommand(ctx, "clone", nil)
}

// switchCommand replaces the session with command typ. Pi aborts the
// running run and rebinds its event stream to the new session, so input
// started meanwhile would run unobserved; the switch holds the slot, and
// fails while a turn runs or input is queued.
func (s *Session) switchCommand(ctx context.Context, typ string, payload map[string]any) (cancelled bool, err error) {
	err = s.exclusive(ctx, "pi: cannot switch sessions while a turn runs or input is queued", func(string) (string, agent.Usage, error) {
		if err := ctx.Err(); err != nil {
			return "", agent.Usage{}, err
		}
		// Once written, the switch goes ahead whatever happens to ctx, so
		// the reply is awaited and the state read before the slot frees.
		ctx := context.WithoutCancel(ctx)
		raw, err := s.Command(ctx, typ, payload)
		if err != nil {
			return "", agent.Usage{}, err
		}
		var data struct {
			Cancelled bool `json:"cancelled"`
		}
		_ = json.Unmarshal(raw, &data)
		if cancelled = data.Cancelled; cancelled {
			return "", agent.Usage{}, nil
		}
		if typ == "new_session" || typ == "switch_session" {
			// The turns of the previous session no longer resolve, and
			// its grants do not carry over to another conversation.
			s.forgetTurns()
			s.mu.Lock()
			clear(s.allowed)
			s.mu.Unlock()
		}
		return "", agent.Usage{}, s.refreshSwitched(ctx)
	})
	return cancelled, err
}

// ForkPoint is a user message available for forking.
type ForkPoint struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

// ForkPoints lists user messages available for ForkAt.
func (s *Session) ForkPoints(ctx context.Context) ([]ForkPoint, error) {
	raw, err := s.Command(ctx, "get_fork_messages", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Messages []ForkPoint `json:"messages"`
	}
	err = json.Unmarshal(raw, &data)
	return data.Messages, err
}

// Messages returns the conversation messages as native AgentMessage values.
func (s *Session) Messages(ctx context.Context) ([]json.RawMessage, error) {
	raw, err := s.Command(ctx, "get_messages", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Messages []json.RawMessage `json:"messages"`
	}
	err = json.Unmarshal(raw, &data)
	return data.Messages, err
}

// Entries returns session entries after the entry with ID since, or all
// entries when since is empty, plus the current leaf entry ID.
func (s *Session) Entries(ctx context.Context, since string) (entries []json.RawMessage, leafID string, err error) {
	payload := map[string]any{}
	if since != "" {
		payload["since"] = since
	}
	raw, err := s.Command(ctx, "get_entries", payload)
	if err != nil {
		return nil, "", err
	}
	var data struct {
		Entries []json.RawMessage `json:"entries"`
		LeafID  *string           `json:"leafId"`
	}
	err = json.Unmarshal(raw, &data)
	if data.LeafID != nil {
		leafID = *data.LeafID
	}
	return data.Entries, leafID, err
}

// Tree returns the session entry tree.
func (s *Session) Tree(ctx context.Context) (json.RawMessage, error) {
	return s.Command(ctx, "get_tree", nil)
}

// LastAssistantText returns the text of the last assistant message.
func (s *Session) LastAssistantText(ctx context.Context) (string, error) {
	raw, err := s.Command(ctx, "get_last_assistant_text", nil)
	if err != nil {
		return "", err
	}
	var data struct {
		Text *string `json:"text"`
	}
	_ = json.Unmarshal(raw, &data)
	if data.Text == nil {
		return "", nil
	}
	return *data.Text, nil
}

// SetTitle sets the session display name.
func (s *Session) SetTitle(ctx context.Context, name string) error {
	if _, err := s.Command(ctx, "set_session_name", map[string]any{"name": name}); err != nil {
		return err
	}
	s.updateInfo(agent.EventMeta{}, func(info *agent.SessionInfo) { info.Title = name })
	return nil
}

// AvailableModels lists configured models as native Model objects.
func (s *Session) AvailableModels(ctx context.Context) ([]json.RawMessage, error) {
	raw, err := s.Command(ctx, "get_available_models", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Models []json.RawMessage `json:"models"`
	}
	err = json.Unmarshal(raw, &data)
	return data.Models, err
}

// CycleModel switches to the next model in the cycle list.
func (s *Session) CycleModel(ctx context.Context) (json.RawMessage, error) {
	raw, err := s.Command(ctx, "cycle_model", nil)
	if err == nil {
		err = s.refresh(ctx)
	}
	return raw, err
}

// ThinkingLevels lists the thinking levels the current model supports.
func (s *Session) ThinkingLevels(ctx context.Context) ([]string, error) {
	raw, err := s.Command(ctx, "get_available_thinking_levels", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Levels []string `json:"levels"`
	}
	err = json.Unmarshal(raw, &data)
	return data.Levels, err
}

// CycleThinkingLevel switches to the next thinking level.
func (s *Session) CycleThinkingLevel(ctx context.Context) (string, error) {
	raw, err := s.Command(ctx, "cycle_thinking_level", nil)
	if err != nil {
		return "", err
	}
	var data struct {
		Level string `json:"level"`
	}
	_ = json.Unmarshal(raw, &data)
	return data.Level, s.refresh(ctx)
}

// SetSteeringMode sets "all" or "one-at-a-time" delivery of steering
// messages.
func (s *Session) SetSteeringMode(ctx context.Context, mode string) error {
	_, err := s.Command(ctx, "set_steering_mode", map[string]any{"mode": mode})
	return err
}

// SetFollowUpMode sets "all" or "one-at-a-time" delivery of follow-ups.
func (s *Session) SetFollowUpMode(ctx context.Context, mode string) error {
	_, err := s.Command(ctx, "set_follow_up_mode", map[string]any{"mode": mode})
	return err
}

// CompactionResult is the result of Compact.
type CompactionResult struct {
	Summary              string          `json:"summary"`
	FirstKeptEntryID     string          `json:"firstKeptEntryId"`
	TokensBefore         int64           `json:"tokensBefore"`
	EstimatedTokensAfter int64           `json:"estimatedTokensAfter"`
	Usage                json.RawMessage `json:"usage"`
	Details              json.RawMessage `json:"details"`
}

// usage returns the token usage and cost of the compaction's model call.
func (r *CompactionResult) usage() agent.Usage {
	var u agent.Usage
	var p piUsage
	if json.Unmarshal(r.Usage, &p) == nil {
		u = p.usage()
	}
	return u
}

// Compact compacts the conversation, optionally with instructions for the
// summary.
func (s *Session) Compact(ctx context.Context, instructions string) error {
	_, err := s.CompactWithResult(ctx, instructions)
	return err
}

// ForkSession starts a new Pi process on a copy of this conversation and
// leaves this session as it is. It needs a persisted session (not
// Ephemeral). Empty fields of cfg are taken from this session.
func (s *Session) ForkSession(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	if s.cfg.Ephemeral {
		return nil, providerkit.Unsupported(Name, "forking an ephemeral session")
	}
	cfg = providerkit.InheritConfig(cfg, s.cfg, s.Info())
	if cfg.ForkAtTurn != "" {
		entry, err := s.userEntry(ctx, cfg.ForkAtTurn)
		if err != nil {
			return nil, err
		}
		cfg.ForkAtTurn = entry
	}
	cfg.Resume = s.ID()
	cfg.Fork = true
	return s.provider.Start(ctx, cfg)
}

// CompactWithResult compacts the conversation and returns Pi's summary of
// the compaction.
//
// The compaction holds the running slot as a turn of its own, as /compact
// sent with Send does, so input sent meanwhile is queued rather than
// refused by Pi; it fails while a turn runs or input is queued.
func (s *Session) CompactWithResult(ctx context.Context, instructions string) (*CompactionResult, error) {
	var r *CompactionResult
	err := s.exclusive(ctx, "pi: cannot compact while a turn runs or input is queued; send /compact to queue it", func(id string) (string, agent.Usage, error) {
		var err error
		if r, err = s.compact(ctx, id, instructions); err != nil {
			return "", agent.Usage{}, err
		}
		return r.Summary, r.usage(), nil
	})
	return r, err
}

// exclusive runs fn while holding the running slot as a built-in turn of
// its own, so input sent meanwhile is queued and waits for it rather than
// reaching Pi during the operation. It fails while a turn runs or input is
// queued, with the error message busy. fn returns the turn's text and
// usage.
func (s *Session) exclusive(ctx context.Context, busy string, fn func(id string) (string, agent.Usage, error)) error {
	id := providerutil.NewUUID()
	defer s.forgetBuiltin(id)
	_, started, err := s.rt.StartIdle(ctx, id, func(context.Context) error {
		// Marked once the turn holds the slot, so ClearQueue cannot drop it.
		s.mu.Lock()
		s.builtins[id] = true
		s.mu.Unlock()
		return nil
	})
	if !started {
		return errors.New(busy)
	}
	if err != nil {
		return err
	}
	text, usage, err := fn(id)
	res := &agent.TurnResult{Status: agent.TurnSucceeded, StopReason: "handled", Text: text, Usage: usage}
	if err != nil {
		res = s.builtinFailure(id, err)
	}
	s.rt.FinishTurn(id, res)
	return err
}

// compact runs Pi's compact command for built-in turn id. When ctx ends it
// aborts the compaction, and it returns only after Pi answered, so the
// turn holding the slot never ends while Pi still compacts.
func (s *Session) compact(ctx context.Context, id, instructions string) (*CompactionResult, error) {
	if interrupted, _ := s.rt.Interrupting(); interrupted == id {
		// Interrupted before the compaction began; Pi ignored the abort.
		return nil, errors.New("pi: compaction interrupted")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload := map[string]any{}
	if instructions != "" {
		payload["customInstructions"] = instructions
	}
	done := make(chan struct{})
	var aborter sync.WaitGroup
	aborter.Add(1)
	go func() {
		defer aborter.Done()
		select {
		case <-ctx.Done():
			_, _ = s.Command(context.WithoutCancel(ctx), "abort", nil)
		case <-done:
		}
	}()
	// The abort, if sent, is answered before compact returns, so it can
	// never stop the input that starts next.
	defer aborter.Wait()
	defer close(done)
	raw, err := s.Command(context.WithoutCancel(ctx), "compact", payload)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	var r CompactionResult
	err = json.Unmarshal(raw, &r)
	return &r, err
}

// SetAutoCompaction turns automatic compaction on or off.
func (s *Session) SetAutoCompaction(ctx context.Context, enabled bool) error {
	_, err := s.Command(ctx, "set_auto_compaction", map[string]any{"enabled": enabled})
	return err
}

// SetAutoRetry turns automatic retry of transient errors on or off.
func (s *Session) SetAutoRetry(ctx context.Context, enabled bool) error {
	_, err := s.Command(ctx, "set_auto_retry", map[string]any{"enabled": enabled})
	return err
}

// AbortRetry cancels a pending retry.
func (s *Session) AbortRetry(ctx context.Context) error {
	_, err := s.Command(ctx, "abort_retry", nil)
	return err
}

// BashResult is the outcome of Bash.
type BashResult struct {
	Output         string `json:"output"`
	ExitCode       *int   `json:"exitCode"`
	Cancelled      bool   `json:"cancelled"`
	Truncated      bool   `json:"truncated"`
	FullOutputPath string `json:"fullOutputPath"`
	// ItemID is the item of the ToolOutputDelta events that streamed the
	// output.
	ItemID string `json:"-"`
}

// Bash runs a shell command and adds its output to the conversation for the
// next prompt, unless excludeFromContext is set. The command is a tool call
// item, named by BashResult.ItemID, whose output streams as ToolOutputDelta
// events and which completes when Pi answers. The command keeps running
// when ctx ends; call AbortBash to stop it.
func (s *Session) Bash(ctx context.Context, command string, excludeFromContext bool) (*BashResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload := map[string]any{"command": command}
	if excludeFromContext {
		payload["excludeFromContext"] = true
	}
	id := s.newRequestID()
	input, _ := json.Marshal(map[string]any{"command": command})
	item := agent.Item{ID: "bash-" + id, Kind: agent.ItemToolCall, Status: agent.ItemRunning,
		Tool: &agent.ToolCall{Name: "bash", Kind: agent.ToolShell, Command: command, Input: input}}
	s.rt.Emit(&agent.ItemStarted{Item: item.Clone()})
	var completed atomic.Bool
	complete := func(resp rpcResponse) {
		completed.Store(true)
		done := item.Clone()
		var r BashResult
		if !resp.Success || json.Unmarshal(resp.Data, &r) != nil {
			done.Status = agent.ItemFailed
			done.Tool.IsError = true
			done.Tool.Output = resp.Error
		} else {
			bashOutcome(&done, r.Output, r.ExitCode, r.Cancelled)
		}
		s.rt.Emit(&agent.ItemCompleted{EventMeta: agent.EventMeta{Raw: resp.Data}, Item: done})
	}
	type outcome struct {
		raw json.RawMessage
		err error
	}
	// The call outlives ctx, so the item completes when Pi answers; the
	// completion runs on the reading goroutine, after the output deltas.
	ch := make(chan outcome, 1)
	go func() {
		raw, err := s.callOrdered(context.WithoutCancel(ctx), id, "bash", payload, complete)
		if err != nil && !completed.Load() {
			done := item.Clone()
			done.Status = agent.ItemFailed
			done.Tool.IsError = true
			done.Tool.Output = err.Error()
			s.rt.Emit(&agent.ItemCompleted{Item: done})
		}
		ch <- outcome{raw, err}
	}()
	var o outcome
	select {
	case o = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if o.err != nil {
		return nil, o.err
	}
	r := BashResult{ItemID: item.ID}
	err := json.Unmarshal(o.raw, &r)
	return &r, err
}

// bashOutcome fills a shell item from a bash command's result: a cancelled
// command is cancelled, and a nonzero exit code is an error.
func bashOutcome(item *agent.Item, output string, exitCode *int, cancelled bool) {
	item.Tool.Output = output
	item.Tool.ExitCode = exitCode
	item.Status = agent.ItemDone
	if cancelled {
		item.Status = agent.ItemCancelled
	} else if exitCode != nil && *exitCode != 0 {
		item.Tool.IsError = true
	}
}

// AbortBash stops a running Bash command.
func (s *Session) AbortBash(ctx context.Context) error {
	_, err := s.Command(ctx, "abort_bash", nil)
	return err
}

// Stats is the get_session_stats result.
type Stats struct {
	SessionFile       string `json:"sessionFile"`
	SessionID         string `json:"sessionId"`
	UserMessages      int    `json:"userMessages"`
	AssistantMessages int    `json:"assistantMessages"`
	ToolCalls         int    `json:"toolCalls"`
	ToolResults       int    `json:"toolResults"`
	TotalMessages     int    `json:"totalMessages"`
	Tokens            struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		CacheRead  int64 `json:"cacheRead"`
		CacheWrite int64 `json:"cacheWrite"`
		Total      int64 `json:"total"`
	} `json:"tokens"`
	Cost         float64 `json:"cost"`
	ContextUsage *struct {
		Tokens        *int64   `json:"tokens"`
		ContextWindow int64    `json:"contextWindow"`
		Percent       *float64 `json:"percent"`
	} `json:"contextUsage"`
}

// Stats returns token, cost, and context statistics for the session.
func (s *Session) Stats(ctx context.Context) (*Stats, error) {
	raw, err := s.Command(ctx, "get_session_stats", nil)
	if err != nil {
		return nil, err
	}
	var st Stats
	err = json.Unmarshal(raw, &st)
	return &st, err
}

// ExportHTML writes the session as HTML and returns the file path. An empty
// outputPath lets Pi choose.
func (s *Session) ExportHTML(ctx context.Context, outputPath string) (string, error) {
	payload := map[string]any{}
	if outputPath != "" {
		payload["outputPath"] = outputPath
	}
	raw, err := s.Command(ctx, "export_html", payload)
	if err != nil {
		return "", err
	}
	var data struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(raw, &data)
	return data.Path, nil
}

// builtinCommands are Pi terminal commands that Send runs through RPC.
var builtinCommands = []agent.SlashCommand{
	{Name: "compact", Description: "Compact the conversation", ArgumentHint: "[instructions]", Kind: "command", Scope: "builtin"},
	{Name: "autocompact", Description: "Turn automatic compaction on or off", ArgumentHint: "[on|off|toggle]", Kind: "command", Scope: "builtin"},
}

// Commands lists extension commands, prompt templates, skills, and the
// built-in commands Send runs through RPC.
func (s *Session) Commands(ctx context.Context) ([]agent.SlashCommand, error) {
	raw, err := s.Command(ctx, "get_commands", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
			SourceInfo  struct {
				Path  string `json:"path"`
				Scope string `json:"scope"`
			} `json:"sourceInfo"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	out := make([]agent.SlashCommand, 0, len(data.Commands)+len(builtinCommands))
	bridge := false
	for _, c := range data.Commands {
		if c.Name == rewindCommand && c.Source == "extension" {
			bridge = true
			continue
		}
		out = append(out, agent.SlashCommand{Name: c.Name, Description: c.Description, Kind: c.Source,
			Path: c.SourceInfo.Path, Scope: commandScope(c.SourceInfo.Scope)})
	}
	// Pi loads extensions again for every session it switches to.
	s.bridgeLoaded.Store(bridge)
	for _, b := range builtinCommands {
		if !slices.ContainsFunc(out, func(c agent.SlashCommand) bool { return c.Name == b.Name }) {
			out = append(out, b)
		}
	}
	return out, nil
}
