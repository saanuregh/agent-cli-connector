package codex

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Session is one Codex thread. It implements agent.Session and adds
// Codex-specific thread operations.
type Session struct {
	provider *Provider
	client   *Client
	owned    bool
	cfg      agent.Config
	opts     SessionOptions
	// schema is Config.OutputSchema as sent, for turns without their own.
	schema json.RawMessage
	rt     *providerkit.Runtime
	// threadID changes when Rewind falls back to a fork.
	threadID atomic.Pointer[string]
	tools    map[string]agent.Tool
	tr       *translator

	mu     sync.Mutex
	info   agent.SessionInfo
	model  string
	effort agent.Effort
	plan   bool
	// sandbox is the thread's current native sandbox policy, and
	// workspaceWrite the latest workspace-write policy seen or read from
	// config, so mode changes keep writable roots and network access.
	sandbox        map[string]any
	workspaceWrite map[string]any
	readWorkspace  bool
	// context is the last AdditionalContext sent, restored after
	// compaction.
	context map[string]any
	// compacting is set while a Compact call waits for its compaction,
	// and compacted is closed when it ends.
	compacting bool
	compacted  chan struct{}

	closeOnce sync.Once
}

var (
	_ agent.Session         = (*Session)(nil)
	_ agent.Titler          = (*Session)(nil)
	_ agent.TurnInterrupter = (*Session)(nil)
	_ agent.Compacter       = (*Session)(nil)
	_ agent.Forker          = (*Session)(nil)
	_ agent.TaskStopper     = (*Session)(nil)
	_ agent.Rewinder        = (*Session)(nil)

	_ agent.RateLimitReader = (*Session)(nil)
)

// loadSkills fills Info.Commands with the skills available in the working
// directory and reports the change with SessionUpdated. It runs in the
// background; the session works without the list.
func (s *Session) loadSkills() {
	ctx, cancel := context.WithTimeout(s.rt.Context(), 30*time.Second)
	defer cancel()
	params := map[string]any{}
	if cwd := s.Info().Cwd; cwd != "" {
		params["cwds"] = []string{cwd}
	}
	var resp skillsListResponse
	if err := s.client.Call(ctx, "skills/list", params, &resp); err != nil {
		s.rt.Logger().Debug("listing skills failed", "error", err)
		return
	}
	var cmds []agent.SlashCommand
	for _, entry := range resp.Data {
		for _, sk := range entry.Skills {
			if !sk.Enabled {
				continue
			}
			short := sk.ShortDescription
			if sk.Interface != nil {
				short = cmp.Or(sk.Interface.ShortDescription, short)
			}
			cmds = append(cmds, agent.SlashCommand{Name: sk.Name, Description: cmp.Or(short, sk.Description), Kind: "skill", Path: sk.Path, Scope: sk.Scope})
		}
	}
	s.updateInfo(agent.EventMeta{}, func(i *agent.SessionInfo) { i.Commands = cmds })
}

// updateInfo applies change to the session info under s.mu and reports the
// result with SessionUpdated. change may also write other fields guarded by
// s.mu.
func (s *Session) updateInfo(meta agent.EventMeta, change func(*agent.SessionInfo)) {
	s.mu.Lock()
	change(&s.info)
	info := s.info
	s.mu.Unlock()
	meta.TurnID = ""
	s.rt.Emit(&agent.SessionUpdated{EventMeta: meta, Info: info})
}

// ownsTurn reports whether turnID is a turn of this thread.
func (s *Session) ownsTurn(turnID string) bool {
	return turnID != "" && s.rt.HasTurn(turnID)
}

// threadResponse is the common shape of thread/start, thread/resume, and
// thread/fork results.
type threadResponse struct {
	Thread struct {
		ID         string  `json:"id"`
		Name       *string `json:"name"`
		Cwd        string  `json:"cwd"`
		CLIVersion string  `json:"cliVersion"`
	} `json:"thread"`
	Model           string          `json:"model"`
	ModelProvider   string          `json:"modelProvider"`
	Cwd             string          `json:"cwd"`
	ApprovalPolicy  json.RawMessage `json:"approvalPolicy"`
	Sandbox         json.RawMessage `json:"sandbox"`
	ReasoningEffort *string         `json:"reasoningEffort"`
}

func (p *Provider) openSession(ctx context.Context, client *Client, owned bool, cfg agent.Config, opts SessionOptions) (*Session, error) {
	if err := refusedConfig(cfg, owned); err != nil {
		return nil, err
	}
	outputSchema, err := opts.outputSchema(cfg.OutputSchema)
	if err != nil {
		return nil, err
	}
	notice, err := providerkit.IgnoredConfig(Name, cfg, ignoredConfig(cfg, owned, !client.opts.DisableExperimentalAPI))
	if err != nil {
		return nil, err
	}
	s := &Session{
		provider: p,
		client:   client,
		owned:    owned,
		cfg:      cfg,
		opts:     opts,
		schema:   outputSchema,
		rt:       providerkit.NewRuntime(Name, cfg),
		tools:    make(map[string]agent.Tool, len(cfg.Tools)),
		effort:   cfg.Effort,
		plan:     cfg.PermissionMode == agent.PermissionPlan,
	}
	if notice != nil {
		s.rt.Emit(notice)
	}
	for _, t := range cfg.Tools {
		s.tools[t.Name] = t
	}
	s.tr = newTranslator(s)
	s.rt.SetStarter(s.startQueued)

	params := s.threadParams(cfg)
	method := "thread/start"
	switch {
	case cfg.Resume != "" && cfg.Fork:
		method = "thread/fork"
		params["threadId"] = cfg.Resume
	case cfg.Resume != "":
		method = "thread/resume"
		params["threadId"] = cfg.Resume
	}
	raw, err := client.CallRaw(ctx, method, params)
	if err != nil && method == "thread/resume" && isArchivedError(err) {
		if uerr := client.UnarchiveThread(ctx, cfg.Resume); uerr == nil {
			raw, err = client.CallRaw(ctx, method, params)
		}
	}
	if err != nil {
		if method != "thread/start" && isNoRollout(err) {
			return nil, fmt.Errorf("codex: %s %s: %w: %w", method, cfg.Resume, agent.ErrSessionNotFound, err)
		}
		return nil, fmt.Errorf("codex: %s: %w", method, err)
	}
	var resp threadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("codex: decode %s: %w", method, err)
	}
	s.setThreadID(resp.Thread.ID)
	s.rememberSandbox(resp.Sandbox)
	s.model = resp.Model
	if s.effort == "" && resp.ReasoningEffort != nil {
		s.effort = agent.Effort(*resp.ReasoningEffort)
	}
	s.rt.SetSessionID(s.ID())
	title := cfg.Title
	if resp.Thread.Name != nil && title == "" {
		title = *resp.Thread.Name
	}
	s.info = agent.SessionInfo{
		ID:             s.ID(),
		Provider:       Name,
		Cwd:            cmp.Or(resp.Cwd, resp.Thread.Cwd, cfg.Cwd),
		Model:          resp.Model,
		Effort:         s.effort,
		PermissionMode: cmp.Or(cfg.PermissionMode, modeOf(resp.ApprovalPolicy, resp.Sandbox)),
		Title:          title,
		Version:        cmp.Or(resp.Thread.CLIVersion, client.Version()),
		Raw:            raw,
	}
	for _, t := range cfg.Tools {
		s.info.Tools = append(s.info.Tools, t.Name)
	}
	client.register(s)

	go s.loadSkills()
	if cfg.Title != "" && method == "thread/start" {
		if err := s.SetTitle(ctx, cfg.Title); err != nil {
			s.rt.Logger().Warn("setting thread name failed", "error", err)
		}
	}
	s.rt.Started(&agent.SessionStarted{EventMeta: agent.EventMeta{Raw: raw}, Info: s.Info(), Resumed: cfg.Resume != ""})
	return s, nil
}

// modeOf names the permission mode an approval policy and sandbox amount
// to: a shared mode where one matches, else the native approval policy.
func modeOf(approval, sandbox json.RawMessage) agent.PermissionMode {
	var policy string
	if json.Unmarshal(approval, &policy) != nil {
		return ""
	}
	var box sandboxPolicyType
	_ = json.Unmarshal(sandbox, &box)
	switch {
	case policy == "on-request" && box.Type == "readOnly":
		return agent.PermissionReadOnly
	case policy == "on-request" && box.Type == "workspaceWrite":
		return agent.PermissionDefault
	case policy == "never" && box.Type == "workspaceWrite":
		return agent.PermissionDontAsk
	case policy == "never" && box.Type == "dangerFullAccess":
		return agent.PermissionBypass
	}
	return agent.PermissionMode(policy)
}

// isNoRollout reports an answer for a thread that has no stored rollout.
func isNoRollout(err error) bool {
	return strings.Contains(protocolMessage(err), "no rollout found")
}

// isArchivedError reports a resume refused because the thread is archived.
func isArchivedError(err error) bool {
	msg := strings.ToLower(protocolMessage(err))
	return strings.Contains(msg, "is archived") || strings.Contains(msg, "codex unarchive")
}

// missingThread wraps err with agent.ErrSessionNotFound when it is
// thread/read's answer for a thread that does not exist. A stored thread
// that is not loaded is still read, so "thread not loaded" means missing.
func missingThread(threadID string, err error) error {
	if msg := protocolMessage(err); strings.HasPrefix(msg, "thread not loaded") || isNoRollout(err) {
		return fmt.Errorf("codex: thread %s: %w: %w", threadID, agent.ErrSessionNotFound, err)
	}
	return err
}

// endCompactionLocked ends the compaction a Compact call waits for. The
// caller holds s.mu.
func (s *Session) endCompactionLocked() {
	s.compacting = false
	if s.compacted != nil {
		close(s.compacted)
		s.compacted = nil
	}
}

// protocolMessage returns the app-server's error message, or "" for other
// errors, so matching never sees text the library wrapped around it.
func protocolMessage(err error) string {
	if perr, ok := errors.AsType[*agent.ProtocolError](err); ok {
		return perr.Message
	}
	return ""
}

// isInvalidRequest reports an app-server "invalid request" error with
// exactly this message.
func isInvalidRequest(err error, message string) bool {
	perr, ok := errors.AsType[*agent.ProtocolError](err)
	return ok && perr.Code == -32600 && perr.Message == message
}

// isNotArchivedError reports an unarchive refused because the thread is
// not archived (any more).
func isNotArchivedError(err error) bool {
	return strings.Contains(strings.ToLower(protocolMessage(err)), "no archived rollout found")
}

// permissionSettings maps a uniform permission mode to an approval policy,
// sandbox mode, and reviewer.
func permissionSettings(mode agent.PermissionMode) (approval any, sandbox, reviewer string) {
	//exhaustive:enforce
	switch mode {
	case "":
		return nil, "", ""
	case agent.PermissionDefault, agent.PermissionAcceptEdits:
		return "on-request", "workspace-write", ""
	case agent.PermissionReadOnly, agent.PermissionPlan:
		// Plan mode promises no changes, so it runs read-only.
		return "on-request", "read-only", ""
	case agent.PermissionDontAsk:
		return "never", "workspace-write", ""
	case agent.PermissionBypass:
		return "never", "danger-full-access", ""
	case agent.PermissionAuto:
		return "on-request", "workspace-write", "auto_review"
	default:
		// Native approval policies pass through.
		return string(mode), "", ""
	}
}

func sandboxPolicy(mode string) map[string]any {
	switch mode {
	case "read-only":
		return map[string]any{"type": "readOnly", "networkAccess": false}
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	}
	return nil
}

// rememberSandbox records the native sandbox policy the thread runs with.
func (s *Session) rememberSandbox(raw json.RawMessage) {
	var sp map[string]any
	if json.Unmarshal(raw, &sp) != nil || sp == nil {
		return
	}
	s.mu.Lock()
	s.setSandboxLocked(sp)
	s.mu.Unlock()
}

func (s *Session) setSandboxLocked(sp map[string]any) {
	s.sandbox = sp
	if sp["type"] == "workspaceWrite" {
		s.workspaceWrite = sp
	}
}

// resolveSandbox returns the native policy for a sandbox mode. A policy of
// the type the thread already runs with is reused, and workspace-write
// keeps the writable roots, network access, and tmp exclusions last seen
// on the thread or configured as sandbox_workspace_write, instead of the
// bare defaults.
func (s *Session) resolveSandbox(ctx context.Context, mode string) map[string]any {
	sp := sandboxPolicy(mode)
	if sp == nil {
		return nil
	}
	s.mu.Lock()
	current, ww, read := s.sandbox, s.workspaceWrite, s.readWorkspace
	s.mu.Unlock()
	if current != nil && current["type"] == sp["type"] {
		return maps.Clone(current)
	}
	if sp["type"] != "workspaceWrite" {
		return sp
	}
	if ww == nil && !read {
		ww = s.configuredWorkspaceWrite(ctx)
		s.mu.Lock()
		s.readWorkspace = true
		if s.workspaceWrite == nil {
			s.workspaceWrite = ww
		}
		s.mu.Unlock()
	}
	if ww != nil {
		return maps.Clone(ww)
	}
	return sp
}

// configuredWorkspaceWrite reads sandbox_workspace_write with config/read
// as a native workspace-write policy, or nil when it is not set.
func (s *Session) configuredWorkspaceWrite(ctx context.Context) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var resp configReadResponse
	params := map[string]any{"includeLayers": false}
	if cwd := s.Info().Cwd; cwd != "" {
		params["cwd"] = cwd
	}
	if err := s.client.Call(ctx, "config/read", params, &resp); err != nil {
		s.rt.Logger().Debug("reading sandbox_workspace_write failed", "error", err)
		return nil
	}
	w := resp.Config.WorkspaceWrite
	if w == nil {
		return nil
	}
	roots := w.WritableRoots
	if roots == nil {
		roots = []string{}
	}
	return map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": w.NetworkAccess,
		"excludeSlashTmp": w.ExcludeSlashTmp, "excludeTmpdirEnvVar": w.ExcludeTmpdirEnvVar}
}

// threadParams builds thread/start, thread/resume, or thread/fork
// parameters from cfg and the session's options.
func (s *Session) threadParams(cfg agent.Config) map[string]any {
	o := s.opts
	p := map[string]any{}
	if cfg.Model != "" {
		p["model"] = cfg.Model
	}
	if cfg.Cwd != "" {
		p["cwd"] = providerutil.AbsDir(cfg.Cwd)
	}
	approval, sandbox, reviewer := permissionSettings(cfg.PermissionMode)
	if o.ApprovalPolicy != nil {
		approval = o.ApprovalPolicy
	}
	if o.Sandbox != "" {
		sandbox = o.Sandbox
	}
	if o.ApprovalsReviewer != "" {
		reviewer = o.ApprovalsReviewer
	}
	if approval != nil {
		p["approvalPolicy"] = approval
	}
	if o.Permissions != "" {
		p["permissions"] = o.Permissions
	} else if sandbox != "" {
		p["sandbox"] = sandbox
	}
	if reviewer == "" && cfg.PermissionMode != "" && cfg.PermissionMode != agent.PermissionAuto {
		// The reviewer is sticky: a resumed thread keeps auto review
		// unless an explicit mode pins the user.
		reviewer = "user"
	}
	if reviewer != "" {
		p["approvalsReviewer"] = reviewer
	}
	if cfg.SystemPrompt != "" {
		p["baseInstructions"] = cfg.SystemPrompt
	}
	if cfg.AppendSystemPrompt != "" {
		p["developerInstructions"] = cfg.AppendSystemPrompt
	}
	if o.ModelProvider != "" {
		p["modelProvider"] = o.ModelProvider
	}
	if o.ServiceTier != "" {
		p["serviceTier"] = o.ServiceTier
	}
	// thread/fork takes no personality.
	if o.Personality != "" && !cfg.Fork {
		p["personality"] = o.Personality
	}
	if len(cfg.AdditionalDirectories) > 0 && cfg.Cwd != "" {
		roots := []string{providerutil.AbsDir(cfg.Cwd)}
		for _, d := range cfg.AdditionalDirectories {
			roots = append(roots, providerutil.AbsDir(d))
		}
		p["runtimeWorkspaceRoots"] = roots
	}
	config := map[string]any{}
	maps.Copy(config, o.Config)
	for name, srv := range cfg.MCPServers {
		config["mcp_servers."+name] = mcpServerConfig(srv)
	}
	s.mu.Lock()
	effort := s.effort
	s.mu.Unlock()
	if effort != "" {
		config["model_reasoning_effort"] = string(effort)
	}
	if o.ReasoningSummary != "" {
		config["model_reasoning_summary"] = o.ReasoningSummary
	}
	if len(config) > 0 {
		p["config"] = config
	}
	if cfg.Resume == "" || cfg.Fork {
		if cfg.Ephemeral {
			p["ephemeral"] = true
		}
	}
	if cfg.Resume != "" {
		// The session never reads the history the response would carry.
		p["excludeTurns"] = true
		if cfg.Fork && cfg.ForkAtTurn != "" {
			p["lastTurnId"] = cfg.ForkAtTurn
		}
	}
	if cfg.Resume == "" {
		if o.HistoryMode != "" {
			p["historyMode"] = o.HistoryMode
		}
		if o.ServiceName != "" {
			p["serviceName"] = o.ServiceName
		}
		if len(cfg.Tools) > 0 && !s.client.opts.DisableExperimentalAPI {
			specs := make([]map[string]any, 0, len(cfg.Tools))
			for _, t := range cfg.Tools {
				specs = append(specs, map[string]any{
					"type":        "function",
					"name":        t.Name,
					"description": t.Description,
					"inputSchema": providerutil.ObjectSchema(t.InputSchema),
				})
			}
			p["dynamicTools"] = specs
		}
	}
	maps.Copy(p, o.ExtraThreadParams)
	return p
}

func mcpServerConfig(srv agent.MCPServer) map[string]any {
	out := map[string]any{}
	switch srv.TransportOrDefault() {
	case agent.MCPHTTP, agent.MCPSSE:
		out["url"] = srv.URL
		if len(srv.Headers) > 0 {
			out["http_headers"] = srv.Headers
		}
	default:
		out["command"] = srv.Command
		if len(srv.Args) > 0 {
			out["args"] = srv.Args
		}
		if len(srv.Env) > 0 {
			out["env"] = srv.Env
		}
		if srv.Cwd != "" {
			out["cwd"] = srv.Cwd
		}
	}
	if srv.Enabled != nil {
		out["enabled"] = *srv.Enabled
	}
	if srv.ToolTimeout > 0 {
		out["tool_timeout_sec"] = srv.ToolTimeout.Seconds()
	}
	if srv.StartupTimeout > 0 {
		out["startup_timeout_sec"] = srv.StartupTimeout.Seconds()
	}
	if len(srv.EnabledTools) > 0 {
		out["enabled_tools"] = srv.EnabledTools
	}
	if len(srv.ApprovedTools) > 0 {
		tools := make(map[string]any, len(srv.ApprovedTools))
		for _, name := range srv.ApprovedTools {
			tools[name] = map[string]any{"approval_mode": "approve"}
		}
		out["tools"] = tools
	}
	return out
}

// ID returns the thread ID.
func (s *Session) ID() string {
	if id := s.threadID.Load(); id != nil {
		return *id
	}
	return ""
}

func (s *Session) setThreadID(id string) { s.threadID.Store(&id) }

// Provider returns "codex".
func (s *Session) Provider() string { return Name }

// Client returns the app-server connection hosting this thread.
func (s *Session) Client() *Client { return s.client }

// Info returns the latest session metadata.
func (s *Session) Info() agent.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Capabilities reports the supported uniform features.
func (s *Session) Capabilities() agent.Capabilities { return s.provider.Capabilities() }

// Events returns the event stream.
func (s *Session) Events() <-chan agent.Event { return s.rt.Events() }

// PendingRequests lists unanswered requests.
func (s *Session) PendingRequests() []agent.Request { return s.rt.PendingRequests() }

// Done is closed after the session ended.
func (s *Session) Done() <-chan struct{} { return s.rt.Done() }

// Err returns why the session ended.
func (s *Session) Err() error { return s.rt.Err() }

// Run sends input and waits for the turn to finish.
func (s *Session) Run(ctx context.Context, in agent.Input) (*agent.TurnResult, error) {
	return providerkit.Run(ctx, s, in)
}

// Send submits input. While a turn runs, DeliverSteer uses turn/steer,
// DeliverInterrupt interrupts the turn and runs the input next, and
// DeliverQueue runs the input after the turn.
func (s *Session) Send(ctx context.Context, in agent.Input) (*agent.Turn, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	if len(in.Parts) == 0 {
		return nil, errors.New("codex: input has no content")
	}
	if err := providerkit.CheckInput(Name, in); err != nil {
		return nil, err
	}
	if _, err := providerkit.TurnOptions[TurnOptions](Name, in.ProviderOptions); err != nil {
		return nil, err
	}
	id := provisionalID()
	turn, ok, err := s.rt.StartIdle(ctx, id, func(ctx context.Context) error { return s.startTurn(ctx, id, in) })
	if ok {
		return turn, err
	}
	running, starting := s.rt.Running()
	switch in.Delivery {
	case agent.DeliverSteer:
		if running == "" || starting {
			return s.enqueue(in)
		}
		return s.steer(ctx, in, running)
	case agent.DeliverInterrupt:
		turn, err := s.enqueue(in)
		if err != nil {
			return nil, err
		}
		if running != "" {
			if err := s.interruptTurn(ctx, running); err != nil {
				return turn, err
			}
		}
		return turn, nil
	default:
		return s.enqueue(in)
	}
}

// provisionalID returns the ID of a turn until Codex reports its own.
func provisionalID() string { return "queued-" + providerutil.NewUUID() }

func (s *Session) enqueue(in agent.Input) (*agent.Turn, error) {
	return s.rt.Enqueue(provisionalID(), in)
}

// steer injects input into the running turn active with turn/steer. When
// the turn ended before the steer arrived, the input runs as the next turn
// instead.
func (s *Session) steer(ctx context.Context, in agent.Input, active string) (*agent.Turn, error) {
	input, err := userInput(in.Parts)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"threadId": s.ID(), "input": input, "expectedTurnId": active}
	if in.ClientID != "" {
		params["clientUserMessageId"] = in.ClientID
	}
	// Take the running turn's handle first: it can complete while the
	// steer request is in flight.
	turn, err := s.rt.NewTurn(active)
	if err != nil {
		return nil, err
	}
	var resp turnSteerResponse
	if err := s.client.Call(ctx, "turn/steer", params, &resp); err != nil {
		if isNoActiveTurn(err) {
			return s.enqueue(in)
		}
		if isSteerRejected(err) {
			return nil, fmt.Errorf("%w: %w", agent.ErrSteerRejected, err)
		}
		return nil, err
	}
	if resp.TurnID != "" && resp.TurnID != active {
		return s.rt.NewTurn(resp.TurnID)
	}
	return turn, nil
}

// answerAsync sends the answer to an asynchronous question: steered into
// the running turn when there is one, otherwise, or when Codex refuses the
// steer, as the next turn.
func (s *Session) answerAsync(ctx context.Context, text string) error {
	in := agent.Text(text)
	if active, starting := s.rt.Running(); active != "" && !starting {
		in.Delivery = agent.DeliverSteer
		_, err := s.Send(ctx, in)
		if !errors.Is(err, agent.ErrSteerRejected) {
			return err
		}
		in.Delivery = agent.DeliverQueue
	}
	_, err := s.Send(ctx, in)
	return err
}

// isNoActiveTurn reports a steer refused because no turn is running.
func isNoActiveTurn(err error) bool { return isInvalidRequest(err, "no active turn to steer") }

var expectedTurnMismatch = regexp.MustCompile("^expected active turn id `[^`]+` but found `[^`]+`$")

// isSteerRejected reports a steer the app-server definitely refused, so
// the input was not delivered. Other errors, such as timeouts, are
// ambiguous.
func isSteerRejected(err error) bool {
	perr, ok := errors.AsType[*agent.ProtocolError](err)
	if !ok {
		return false
	}
	if perr.Code == -32601 {
		return true
	}
	if perr.Code != -32600 {
		return false
	}
	if perr.ErrorCode == "activeTurnNotSteerable" {
		return true
	}
	return perr.Message == "active turn uses a different output schema" || expectedTurnMismatch.MatchString(perr.Message)
}

// isIdleInterrupt reports an interrupt refused because the turn already
// ended.
func isIdleInterrupt(err error) bool { return isInvalidRequest(err, "no active turn to interrupt") }

// startQueued starts queued input as turn id.
func (s *Session) startQueued(ctx context.Context, id string, in agent.Input) error {
	err := s.startTurn(ctx, id, in)
	if err != nil {
		s.rt.Emit(&agent.Notice{Level: agent.NoticeError, Code: "queued_turn_failed", Message: err.Error()})
	}
	return err
}

// startTimeout bounds a turn/start call, which the caller's context does
// not cancel.
const startTimeout = time.Minute

// startTurn calls turn/start for turn id, which holds the running slot.
func (s *Session) startTurn(ctx context.Context, id string, in agent.Input) error {
	var resp turnStartResponse
	params, err := s.turnParams(ctx, in)
	if err == nil {
		_, schema := params["outputSchema"]
		s.tr.beginStart(schema)
		// Once sent, the start is not abandoned because the caller gave
		// up: Codex may already run the turn, and a later start could be
		// mistaken for it.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
		err = s.client.Call(callCtx, "turn/start", params, &resp)
		cancel()
		if err != nil {
			resp.Turn.ID = ""
		}
		s.tr.endStart(resp.Turn.ID)
	}
	if err != nil {
		return err
	}
	sandbox, _ := params["sandboxPolicy"].(map[string]any)
	s.accepted(id, resp.Turn.ID, sandbox)
	return nil
}

// accepted completes the start of turn id, which Codex started as turn
// native with the sandbox policy sandbox, when set.
func (s *Session) accepted(id, native string, sandbox map[string]any) {
	s.mu.Lock()
	// While a start is in flight the translator leaves turn tracking to it,
	// so the turn is registered exactly once.
	stop := s.rt.Accepted(id, native)
	// A turn that failed instantly may have completed before it was
	// registered; its result was held back for this moment.
	early := s.tr.takeEarlyResultLocked(native)
	if sandbox != nil {
		s.setSandboxLocked(sandbox)
	}
	// Results of other turns that ended while no start was in flight to
	// claim them belong to turns this session did not start.
	var others []*agent.TurnResult
	if _, starting := s.rt.Running(); !starting {
		others = s.tr.takeAllEarlyLocked()
	}
	s.mu.Unlock()
	for _, res := range others {
		s.rt.FinishTurn(res.TurnID, res)
	}
	if early != nil {
		s.rt.FinishTurn(native, early)
	} else if stop {
		go s.interruptStarted(native)
	}
}

// interruptStarted interrupts a turn that Interrupt was asked to stop
// while its turn/start was in flight.
func (s *Session) interruptStarted(id string) {
	ctx, cancel := context.WithTimeout(s.rt.Context(), 10*time.Second)
	defer cancel()
	err := s.client.Call(ctx, "turn/interrupt", map[string]any{"threadId": s.ID(), "turnId": id}, nil)
	if err != nil && !isIdleInterrupt(err) {
		s.rt.ClearInterrupt(id)
		s.rt.Emit(&agent.Notice{Level: agent.NoticeError, Code: "interrupt_failed", Message: err.Error()})
	}
}

func (s *Session) turnParams(ctx context.Context, in agent.Input) (map[string]any, error) {
	input, err := userInput(in.Parts)
	if err != nil {
		return nil, err
	}
	p := map[string]any{"threadId": s.ID(), "input": input}
	schema := s.schema
	if len(in.OutputSchema) > 0 {
		if schema, err = s.opts.outputSchema(in.OutputSchema); err != nil {
			return nil, err
		}
	}
	if len(schema) > 0 {
		p["outputSchema"] = schema
	}
	if in.ClientID != "" {
		p["clientUserMessageId"] = in.ClientID
	}
	s.mu.Lock()
	// Codex keeps a turn's model and effort for later turns.
	if in.Model != "" {
		s.model = in.Model
		s.info.Model = in.Model
		p["model"] = in.Model
	}
	if in.Effort != "" {
		s.effort = in.Effort
		s.info.Effort = in.Effort
	}
	if s.effort != "" {
		p["effort"] = string(s.effort)
	}
	plan, model, effort := s.plan, s.model, s.effort
	s.mu.Unlock()
	if s.opts.ReasoningSummary != "" {
		p["summary"] = s.opts.ReasoningSummary
	}
	if plan {
		p["collaborationMode"] = collaborationMode("plan", model, effort)
	}
	maps.Copy(p, s.opts.ExtraTurnParams)
	if t, err := providerkit.TurnOptions[TurnOptions](Name, in.ProviderOptions); err == nil {
		if t.CollaborationMode != nil {
			p["collaborationMode"] = t.CollaborationMode
		}
		if t.ReasoningSummary != "" {
			p["summary"] = t.ReasoningSummary
		}
		if t.ServiceTier != "" {
			p["serviceTierForTurn"] = t.ServiceTier
		}
		if t.StickyServiceTier != "" {
			p["serviceTier"] = t.StickyServiceTier
		}
		if t.ApprovalPolicy != nil {
			p["approvalPolicy"] = t.ApprovalPolicy
		}
		if mode, ok := t.SandboxPolicy.(string); ok {
			if sp := s.resolveSandbox(ctx, mode); sp != nil {
				p["sandboxPolicy"] = sp
			}
		} else if t.SandboxPolicy != nil {
			p["sandboxPolicy"] = t.SandboxPolicy
		}
		if t.ApprovalsReviewer != "" {
			p["approvalsReviewer"] = t.ApprovalsReviewer
		}
		if t.Cwd != "" {
			p["cwd"] = providerutil.AbsDir(t.Cwd)
		}
		if len(t.AdditionalContext) > 0 {
			p["additionalContext"] = t.AdditionalContext
			s.mu.Lock()
			s.context = t.AdditionalContext
			s.mu.Unlock()
		}
		maps.Copy(p, t.Extra)
	}
	return p, nil
}

func collaborationMode(mode, model string, effort agent.Effort) CollaborationMode {
	cm := CollaborationMode{Mode: mode, Settings: CollaborationModeSettings{Model: model}}
	if effort != "" {
		e := string(effort)
		cm.Settings.ReasoningEffort = &e
	}
	return cm
}

// userInput converts parts to Codex UserInput values.
func userInput(parts []agent.Part) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case agent.PartText:
			out = append(out, map[string]any{"type": "text", "text": p.Text, "text_elements": []any{}})
		case agent.PartImage:
			switch {
			case p.URL != "":
				out = append(out, map[string]any{"type": "image", "url": p.URL})
			case p.Path != "" && len(p.Data) == 0:
				out = append(out, map[string]any{"type": "localImage", "path": providerutil.AbsDir(p.Path)})
			default:
				mediaType := p.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				url := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
				out = append(out, map[string]any{"type": "image", "url": url})
			}
		case agent.PartFile:
			// UserInput has no file type, and the model never sees a
			// mention item, so the path goes in as text, the form Codex's
			// own file search inserts.
			if p.Path == "" {
				return nil, providerkit.Unsupported(Name, "file part without Path")
			}
			out = append(out, map[string]any{"type": "text", "text": providerutil.AbsDir(p.Path), "text_elements": []any{}})
		case agent.PartSkill:
			out = append(out, map[string]any{"type": "skill", "name": p.Name, "path": p.Path})
		case agent.PartMention:
			out = append(out, map[string]any{"type": "mention", "name": p.Name, "path": p.Path})
		default:
			return nil, providerkit.Unsupported(Name, "input part "+string(p.Type))
		}
	}
	return out, nil
}

// Interrupt stops the running turn and the running turns of its
// subagents, including subagent turns that start later. Queued input still
// runs afterwards. A turn whose turn/start is still in flight is
// interrupted as soon as Codex reports its ID, and a turn that already
// ended is not an error.
func (s *Session) Interrupt(ctx context.Context) error {
	return s.interruptTurn(ctx, "")
}

// InterruptTurn stops turn id: queued input is dropped, and a running turn
// is interrupted as Interrupt does.
func (s *Session) InterruptTurn(ctx context.Context, id string) error {
	if s.rt.CancelQueued(id) {
		return nil
	}
	return s.interruptTurn(ctx, id)
}

// interruptTurn interrupts the running turn if it is want. An empty want
// means whatever runs, including a turn whose turn/start is in flight, so
// an interrupt meant for one turn never stops the input that replaced it.
func (s *Session) interruptTurn(ctx context.Context, want string) error {
	s.mu.Lock()
	active, starting := s.rt.Interrupt(want)
	children := maps.Clone(s.tr.childTurns)
	s.mu.Unlock()
	if active == "" || starting {
		return nil
	}
	// A refused interrupt is cleared as its reply is read, so a
	// turn/completed that follows it is not taken as interrupted.
	err := s.client.callOrdered(ctx, "turn/interrupt", map[string]any{"threadId": s.ID(), "turnId": active}, func(m *jsonrpc.Message) {
		if m.Error != nil {
			s.rt.ClearInterrupt(active)
		}
	})
	if err != nil {
		s.rt.ClearInterrupt(active)
	}
	if isIdleInterrupt(err) {
		err = nil
	}
	for thread, turn := range children {
		if cerr := s.client.Call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, nil); cerr != nil {
			s.rt.Logger().Debug("interrupting subagent turn failed", "thread", thread, "error", cerr)
		}
	}
	return err
}

// ClearQueue drops input queued behind the running turn and returns it.
func (s *Session) ClearQueue() []agent.Input { return s.rt.ClearQueue() }

// UpdateSettings changes thread settings with thread/settings/update.
// Keys are the native field names, such as "model", "effort",
// "approvalPolicy", "sandboxPolicy", and "collaborationMode".
func (s *Session) UpdateSettings(ctx context.Context, settings map[string]any) error {
	params := map[string]any{"threadId": s.ID()}
	maps.Copy(params, settings)
	return s.threadCall(ctx, "thread/settings/update", params, nil)
}

// SetModel switches the model for later turns.
func (s *Session) SetModel(ctx context.Context, model string) error {
	if err := s.UpdateSettings(ctx, map[string]any{"model": model}); err != nil {
		return err
	}
	s.updateInfo(agent.EventMeta{}, func(i *agent.SessionInfo) {
		s.model = model
		i.Model = model
	})
	return nil
}

// SetEffort changes the reasoning effort for later turns.
func (s *Session) SetEffort(ctx context.Context, effort agent.Effort) error {
	var value any = string(effort)
	if effort == "" {
		value = nil
	}
	if err := s.UpdateSettings(ctx, map[string]any{"effort": value}); err != nil {
		return err
	}
	s.updateInfo(agent.EventMeta{}, func(i *agent.SessionInfo) {
		s.effort = effort
		i.Effort = effort
	})
	return nil
}

// SetPermissionMode changes the approval policy and sandbox for later
// turns. PermissionPlan switches the plan collaboration mode and the
// read-only sandbox on; any other mode switches plan mode off and applies
// its own sandbox.
func (s *Session) SetPermissionMode(ctx context.Context, mode agent.PermissionMode) error {
	approval, sandbox, reviewer := permissionSettings(mode)
	settings := map[string]any{}
	if approval != nil {
		settings["approvalPolicy"] = approval
	}
	sp := s.resolveSandbox(ctx, sandbox)
	if sp != nil {
		settings["sandboxPolicy"] = sp
	}
	if reviewer != "" {
		settings["approvalsReviewer"] = reviewer
	} else if mode != "" {
		settings["approvalsReviewer"] = "user"
	}
	plan := mode == agent.PermissionPlan
	s.mu.Lock()
	model, effort, wasPlan := s.model, s.effort, s.plan
	s.mu.Unlock()
	// The collaboration mode is sent only when plan mode toggles. Its null
	// developer_instructions selects the mode's built-in instructions and
	// keeps the thread's developerInstructions (AppendSystemPrompt);
	// setting it would replace Codex's plan-mode instructions (verified
	// against codex-cli 0.163.0).
	if plan != wasPlan {
		name := "default"
		if plan {
			name = "plan"
		}
		settings["collaborationMode"] = collaborationMode(name, model, effort)
	}
	if err := s.UpdateSettings(ctx, settings); err != nil {
		return err
	}
	s.updateInfo(agent.EventMeta{}, func(i *agent.SessionInfo) {
		if sp != nil {
			s.setSandboxLocked(sp)
		}
		s.plan = plan
		i.PermissionMode = mode
	})
	return nil
}

// Close ends the session. A dedicated app-server is stopped; on a shared
// server the thread is unsubscribed.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.client.unregister(s.ID())
		if s.owned {
			_ = s.client.Close()
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), s.client.shutdownGrace())
			// The shared server keeps running; refuse what it still waits
			// for so the thread does not stay blocked.
			for _, req := range s.rt.PendingRequests() {
				_ = req.Cancel(ctx)
			}
			_ = s.Interrupt(ctx)
			_ = s.client.Call(ctx, "thread/unsubscribe", map[string]any{"threadId": s.ID()}, nil)
			cancel()
			s.client.forget(s.ID())
		}
		s.rt.Finish(nil)
	})
	return nil
}

// serverRequest dispatches an app-server request for this thread.
func (s *Session) serverRequest(msg *jsonrpc.Message, line []byte) (any, error) {
	return s.tr.serverRequest(msg, line)
}

// notify dispatches an app-server notification for this thread.
func (s *Session) notify(msg *jsonrpc.Message, line []byte) {
	s.tr.notify(msg, line)
}

// refusedConfig refuses the Config restrictions Codex cannot honor,
// because a restriction is never ignored. owned is false when the session
// runs on an app-server it does not own, whose environment it cannot
// change.
func refusedConfig(cfg agent.Config, owned bool) error {
	if len(cfg.DisallowedTools) > 0 {
		return providerkit.Unsupported(Name, "DisallowedTools")
	}
	if !owned && len(cfg.UnsetEnv) > 0 {
		return providerkit.Unsupported(Name, "UnsetEnv on a shared app-server")
	}
	return nil
}

// ignoredConfig names the Config fields Codex cannot honor. owned is false
// when the session runs on an app-server it does not own.
func ignoredConfig(cfg agent.Config, owned, experimental bool) []string {
	var ignored []string
	if len(cfg.Tools) > 0 && !experimental {
		ignored = append(ignored, "Tools (dynamic tools need the experimental API)")
	}
	if len(cfg.AllowedTools) > 0 {
		ignored = append(ignored, "AllowedTools")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
		if cfg.MCPServers[name].AlwaysLoad {
			ignored = append(ignored, "MCPServers["+name+"].AlwaysLoad")
		}
	}
	if !owned && len(cfg.Env) > 0 {
		ignored = append(ignored, "Env on a shared app-server")
	}
	return ignored
}

type skillsListResponse struct {
	Data []struct {
		Skills []struct {
			Name             string `json:"name"`
			Description      string `json:"description"`
			ShortDescription string `json:"shortDescription"`
			Enabled          bool   `json:"enabled"`
			Path             string `json:"path"`
			Scope            string `json:"scope"`
			Interface        *struct {
				ShortDescription string `json:"shortDescription"`
			} `json:"interface"`
		} `json:"skills"`
	} `json:"data"`
}

type sandboxPolicyType struct {
	Type string `json:"type"`
}

type configReadResponse struct {
	Config struct {
		WorkspaceWrite *struct {
			WritableRoots       []string `json:"writable_roots"`
			NetworkAccess       bool     `json:"network_access"`
			ExcludeSlashTmp     bool     `json:"exclude_slash_tmp"`
			ExcludeTmpdirEnvVar bool     `json:"exclude_tmpdir_env_var"`
		} `json:"sandbox_workspace_write"`
	} `json:"config"`
}

type turnSteerResponse struct {
	TurnID string `json:"turnId"`
}

type turnStartResponse struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}
