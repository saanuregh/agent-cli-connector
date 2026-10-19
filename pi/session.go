package pi

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/mcpserver"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Session is a live Pi RPC session. It implements agent.Session and adds
// every Pi RPC command.
type Session struct {
	provider *Provider
	cfg      agent.Config
	opts     SessionOptions
	rt       *providerkit.Runtime
	proc     *providerkit.Process
	tr       *translator
	token    string
	tools    map[string]agent.Tool

	nextID atomic.Uint64
	// compacting is set between Pi's compaction_start and compaction_end.
	compacting atomic.Bool
	// bridgeLoaded reports that the session's commands list the bridge's
	// rewind command. The bridge registers it after its tool gate, so it
	// also shows that the gate is in place.
	bridgeLoaded atomic.Bool
	waitMu       sync.Mutex
	waiters      map[string]waiter
	readDone     chan struct{}
	closing      atomic.Bool
	closeOnce    sync.Once

	// steerMu runs one steer at a time; the runtime tracks a single steer.
	steerMu sync.Mutex
	mu      sync.Mutex
	info    agent.SessionInfo
	mode    agent.PermissionMode
	// allowed holds AllowSession grants by grantKey.
	allowed map[string]bool
	denied  map[string]bool
	// turnStamp holds the timestamp of each turn's first user message,
	// which locates the turn's session entry; turnEntry holds the entry
	// itself once the bridge reported it.
	turnStamp map[string]int64
	turnEntry map[string]string
	// turnOrder lists the turns in turnStamp, oldest first, to forget the
	// oldest beyond maxRememberedTurns; their history items still carry
	// entry IDs that Rewind accepts.
	turnOrder []string
	tasks     map[string]*agent.Task
	provName  string
	window    int64
	// modelInput lists the current model's input kinds; nil when unknown.
	modelInput []string
	// builtins holds the queued or running turns of built-in commands,
	// such as /compact, which run without a Pi run to steer.
	builtins map[string]bool
	// clientIDs are prompts sent with an Input.ClientID whose user
	// message Pi has not echoed yet.
	clientIDs []pendingClient
	// rewinds wait for the bridge's answer to a rewind command, by nonce.
	rewinds map[string]chan rewindResult
	// subagentFiles holds each subagent task's session file.
	subagentFiles map[string]string
	// approvedMCP holds the Pi names of MCPServer.ApprovedTools.
	approvedMCP map[string]bool
	// toolCalls cancels the running host tool calls, by bridge request.
	toolCalls map[string]context.CancelFunc
	// outputs holds the turns that asked for structured output; true when
	// the bridge wraps a schema that is not an object.
	outputs map[string]bool
}

type pendingClient struct {
	text     string
	clientID string
}

type rewindResult struct {
	Nonce     string `json:"nonce"`
	OK        bool   `json:"ok"`
	Cancelled bool   `json:"cancelled"`
	Error     string `json:"error"`
}

// rewindCommand is the bridge command that rewinds the session in place.
const rewindCommand = "agent-cli-rewind"

// mcpServerName is the set of names Pi accepts for MCP servers.
var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (s *Session) bridgeDisabled() bool {
	return s.provider.opts.DisableBridge || s.opts.DisableBridge
}

func (s *Session) contextWindow() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window
}

type rpcResponse struct {
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

var (
	_ agent.Session         = (*Session)(nil)
	_ agent.Titler          = (*Session)(nil)
	_ agent.TurnInterrupter = (*Session)(nil)
	_ agent.Compacter       = (*Session)(nil)
	_ agent.Forker          = (*Session)(nil)
	_ agent.Rewinder        = (*Session)(nil)
)

func (p *Provider) startSession(ctx context.Context, cfg agent.Config, opts SessionOptions) (*Session, error) {
	if cfg.Fork && cfg.Resume == "" {
		return nil, errors.New("pi: Fork requires Resume")
	}
	var ignored []string
	if len(cfg.AdditionalDirectories) > 0 {
		ignored = append(ignored, "AdditionalDirectories")
	}
	if len(cfg.OutputSchema) > 0 {
		if _, _, err := outputSchema(cfg.OutputSchema); err != nil {
			return nil, err
		}
		// The schema needs the bridge's structured_output tool.
		if p.opts.DisableBridge || opts.DisableBridge || opts.NoTools {
			ignored = append(ignored, "OutputSchema")
			cfg.OutputSchema = nil
		}
	}
	notice, err := providerkit.IgnoredConfig(Name, cfg, ignored)
	if err != nil {
		return nil, err
	}
	s := &Session{
		provider:  p,
		cfg:       cfg,
		opts:      opts,
		rt:        providerkit.NewRuntime(Name, cfg),
		tools:     make(map[string]agent.Tool, len(cfg.Tools)),
		waiters:   make(map[string]waiter),
		readDone:  make(chan struct{}),
		mode:      cfg.PermissionMode,
		allowed:   make(map[string]bool),
		denied:    make(map[string]bool),
		tasks:     make(map[string]*agent.Task),
		turnStamp: make(map[string]int64),
		builtins:  make(map[string]bool),
		turnEntry: make(map[string]string),
		rewinds:   make(map[string]chan rewindResult),

		subagentFiles: make(map[string]string),
		outputs:       make(map[string]bool),
		toolCalls:     make(map[string]context.CancelFunc),
		approvedMCP:   make(map[string]bool),
	}
	if notice != nil {
		s.rt.Emit(notice)
	}
	for _, t := range cfg.Tools {
		if t.Name == outputTool {
			return nil, fmt.Errorf("pi: tool name %q is reserved for structured output", outputTool)
		}
		s.tools[t.Name] = t
	}
	namespaces := map[string]string{}
	for name, srv := range cfg.MCPServers {
		if srv.TransportOrDefault() == agent.MCPSSE {
			return nil, providerkit.Unsupported(Name, "SSE MCP server "+name+" (Pi supports stdio and streamable HTTP)")
		}
		if !mcpServerName.MatchString(name) {
			return nil, fmt.Errorf("pi: invalid MCP server name %q (use letters, digits, \"_\", and \"-\")", name)
		}
		ns := strings.ReplaceAll(name, "-", "_")
		if other, ok := namespaces[ns]; ok {
			return nil, fmt.Errorf("pi: MCP server names %q and %q collide (Pi treats \"-\" and \"_\" alike)", other, name)
		}
		namespaces[ns] = name
		for _, tool := range srv.ApprovedTools {
			s.approvedMCP[mcpToolName(name, tool)] = true
		}
	}
	if (len(cfg.Tools) > 0 || len(cfg.MCPServers) > 0) && s.bridgeDisabled() {
		return nil, errors.New("pi: Tools and MCPServers need the bridge extension")
	}
	// Pi has no permission system: without the bridge every tool runs.
	if s.bridgeDisabled() {
		if !gatesNothing(cfg.PermissionMode) {
			return nil, providerkit.Unsupported(Name, "permission mode "+string(cfg.PermissionMode)+" without the bridge extension")
		}
	} else if err := providerkit.CheckPermissionMode(Name, cfg.PermissionMode, s.Capabilities().PermissionModes); err != nil {
		return nil, err
	}
	s.tr = newTranslator(s)
	s.rt.SetStarter(s.start)

	env := map[string]string{}
	maps.Copy(env, p.opts.Env)
	maps.Copy(env, cfg.Env)
	args, err := s.buildArgs(env)
	if err != nil {
		return nil, err
	}
	proc, err := providerkit.StartProcess(ctx, providerkit.ProcessOptions{
		Provider: Name,
		Spawner:  p.opts.Spawner,
		Command:  agent.Command{Path: p.path(), Args: args, Env: providerkit.EnvList(env), UnsetEnv: cfg.UnsetEnv, Dir: cfg.Cwd},
		Logger:   s.rt.Logger(),
		WireTap:  cfg.WireTap,
	})
	if err != nil {
		return nil, err
	}
	s.proc = proc
	go s.readLoop()

	startCtx, cancel := context.WithTimeout(ctx, p.startupTimeout())
	defer cancel()
	state, err := s.State(startCtx)
	if err != nil {
		s.shutdown()
		if exitErr := s.exitError(); exitErr != nil {
			return nil, fmt.Errorf("pi: start: %w", exitErr)
		}
		return nil, fmt.Errorf("pi: start: %w", err)
	}
	s.applyState(state)
	// A resumed session file runs in the directory its header records.
	s.followSessionCwd(state.SessionFile)
	if err := s.applyStartupSettings(startCtx); err != nil {
		s.shutdown()
		return nil, err
	}
	if cfg.Resume != "" && cfg.Model != "" {
		s.applyResumeModel(startCtx)
	}
	s.loadCommands(startCtx)
	if err := s.checkGate(cfg.PermissionMode); err != nil {
		s.shutdown()
		return nil, err
	}
	if cfg.Fork && cfg.ForkAtTurn != "" {
		if err := s.truncateAfter(startCtx, cfg.ForkAtTurn); err != nil {
			s.shutdown()
			return nil, err
		}
	}
	info := s.Info()
	s.rt.Started(&agent.SessionStarted{EventMeta: agent.EventMeta{Raw: state.Raw}, Info: info, Resumed: cfg.Resume != ""})
	return s, nil
}

func (s *Session) buildArgs(env map[string]string) ([]string, error) {
	cfg, o := s.cfg, s.opts
	args := []string{"--mode", "rpc"}
	// A resumed session keeps its model unless the requested one is
	// available; applyResumeModel sets it after start.
	if cfg.Model != "" && cfg.Resume == "" {
		args = append(args, "--model", cfg.Model)
	}
	if o.Provider != "" {
		args = append(args, "--provider", o.Provider)
	}
	if cfg.Effort != "" {
		args = append(args, "--thinking", thinkingLevel(cfg.Effort))
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", cfg.AppendSystemPrompt)
	}
	if cfg.Ephemeral {
		args = append(args, "--no-session")
	}
	// Pi asks on stdin before opening a session ID recorded under another
	// directory, which breaks RPC mode; a session file path opens directly.
	resume := cfg.Resume
	if resume != "" {
		if path, err := s.sessionStore(env).findSession(resume, cfg.Cwd); err == nil {
			resume = path
		}
	}
	switch {
	case resume != "" && cfg.Fork:
		args = append(args, "--fork", resume)
	case resume != "":
		args = append(args, "--session", resume)
	}
	if cfg.SessionID != "" {
		args = append(args, "--session-id", cfg.SessionID)
	}
	if cfg.Title != "" {
		args = append(args, "--name", cfg.Title)
	}
	if len(o.Models) > 0 {
		args = append(args, "--models", strings.Join(o.Models, ","))
	}
	if !s.bridgeDisabled() {
		path := s.provider.opts.BridgePath
		if path == "" {
			p, err := localBridgePath()
			if err != nil {
				return nil, err
			}
			path = p
		}
		s.token = "agent-cli-bridge-" + providerutil.NewUUID()
		bridge, err := json.Marshal(s.bridgeConfig())
		if err != nil {
			return nil, fmt.Errorf("pi: encode bridge config: %w", err)
		}
		env["AGENT_CLI_BRIDGE"] = string(bridge)
		args = append(args, "--extension", path)
	}
	for _, e := range o.Extensions {
		args = append(args, "--extension", e)
	}
	if o.NoExtensions {
		args = append(args, "--no-extensions")
	}
	for _, sk := range o.Skills {
		args = append(args, "--skill", sk)
	}
	if o.NoSkills {
		args = append(args, "--no-skills")
	}
	for _, pt := range o.PromptTemplates {
		args = append(args, "--prompt-template", pt)
	}
	if o.NoPromptTemplates {
		args = append(args, "--no-prompt-templates")
	}
	if o.NoContextFiles {
		args = append(args, "--no-context-files")
	}
	if len(o.Tools) > 0 {
		args = append(args, "--tools", strings.Join(s.toolAllowlist(), ","))
	}
	if o.NoTools {
		if len(cfg.Tools) > 0 || len(cfg.MCPServers) > 0 {
			return nil, errors.New("pi: SessionOptions.NoTools removes Config.Tools and MCP server tools; list the tools in SessionOptions.Tools instead")
		}
		args = append(args, "--no-tools")
	}
	if o.NoBuiltinTools {
		args = append(args, "--no-builtin-tools")
	}
	if len(cfg.DisallowedTools) > 0 {
		args = append(args, "--exclude-tools", strings.Join(cfg.DisallowedTools, ","))
	}
	if o.NoMCP {
		args = append(args, "--no-mcp")
	}
	if dir := cmp.Or(o.SessionDir, s.provider.opts.SessionDir); dir != "" {
		args = append(args, "--session-dir", dir)
	}
	if o.TrustProject != nil {
		if *o.TrustProject {
			args = append(args, "--approve")
		} else {
			args = append(args, "--no-approve")
		}
	}
	if o.Offline {
		args = append(args, "--offline")
	}
	args = append(args, s.provider.opts.ExtraArgs...)
	args = append(args, o.ExtraArgs...)
	return args, nil
}

func (s *Session) bridgeConfig() map[string]any {
	tools := make([]map[string]any, 0, len(s.cfg.Tools))
	for _, t := range s.cfg.Tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  providerutil.ObjectSchema(t.InputSchema),
			"readOnly":    t.ReadOnly,
		})
	}
	servers := map[string]any{}
	for name, srv := range s.cfg.MCPServers {
		entry := map[string]any{}
		switch srv.TransportOrDefault() {
		case agent.MCPHTTP:
			entry["url"] = srv.URL
			if len(srv.Headers) > 0 {
				entry["headers"] = srv.Headers
			}
		default:
			entry["command"] = srv.Command
			if len(srv.Args) > 0 {
				entry["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				entry["env"] = srv.Env
			}
			if srv.Cwd != "" {
				entry["cwd"] = srv.Cwd
			}
		}
		if srv.Enabled != nil {
			entry["enabled"] = *srv.Enabled
		}
		exposure := "codemode" // Pi's default
		if srv.AlwaysLoad {
			exposure = "direct"
			entry["exposure"] = exposure
		}
		if len(srv.EnabledTools) > 0 {
			only := make(map[string]string, len(srv.EnabledTools))
			for _, tool := range srv.EnabledTools {
				only[tool] = exposure
			}
			entry["exposure"] = "hidden"
			entry["toolExposure"] = only
		}
		if srv.ToolTimeout > 0 {
			entry["timeout"] = int64(math.Ceil(srv.ToolTimeout.Seconds()))
		}
		servers[name] = entry
	}
	return map[string]any{"token": s.token, "gate": true, "tools": tools, "mcpServers": servers}
}

// thinkingLevel maps uniform effort to a Pi thinking level.
func thinkingLevel(e agent.Effort) string {
	if e == agent.EffortNone {
		return "off"
	}
	return string(e)
}

func (s *Session) applyStartupSettings(ctx context.Context) error {
	o := s.opts
	if o.SteeringMode != "" {
		if err := s.SetSteeringMode(ctx, o.SteeringMode); err != nil {
			return err
		}
	}
	if o.FollowUpMode != "" {
		if err := s.SetFollowUpMode(ctx, o.FollowUpMode); err != nil {
			return err
		}
	}
	if o.AutoCompaction != nil {
		if err := s.SetAutoCompaction(ctx, *o.AutoCompaction); err != nil {
			return err
		}
	}
	if o.AutoRetry != nil {
		return s.SetAutoRetry(ctx, *o.AutoRetry)
	}
	return nil
}

// ID returns the Pi session ID.
func (s *Session) ID() string { return s.rt.SessionID() }

// Provider returns "pi".
func (s *Session) Provider() string { return Name }

// Info returns the latest session metadata.
func (s *Session) Info() agent.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.info
	info.ID = s.rt.SessionID()
	return info
}

// Capabilities reports the supported uniform features. Without the bridge
// extension, permission prompts and modes, host tools, MCP servers, and
// output schemas are unsupported; NoTools rules out host tools, MCP
// servers, and output schemas.
func (s *Session) Capabilities() agent.Capabilities {
	caps := s.provider.Capabilities()
	if s.bridgeDisabled() {
		caps.PermissionModes = nil
		caps.PermissionPrompt = false
		caps.HostTools = false
		caps.MCPServers = false
		caps.OutputSchema = false
	}
	if s.opts.NoTools {
		caps.HostTools = false
		caps.MCPServers = false
		caps.OutputSchema = false
	}
	return caps
}

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

// Send submits input. While Pi works, DeliverSteer uses Pi's steering
// queue (delivered before the next model call), DeliverInterrupt aborts and
// then runs the input, and DeliverQueue runs the input as the next turn.
// Pi's own follow-up queue is available through FollowUp.
func (s *Session) Send(ctx context.Context, in agent.Input) (*agent.Turn, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	if err := providerkit.CheckInput(Name, in); err != nil {
		return nil, err
	}
	if in.ProviderOptions != nil {
		return nil, fmt.Errorf("pi: Input.ProviderOptions holds %T; pi has no turn options", in.ProviderOptions)
	}
	_, _, builtin := s.builtinCommand(in)
	if len(in.OutputSchema) > 0 {
		if s.token == "" || s.opts.NoTools {
			return nil, providerkit.Unsupported(Name, "output schema without the bridge extension or with NoTools")
		}
		if builtin || s.extensionCommand(in) {
			return nil, providerkit.Unsupported(Name, "output schema for a command")
		}
		if _, _, err := outputSchema(in.OutputSchema); err != nil {
			return nil, err
		}
	}
	// Turn IDs are always new: a reused ClientID, such as a retry, must
	// not resolve to the earlier turn.
	id := providerutil.NewUUID()
	if builtin {
		s.mu.Lock()
		s.builtins[id] = true
		s.mu.Unlock()
		turn, started, err := s.rt.StartIdle(ctx, id, func(ctx context.Context) error { return s.start(ctx, id, in) })
		if started {
			return turn, err
		}
		current, _ := s.rt.Running()
		if turn, err = s.rt.Enqueue(id, in); err != nil {
			s.forgetBuiltin(id)
			return nil, err
		}
		if in.Delivery == agent.DeliverInterrupt && current != "" {
			err = s.interruptTurn(ctx, current)
		}
		return turn, err
	}
	// Rendered now to refuse bad input at once; start renders it again,
	// and checks images against the input's model once it switched.
	render := s.promptPayload
	if in.Model != "" {
		render = func(in agent.Input) (string, []map[string]any, error) { return promptPayload(in) }
	}
	msg, images, err := render(in)
	if err != nil {
		return nil, err
	}
	turn, started, err := s.rt.StartIdle(ctx, id, func(ctx context.Context) error { return s.start(ctx, id, in) })
	if started {
		return turn, err
	}
	current, _ := s.rt.Running()
	switch {
	case current == "", s.isBuiltin(current) && in.Delivery != agent.DeliverInterrupt:
		// Queued input is still waiting to start, or a built-in command
		// runs without a Pi run to steer; keep the order.
		return s.rt.Enqueue(id, in)
	case in.Delivery == agent.DeliverSteer && len(in.OutputSchema) == 0:
		// Input with an output schema needs a run of its own.
		return s.steer(ctx, in, current, msg, images)
	case in.Delivery == agent.DeliverInterrupt:
		turn, err := s.rt.Enqueue(id, in)
		if err != nil {
			return nil, err
		}
		return turn, s.interruptTurn(ctx, current)
	default:
		return s.rt.Enqueue(id, in)
	}
}

// forgetTurns forgets the session entries of earlier turns.
func (s *Session) forgetTurns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.turnStamp)
	clear(s.turnEntry)
	s.turnOrder = nil
}

// forgetBuiltin drops turn id from the built-in command turns.
func (s *Session) forgetBuiltin(id string) {
	s.mu.Lock()
	delete(s.builtins, id)
	s.mu.Unlock()
}

// isBuiltin reports whether turn id runs a built-in command.
func (s *Session) isBuiltin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builtins[id]
}

// start starts input as turn id, which holds the running slot: a built-in
// command runs in the background, other input is sent as a prompt.
func (s *Session) start(ctx context.Context, id string, in agent.Input) error {
	if err := s.applyInputSettings(ctx, in); err != nil {
		s.forgetBuiltin(id)
		return err
	}
	name, args, builtin := s.builtinCommand(in)
	s.mu.Lock()
	if builtin {
		s.builtins[id] = true
	} else {
		delete(s.builtins, id)
	}
	s.mu.Unlock()
	if builtin {
		go s.runBuiltin(id, name, args)
		return nil
	}
	if len(in.OutputSchema) == 0 && !s.extensionCommand(in) {
		in.OutputSchema = s.cfg.OutputSchema
	}
	msg, images, err := s.promptFor(id, in)
	if err != nil {
		return err
	}
	return s.prompt(ctx, id, in.ClientID, msg, images)
}

// toolAllowlist returns SessionOptions.Tools plus the bridge's tools,
// which Pi would otherwise drop. Pi refuses lists that mix plain names
// with "+name"/"-name" changes, so added names follow the list's form.
func (s *Session) toolAllowlist() []string {
	tools := slices.Clone(s.opts.Tools)
	changes := slices.ContainsFunc(tools, func(t string) bool { return strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") })
	add := func(name string) {
		if changes {
			name = "+" + name
		}
		if !slices.Contains(tools, name) {
			tools = append(tools, name)
		}
	}
	for _, t := range s.cfg.Tools {
		add(t.Name)
	}
	if !s.bridgeDisabled() {
		add(outputTool)
	}
	return tools
}

// sessionStore returns a provider that finds sessions where this
// session's Pi process stores them; env is the process environment.
func (s *Session) sessionStore(env map[string]string) *Provider {
	opts := s.provider.opts
	opts.SessionDir = cmp.Or(s.opts.SessionDir, opts.SessionDir)
	opts.Env = env
	return &Provider{opts: opts}
}

// applyInputSettings switches to the input's model and effort, when set,
// as the input starts.
//
// Once a command is written Pi applies it whatever happens to ctx, so the
// replies are awaited regardless, keeping Info in step with Pi.
func (s *Session) applyInputSettings(ctx context.Context, in agent.Input) error {
	if in.Model == "" && in.Effort == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	if in.Model != "" {
		if err := s.SetModel(ctx, in.Model); err != nil {
			return err
		}
	}
	if in.Effort != "" {
		return s.SetEffort(ctx, in.Effort)
	}
	return nil
}

// promptFor renders input for the prompt of turn id. With an output
// schema, it appends the line the bridge turns into the structured_output
// tool and remembers that the turn wants structured output.
func (s *Session) promptFor(id string, in agent.Input) (string, []map[string]any, error) {
	msg, images, err := s.promptPayload(in)
	if err != nil || len(in.OutputSchema) == 0 {
		return msg, images, err
	}
	schema, wrapped, err := outputSchema(in.OutputSchema)
	if err != nil {
		return "", nil, err
	}
	payload, err := json.Marshal(map[string]json.RawMessage{"schema": schema})
	if err != nil {
		return "", nil, fmt.Errorf("pi: encode output schema: %w", err)
	}
	s.mu.Lock()
	s.outputs[id] = wrapped
	s.mu.Unlock()
	return msg + "\n" + s.token + ":output:" + base64.RawURLEncoding.EncodeToString(payload), images, nil
}

// outputSchema returns the parameters of the structured_output tool for an
// output schema. Tool parameters must be an object, so another schema is
// wrapped in the "value" property, and wrapped is true.
func outputSchema(schema json.RawMessage) (params json.RawMessage, wrapped bool, err error) {
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, false, fmt.Errorf("pi: output schema must be a JSON object: %w", err)
	}
	if s["type"] == "object" {
		return schema, false, nil
	}
	params, err = json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]json.RawMessage{"value": schema},
		"required":             []string{"value"},
		"additionalProperties": false,
	})
	return params, true, err
}

// takeOutput forgets turn id's output request and reports whether it had
// one and whether its schema was wrapped.
func (s *Session) takeOutput(id string) (wanted, wrapped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wrapped, wanted = s.outputs[id]
	delete(s.outputs, id)
	return wanted, wrapped
}

// prompt sends the prompt of turn id. A prompt an extension command
// consumed ends the turn at once, as no run starts.
func (s *Session) prompt(ctx context.Context, id, clientID, msg string, images []map[string]any) error {
	disposition, err := s.sendPrompt(ctx, clientID, msg, images)
	if err == nil && (disposition == "started" || disposition == "queued") && s.rt.Accepted(id, "") {
		// Interrupted while the prompt started.
		if _, err := s.Command(s.rt.Context(), "abort", nil); err != nil {
			s.rt.ClearInterrupt(id)
		}
	}
	if err != nil || disposition == "handled" {
		// No run starts, so no run result takes the output request.
		s.takeOutput(id)
	}
	if err == nil && disposition == "handled" {
		s.rt.FinishTurn(id, &agent.TurnResult{Status: agent.TurnSucceeded, StopReason: "handled"})
	}
	return err
}

// steer injects input into the running turn. Pi may have gone idle before
// it reads the command, in which case it starts a new run that becomes a
// turn of its own.
func (s *Session) steer(ctx context.Context, in agent.Input, current, msg string, images []map[string]any) (*agent.Turn, error) {
	id := providerutil.NewUUID()
	s.steerMu.Lock()
	defer s.steerMu.Unlock()
	// Queued input waits while the steer may still start a run. Input for
	// a turn that is still starting, or that ended, is queued instead:
	// Pi, still idle, would start a run for it ahead of that turn.
	if !s.rt.BeginSteer(id, current) {
		return s.rt.Enqueue(id, in)
	}
	// A steer's model and effort apply to the run it joins; images are
	// checked against that model, so a refused steer leaves the run on
	// the new model.
	if err := s.applyInputSettings(ctx, in); err != nil {
		s.rt.EndSteer(id)
		return nil, err
	}
	if in.Model != "" {
		if _, _, err := s.promptPayload(in); err != nil {
			s.rt.EndSteer(id)
			return nil, err
		}
	}
	// Both handles are taken before Pi can start or finish a run, so
	// neither turn can complete before it is registered.
	turn, err := s.rt.NewTurn(id)
	if err != nil {
		s.rt.EndSteer(id)
		return nil, err
	}
	joined, err := s.rt.NewTurn(current)
	if err != nil {
		s.rt.EndSteer(id)
		s.rt.AbandonTurn(id, err)
		return nil, err
	}
	disposition, err := s.sendPrompt(ctx, in.ClientID, msg, images)
	if err != nil && strings.Contains(err.Error(), "compaction is in progress") {
		// Pi refuses prompts during a manual compaction started outside
		// a built-in turn, such as a raw compact Command or one an
		// extension runs; its steering queue still takes the message.
		payload := map[string]any{"message": msg}
		if len(images) > 0 {
			payload["images"] = images
		}
		if _, serr := s.Command(context.WithoutCancel(ctx), "steer", payload); serr == nil {
			err, disposition = nil, "queued"
			s.expectClientID(in.ClientID, msg)
		}
	}
	if running, _ := s.rt.Running(); running == id {
		// Pi started a run for the steer and it took the slot; the run is
		// live whatever the reply said, so the steer's turn stays.
		return turn, nil
	}
	if err != nil || disposition != "started" {
		s.rt.EndSteer(id)
	}
	switch {
	case err != nil:
		s.rt.AbandonTurn(id, err)
		return nil, err
	case disposition == "started":
		return turn, nil
	case disposition == "handled":
		s.rt.FinishTurn(id, &agent.TurnResult{Status: agent.TurnSucceeded, StopReason: "handled"})
		return turn, nil
	}
	s.rt.AbandonTurn(id, nil)
	return joined, nil
}

// sendPrompt sends a prompt that steers a run Pi already started, so it
// never fails because Pi is busy, and returns Pi's disposition: started,
// queued, or handled. clientID is matched to the user message Pi echoes.
func (s *Session) sendPrompt(ctx context.Context, clientID, msg string, images []map[string]any) (string, error) {
	payload := map[string]any{"message": msg, "streamingBehavior": "steer"}
	if len(images) > 0 {
		payload["images"] = images
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.expectClientID(clientID, msg)
	// Once written, Pi runs the prompt whatever happens to ctx, so the
	// reply is awaited regardless: giving up would leave Pi running input
	// the session no longer tracks. The reply can wait for a compaction
	// Pi runs before the prompt; the wait ends if Pi exits.
	raw, err := s.Command(context.WithoutCancel(ctx), "prompt", payload)
	if err != nil {
		s.mu.Lock()
		s.clientIDs = slices.DeleteFunc(s.clientIDs, func(p pendingClient) bool { return p.clientID == clientID })
		s.mu.Unlock()
		return "", err
	}
	var data struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(raw, &data)
	return data.Disposition, nil
}

// expectClientID remembers clientID for the user message Pi echoes for
// prompt msg, which carries no structured output line: the bridge strips
// it before Pi records the message.
func (s *Session) expectClientID(clientID, msg string) {
	if clientID == "" {
		return
	}
	if s.token != "" {
		if i := strings.LastIndex(msg, "\n"+s.token+":output:"); i >= 0 {
			msg = msg[:i]
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Prompts Pi rewrites, such as skills, are never matched; keep the
	// list short.
	if len(s.clientIDs) >= 16 {
		s.clientIDs = s.clientIDs[1:]
	}
	s.clientIDs = append(s.clientIDs, pendingClient{text: msg, clientID: clientID})
}

// takeClientID returns the ClientID of the pending prompt whose text is
// text, and forgets it.
func (s *Session) takeClientID(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Pi appends hints, such as image resizing notes, on lines after the
	// prompt.
	i := slices.IndexFunc(s.clientIDs, func(p pendingClient) bool { return text == p.text || strings.HasPrefix(text, p.text+"\n") })
	if i < 0 {
		return ""
	}
	id := s.clientIDs[i].clientID
	s.clientIDs = slices.Delete(s.clientIDs, i, i+1)
	return id
}

// promptPayload renders input for the prompt command. Images are refused
// when the current model does not accept them.
func (s *Session) promptPayload(in agent.Input) (string, []map[string]any, error) {
	msg, images, err := promptPayload(in)
	if err != nil || len(images) == 0 {
		return msg, images, err
	}
	s.mu.Lock()
	input, model := s.modelInput, s.info.Model
	s.mu.Unlock()
	if input != nil && !slices.Contains(input, "image") {
		return "", nil, providerkit.Unsupported(Name, "image input for model "+model)
	}
	return msg, images, nil
}

func promptPayload(in agent.Input) (string, []map[string]any, error) {
	var images []map[string]any
	for _, p := range in.Parts {
		if p.Type != agent.PartImage {
			continue
		}
		if p.URL != "" {
			return "", nil, providerkit.Unsupported(Name, "image URLs")
		}
		data, mediaType, err := providerkit.ReadPart(p.Data, p.MediaType, p.Path)
		if err != nil {
			return "", nil, fmt.Errorf("pi: image %s: %w", p.Path, err)
		}
		images = append(images, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(data), "mimeType": mediaType})
	}
	msg := skillPrompt(in)
	if msg == "" && len(images) == 0 {
		return "", nil, errors.New("pi: input has no content")
	}
	return msg, images, nil
}

// skillPrompt renders the input as text. Pi expands a skill only when the
// prompt starts with "/skill:name", so the first skill part leads and the
// rest of the input becomes its arguments.
func skillPrompt(in agent.Input) string {
	i := slices.IndexFunc(in.Parts, func(p agent.Part) bool { return p.Type == agent.PartSkill })
	if i < 0 {
		return in.PlainText()
	}
	name := in.Parts[i].Name
	if !strings.HasPrefix(name, "skill:") {
		name = "skill:" + name
	}
	rest := in
	rest.Parts = slices.Delete(slices.Clone(in.Parts), i, i+1)
	if args := rest.PlainText(); args != "" {
		return "/" + name + " " + args
	}
	return "/" + name
}

// Interrupt aborts the running turn. It first clears Pi's queued steering
// and follow-up messages, which Pi would otherwise run after the abort;
// input queued by this library still runs afterwards.
func (s *Session) Interrupt(ctx context.Context) error {
	return s.interruptTurn(ctx, "")
}

// InterruptTurn stops turn id: queued input is dropped, and a running turn
// is aborted as Interrupt does.
func (s *Session) InterruptTurn(ctx context.Context, id string) error {
	if s.rt.CancelQueued(id) {
		s.forgetBuiltin(id)
		return nil
	}
	return s.interruptTurn(ctx, id)
}

// interruptTurn aborts the running turn if it is want, or any running turn
// when want is empty, so an interrupt meant for one turn never stops the
// input that replaced it.
func (s *Session) interruptTurn(ctx context.Context, want string) error {
	// The record marks the turn interrupted when it settles.
	current, starting := s.rt.Interrupt(want)
	if current == "" {
		return nil
	}
	if starting && !s.isBuiltin(current) {
		// Before the "started" reply Pi may compact, and an abort stops
		// that compaction; otherwise Pi ignores it. Either way the
		// record stays pending, and prompt sends the abort again once
		// Pi reports the run started, since Pi starts the model call
		// without checking for an earlier abort.
		if s.compacting.Load() {
			_, err := s.Command(ctx, "abort", nil)
			return err
		}
		return nil
	}
	// Pi's abort waits for the run to stop, and a tool call waiting on a
	// bridge answer cannot stop until it gets one.
	s.mu.Lock()
	for _, cancel := range s.toolCalls {
		cancel()
	}
	s.mu.Unlock()
	for _, req := range s.rt.PendingRequests() {
		if p, ok := req.(*agent.PermissionRequest); ok {
			_ = p.Deny(ctx, "The turn was interrupted.")
		} else {
			_ = req.Cancel(ctx)
		}
	}
	_, clearErr := s.Command(ctx, "clear_queue", nil)
	if clearErr == nil {
		// Steers Pi had queued are gone; their echoes will never arrive.
		s.mu.Lock()
		s.clientIDs = nil
		s.mu.Unlock()
	}
	var pe *agent.ProtocolError
	if errors.As(clearErr, &pe) && strings.Contains(pe.Message, "Unknown command") {
		clearErr = nil // older Pi
	}
	if id, _ := s.rt.Interrupting(); id != current {
		// The turn ended while the queue was cleared; an abort now would
		// stop the input that replaced it.
		return clearErr
	}
	// A refused abort is cleared as its response is read, so a turn end
	// that follows it is not taken as aborted.
	_, err := s.callOrdered(ctx, s.newRequestID(), "abort", nil, func(resp rpcResponse) {
		if !resp.Success {
			s.rt.ClearInterrupt(current)
		}
	})
	if err != nil {
		s.rt.ClearInterrupt(current)
		return err
	}
	return clearErr
}

// splitModel splits "provider/id" or "provider:id", with an optional
// ":thinking" level suffix. provider is empty for a bare ID.
func splitModel(model string) (provider, id, level string) {
	if i := strings.LastIndex(model, ":"); i >= 0 && slices.Contains(thinkingLevels, model[i+1:]) {
		model, level = model[:i], model[i+1:]
	}
	slash, colon := strings.Index(model, "/"), strings.Index(model, ":")
	switch {
	case slash >= 0 && (colon < 0 || slash < colon):
		return model[:slash], model[slash+1:], level
	case colon >= 0:
		return model[:colon], model[colon+1:], level
	}
	return "", model, level
}

// applyResumeModel switches a resumed session to Config.Model when Pi lists
// it as available; otherwise the session keeps its recorded model.
func (s *Session) applyResumeModel(ctx context.Context) {
	provider, id, level := splitModel(s.cfg.Model)
	provider = cmp.Or(provider, s.opts.Provider)
	raws, err := s.AvailableModels(ctx)
	if err != nil {
		s.rt.Logger().Warn("pi: cannot list models to apply the requested model", "model", s.cfg.Model, "error", err)
		return
	}
	var found *modelInfo
	for _, raw := range raws {
		var m modelInfo
		if json.Unmarshal(raw, &m) == nil && m.ID == id && (provider == "" || m.Provider == provider) {
			found = &m
			break
		}
	}
	if found == nil {
		s.rt.Logger().Warn("pi: requested model is not available; keeping the session's model", "model", s.cfg.Model)
		s.rt.Emit(&agent.Notice{Level: agent.NoticeWarning, Code: "model_unavailable",
			Message: "model " + s.cfg.Model + " is not available; the session keeps " + s.Info().Model})
		return
	}
	if want := found.Provider + "/" + found.ID; want != s.Info().Model {
		if err := s.SetModel(ctx, want); err != nil {
			s.rt.Logger().Warn("pi: cannot apply the requested model", "model", want, "error", err)
			return
		}
	}
	if level != "" && s.cfg.Effort == "" {
		if err := s.SetEffort(ctx, effortOf(level)); err != nil {
			s.rt.Logger().Warn("pi: cannot apply the requested thinking level", "level", level, "error", err)
		}
	}
}

// SetModel switches the model. model is "provider/id" or "provider:id",
// optionally with a ":thinking" level suffix, or an ID of the current
// provider. Info reflects the thinking level Pi clamped for the model.
func (s *Session) SetModel(ctx context.Context, model string) error {
	provider, id, level := splitModel(model)
	if provider == "" {
		s.mu.Lock()
		provider = s.provName
		s.mu.Unlock()
	}
	raw, err := s.Command(ctx, "set_model", map[string]any{"provider": provider, "modelId": id})
	if err != nil {
		return err
	}
	if level != "" {
		if _, err := s.Command(ctx, "set_thinking_level", map[string]any{"level": level}); err != nil {
			return err
		}
	}
	if s.refresh(ctx) == nil {
		return nil
	}
	var m modelInfo
	_ = json.Unmarshal(raw, &m)
	s.updateInfo(agent.EventMeta{}, func(info *agent.SessionInfo) {
		s.provName = cmp.Or(m.Provider, provider)
		info.Model = s.provName + "/" + cmp.Or(m.ID, id)
		if m.ContextWindow > 0 {
			s.window = m.ContextWindow
		}
		s.modelInput = m.Input
	})
	return nil
}

// SetEffort sets the thinking level. Info reflects the level Pi clamped
// it to for the current model.
func (s *Session) SetEffort(ctx context.Context, effort agent.Effort) error {
	if effort == "" {
		effort = agent.EffortNone
	}
	if _, err := s.Command(ctx, "set_thinking_level", map[string]any{"level": thinkingLevel(effort)}); err != nil {
		return err
	}
	if s.refresh(ctx) == nil {
		return nil
	}
	s.updateInfo(agent.EventMeta{}, func(info *agent.SessionInfo) { info.Effort = effort })
	return nil
}

// gatesNothing reports whether mode runs every tool, which Pi does
// without the bridge.
func gatesNothing(mode agent.PermissionMode) bool {
	return mode == "" || mode == agent.PermissionBypass
}

// checkGate refuses a mode that gates tool calls unless the bridge, whose
// gate enforces it, is loaded. It relies on the bridge registering its
// rewind command after its tool_call handler: Pi lists the command only
// when the bridge loaded that far.
func (s *Session) checkGate(mode agent.PermissionMode) error {
	if gatesNothing(mode) || s.bridgeLoaded.Load() {
		return nil
	}
	return fmt.Errorf("pi: the bridge extension did not load, so permission mode %s cannot be enforced", mode)
}

// SetPermissionMode changes how the bridge gates tool calls.
func (s *Session) SetPermissionMode(ctx context.Context, mode agent.PermissionMode) error {
	if s.bridgeDisabled() {
		return providerkit.Unsupported(Name, "permission modes without the bridge extension")
	}
	if err := providerkit.CheckPermissionMode(Name, mode, s.Capabilities().PermissionModes); err != nil {
		return err
	}
	if err := s.checkGate(mode); err != nil {
		return err
	}
	s.updateInfo(agent.EventMeta{}, func(info *agent.SessionInfo) {
		s.mode = mode
		info.PermissionMode = cmp.Or(mode, agent.PermissionBypass)
	})
	return nil
}

// updateInfo applies change to Info under the session lock, then reports
// the new Info.
func (s *Session) updateInfo(meta agent.EventMeta, change func(*agent.SessionInfo)) {
	s.mu.Lock()
	change(&s.info)
	info := s.info
	s.mu.Unlock()
	s.rt.Emit(&agent.SessionUpdated{EventMeta: meta, Info: info})
}

// Close ends the session and waits for the process to exit.
func (s *Session) Close() error {
	s.shutdown()
	return nil
}

func (s *Session) shutdown() {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		if s.proc != nil {
			s.proc.Stop(s.provider.shutdownGrace())
			<-s.readDone
		}
		s.rt.Finish(nil)
	})
}

func (s *Session) exitError() error {
	if s.proc == nil {
		return nil
	}
	return s.proc.ExitError()
}

// Command sends an RPC command and returns its data. Use it for commands
// without a typed method.
func (s *Session) Command(ctx context.Context, typ string, payload map[string]any) (json.RawMessage, error) {
	return s.callOrdered(ctx, s.newRequestID(), typ, payload, nil)
}

func (s *Session) newRequestID() string { return "req_" + strconv.FormatUint(s.nextID.Add(1), 10) }

// waiter is a command waiting for its response.
type waiter struct {
	ch    chan rpcResponse
	reply func(rpcResponse)
}

// callOrdered sends command typ with request ID id and waits for its
// response. reply, when not nil, runs on the reading goroutine before any
// later line is handled; it must not block.
func (s *Session) callOrdered(ctx context.Context, id, typ string, payload map[string]any, reply func(rpcResponse)) (json.RawMessage, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	body := map[string]any{}
	maps.Copy(body, payload)
	body["id"] = id
	body["type"] = typ
	ch := make(chan rpcResponse, 1)
	s.waitMu.Lock()
	s.waiters[id] = waiter{ch: ch, reply: reply}
	s.waitMu.Unlock()
	defer func() {
		s.waitMu.Lock()
		delete(s.waiters, id)
		s.waitMu.Unlock()
	}()
	if err := s.proc.WriteJSON(body); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if !resp.Success {
			return nil, &agent.ProtocolError{Provider: Name, Method: typ, Message: resp.Error}
		}
		return resp.Data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.readDone:
		if err := s.exitError(); err != nil {
			return nil, err
		}
		return nil, s.rt.ClosedErr()
	}
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	if err := s.proc.ReadLines(s.handleLine); err != nil {
		s.rt.Logger().Warn("reading pi output failed", "error", err)
	}
	<-s.proc.Exited()
	if s.closing.Load() {
		s.rt.Finish(nil)
		return
	}
	s.rt.Finish(s.proc.ExitError())
}

func (s *Session) handleLine(line []byte) {
	var head struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		s.rt.Logger().Warn("ignoring malformed pi output", "error", err)
		return
	}
	switch head.Type {
	case "response":
		var resp rpcResponse
		if json.Unmarshal(line, &resp) != nil {
			return
		}
		s.waitMu.Lock()
		w, ok := s.waiters[resp.ID]
		s.waitMu.Unlock()
		if ok {
			if w.reply != nil {
				w.reply(resp)
			}
			w.ch <- resp
		} else if !resp.Success {
			s.rt.Emit(&agent.Notice{EventMeta: agent.EventMeta{Raw: line}, Level: agent.NoticeError, Code: resp.Command, Message: resp.Error})
		}
	case "extension_ui_request":
		s.extensionUI(line)
	default:
		s.tr.handle(head.Type, line)
	}
}

func (s *Session) uiRespond(id string, fields map[string]any) error {
	msg := map[string]any{"type": "extension_ui_response", "id": id}
	maps.Copy(msg, fields)
	return s.proc.WriteJSON(msg)
}

// extensionUI handles extension UI requests: bridge calls, dialogs, and
// fire-and-forget display updates.
func (s *Session) extensionUI(line []byte) {
	var req struct {
		ID          string   `json:"id"`
		Method      string   `json:"method"`
		Title       string   `json:"title"`
		Message     string   `json:"message"`
		Placeholder string   `json:"placeholder"`
		Prefill     string   `json:"prefill"`
		Options     []string `json:"options"`
		Timeout     *int64   `json:"timeout"`
		NotifyType  string   `json:"notifyType"`
		StatusKey   string   `json:"statusKey"`
		StatusText  string   `json:"statusText"`
	}
	if json.Unmarshal(line, &req) != nil {
		return
	}
	meta := agent.EventMeta{Raw: line}
	if req.Method == "input" && s.token != "" && req.Title == s.token {
		go s.bridgeCall(req.ID, req.Placeholder, meta)
		return
	}
	if req.Method == "setStatus" && s.token != "" {
		switch req.StatusKey {
		case s.token + ":event":
			s.subagentEvent(req.StatusText, meta)
			return
		case s.token + ":user":
			s.tr.userEntry(req.StatusText)
			return
		case s.token + ":rewind":
			var r rewindResult
			_ = json.Unmarshal([]byte(req.StatusText), &r)
			s.mu.Lock()
			ch := s.rewinds[r.Nonce]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- r:
				default:
				}
			}
			return
		}
	}
	switch req.Method {
	case "notify":
		level := agent.NoticeInfo
		switch req.NotifyType {
		case "warning":
			level = agent.NoticeWarning
		case "error":
			level = agent.NoticeError
		}
		s.rt.Emit(&agent.Notice{EventMeta: meta, Level: level, Code: "extension", Message: req.Message})
		return
	case "select", "confirm", "input", "editor":
	default:
		s.rt.Emit(&UIUpdate{EventMeta: meta, Method: req.Method, Fields: line})
		return
	}

	q := agent.Question{ID: "answer", Text: req.Title, Header: req.Title}
	switch req.Method {
	case "select":
		for _, o := range req.Options {
			q.Options = append(q.Options, agent.QuestionOption{Label: o})
		}
	case "confirm":
		q.Text = strings.TrimSpace(req.Title + "\n\n" + req.Message)
		q.Options = []agent.QuestionOption{{Label: "Yes"}, {Label: "No"}}
	case "input":
		q.Placeholder = req.Placeholder
	case "editor":
		q.Multiline = true
		q.Default = req.Prefill
	}
	method := req.Method
	ui := providerkit.NewUserInputRequest(agent.UserInputRequest{EventMeta: meta, ID: req.ID, Title: req.Title, Questions: []agent.Question{q}},
		func(ctx context.Context, resp agent.UserInputResponse) error {
			answer := resp.Answers["answer"]
			if resp.Cancelled || len(answer) == 0 {
				return s.uiRespond(req.ID, map[string]any{"cancelled": true})
			}
			if method == "confirm" {
				return s.uiRespond(req.ID, map[string]any{"confirmed": strings.EqualFold(answer[0], "yes")})
			}
			return s.uiRespond(req.ID, map[string]any{"value": strings.Join(answer, "\n")})
		})
	s.rt.Request(ui)
	if req.Timeout != nil && *req.Timeout > 0 {
		// Pi answers the dialog itself when its timeout expires.
		time.AfterFunc(time.Duration(*req.Timeout)*time.Millisecond, func() { providerkit.WithdrawRequest(ui) })
	}
}

type bridgeRequest struct {
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	ID         string          `json:"id"`
	Arguments  json.RawMessage `json:"arguments"`
	ToolName   string          `json:"toolName"`
	ToolCallID string          `json:"toolCallId"`
	Input      json.RawMessage `json:"input"`
	ReadOnly   bool            `json:"readOnly"`
	Source     string          `json:"source"`
}

func (s *Session) bridgeReply(uiID string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.uiRespond(uiID, map[string]any{"value": string(data)})
}

func (s *Session) bridgeCall(uiID, payload string, meta agent.EventMeta) {
	var req bridgeRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		_ = s.uiRespond(uiID, map[string]any{"cancelled": true})
		return
	}
	switch req.Kind {
	case "tool":
		tool, ok := s.tools[req.Name]
		var res agent.ToolResult
		if !ok {
			res = agent.ErrorResult("unknown tool " + req.Name)
		} else {
			// Interrupt cancels the call: Pi's abort waits for running tools.
			ctx, cancel := context.WithCancel(s.rt.Context())
			s.mu.Lock()
			s.toolCalls[uiID] = cancel
			s.mu.Unlock()
			res = providerkit.CallTool(ctx, tool, agent.ToolCallRequest{ID: req.ID, Name: req.Name, Arguments: req.Arguments, SessionID: s.ID()})
			s.mu.Lock()
			delete(s.toolCalls, uiID)
			s.mu.Unlock()
			cancel()
		}
		_ = s.bridgeReply(uiID, bridgeToolResult(res))
	case "permission":
		s.gate(uiID, req, meta)
	default:
		_ = s.uiRespond(uiID, map[string]any{"cancelled": true})
	}
}

func bridgeToolResult(res agent.ToolResult) map[string]any {
	out := map[string]any{"content": mcpserver.EncodeContent(res), "isError": res.IsError}
	if len(res.Structured) > 0 {
		out["structured"] = res.Structured
	}
	return out
}

var (
	readTools = map[string]bool{"read": true, "grep": true, "find": true, "ls": true}
	editTools = map[string]bool{"edit": true, "write": true}
)

// grantKey is what an AllowSession answer approves: the exact command of
// a bash call, the exact path of an edit or write, and otherwise the tool,
// since Pi's other tools carry no narrower identity the host can check.
func grantKey(tool, command, path string) string {
	switch {
	case tool == "bash":
		return "bash\x00command\x00" + command
	case editTools[tool]:
		return tool + "\x00path\x00" + path
	}
	return tool
}

// gate decides a tool call for the bridge, asking the host when the mode
// requires it.
func (s *Session) gate(uiID string, req bridgeRequest, meta agent.EventMeta) {
	allow := func() { _ = s.bridgeReply(uiID, map[string]any{"allow": true}) }
	deny := func(reason string) error {
		s.mu.Lock()
		s.denied[req.ToolCallID] = true
		s.mu.Unlock()
		return s.bridgeReply(uiID, map[string]any{"allow": false, "reason": reason})
	}
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal(req.Input, &in)
	grant := grantKey(req.ToolName, in.Command, in.Path)
	s.mu.Lock()
	mode := s.mode
	configured := slices.Contains(s.cfg.AllowedTools, req.ToolName) || s.approvedMCP[req.ToolName]
	granted := s.allowed[grant]
	s.mu.Unlock()
	// A read-only hint counts only from built-in extensions, such as MCP
	// servers and tool search; any other extension could claim it.
	readOnly := readTools[req.ToolName] || req.ReadOnly && strings.HasPrefix(req.Source, "builtin:")
	hostApproved := false
	if tool, ok := s.tools[req.ToolName]; ok {
		hostApproved = !tool.NeedsApproval
		readOnly = tool.ReadOnly
	}
	// The mode decides first, so no grant or allowed tool lifts the
	// refusals of readOnly, plan, and dontAsk.
	switch {
	case gatesNothing(mode), readOnly:
		// Read-only tools never ask, so every mode, dontAsk included, runs them.
		allow()
		return
	case mode == agent.PermissionReadOnly || mode == agent.PermissionPlan:
		deny(req.ToolName + " is not allowed in read-only mode")
		return
	case mode == agent.PermissionDontAsk:
		// Only the session's configuration pre-approves tools here.
		if configured {
			allow()
		} else {
			deny(req.ToolName + " is not pre-approved")
		}
		return
	case configured, granted, hostApproved:
		allow()
		return
	case mode == agent.PermissionAcceptEdits && editTools[req.ToolName]:
		allow()
		return
	}

	kind := agent.PermissionTool
	var command string
	var changes []agent.FileChange
	switch {
	case req.ToolName == "bash":
		kind = agent.PermissionCommand
		command = in.Command
	case editTools[req.ToolName]:
		kind = agent.PermissionFileChange
		changes = []agent.FileChange{{Path: in.Path, Kind: agent.FileModified}}
	}
	title := "Run " + req.ToolName
	if command != "" {
		title = "Run " + command
	}
	pr := providerkit.NewPermissionRequest(agent.PermissionRequest{
		EventMeta:  meta,
		ID:         uiID,
		Kind:       kind,
		ToolName:   req.ToolName,
		ToolCallID: req.ToolCallID,
		Input:      req.Input,
		Title:      title,
		Command:    command,
		Changes:    changes,
		Decisions:  []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny, agent.DecisionCancel},
	}, func(ctx context.Context, resp agent.PermissionResponse) error {
		if resp.ProviderResponse != nil {
			return s.bridgeReply(uiID, resp.ProviderResponse)
		}
		switch resp.Decision {
		case agent.DecisionDeny, agent.DecisionCancel:
			err := deny(cmp.Or(resp.Message, "The user denied this action."))
			if err == nil && resp.Decision == agent.DecisionCancel {
				go func() { _ = s.Interrupt(context.Background()) }()
			}
			return err
		case agent.DecisionAllowSession, agent.DecisionAllowAlways:
			s.mu.Lock()
			s.allowed[grant] = true
			s.mu.Unlock()
		}
		answer := map[string]any{"allow": true}
		if len(resp.UpdatedInput) > 0 {
			answer["input"] = resp.UpdatedInput
		}
		return s.bridgeReply(uiID, answer)
	})
	s.rt.Request(pr)
}

// subagentEvent maps a subagents:* event forwarded by the bridge to a task
// update.
func (s *Session) subagentEvent(text string, meta agent.EventMeta) {
	var ev struct {
		Event   string `json:"event"`
		Payload struct {
			ID           string          `json:"id"`
			Type         string          `json:"type"`
			Description  string          `json:"description"`
			IsBackground *bool           `json:"isBackground"`
			Result       string          `json:"result"`
			Error        string          `json:"error"`
			Status       string          `json:"status"`
			ToolUses     int             `json:"toolUses"`
			DurationMS   int64           `json:"durationMs"`
			Tokens       json.RawMessage `json:"tokens"`
			Usage        *piUsage        `json:"usage"`
			Message      string          `json:"message"`
		} `json:"payload"`
	}
	if json.Unmarshal([]byte(text), &ev) != nil || ev.Payload.ID == "" {
		return
	}
	p := ev.Payload
	s.mu.Lock()
	task := s.tasks[p.ID]
	if task == nil {
		task = &agent.Task{ID: p.ID, Kind: agent.TaskSubagent, NativeKind: "pi-subagent", Status: agent.TaskRunning, Subagent: &agent.Subagent{Depth: 1}}
		s.tasks[p.ID] = task
	}
	task.Description = cmp.Or(p.Description, task.Description)
	if p.Type != "" {
		task.Subagent.Type = p.Type
	}
	if p.IsBackground != nil {
		task.Subagent.Background = *p.IsBackground
	}
	switch ev.Event {
	case "created", "started":
		task.Status = agent.TaskRunning
	case "steered":
		task.Summary = "steered: " + p.Message
	case "compacted":
		task.Summary = "context compacted"
	case "completed", "failed":
		task.Status = agent.TaskCompleted
		switch {
		case p.Status == "stopped" || p.Status == "aborted":
			task.Status = agent.TaskStopped
		case ev.Event == "failed":
			task.Status = agent.TaskFailed
		}
		task.Output = p.Result
		task.Error = p.Error
		task.ToolUses = p.ToolUses
		task.Duration = time.Duration(p.DurationMS) * time.Millisecond
		if p.Usage != nil {
			u := p.Usage.usage()
			task.Usage = &u
		}
	}
	snapshot := task.Clone()
	if task.Status != agent.TaskRunning {
		delete(s.tasks, p.ID)
	}
	s.mu.Unlock()
	s.rt.Emit(&agent.TaskUpdated{EventMeta: meta, Task: snapshot})
}

// userEntry returns the session entry of turn id's prompt. An ID that is
// not a turn of this session is taken as a Pi entry ID, such as the ID of
// a user message item.
func (s *Session) userEntry(ctx context.Context, id string) (string, error) {
	if strings.HasPrefix(id, "v1-") {
		// ReadHistory numbers the entries of version 1 files, which Pi
		// gives new IDs when it opens them.
		return "", fmt.Errorf("pi: %s is from a version 1 session file; read the history again after resuming the session", id)
	}
	s.mu.Lock()
	entry, known := s.turnEntry[id]
	stamp, ok := s.turnStamp[id]
	s.mu.Unlock()
	if known {
		return entry, nil
	}
	if !ok {
		return id, nil
	}
	entries, _, err := s.Entries(ctx, "")
	if err != nil {
		return "", err
	}
	for _, raw := range slices.Backward(entries) {
		var e struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Message struct {
				Role      string `json:"role"`
				Timestamp int64  `json:"timestamp"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Type == "message" && e.Message.Role == "user" && e.Message.Timestamp == stamp {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("pi: no user message for turn %s", id)
}

// Rewind drops turnID and every later turn. turnID may also be the ID of a
// user message item, such as a steer, or a Pi user entry ID. With the
// bridge loaded, the session moves back in place and keeps its ID: the
// bridge navigates the session tree to before the message and records the
// new branch, so it survives reopening the session. Without the bridge,
// Rewind forks the session at the message; Pi writes the fork to a new
// session file, so the session ID changes.
func (s *Session) Rewind(ctx context.Context, turnID string) error {
	if !s.rt.Idle() {
		return errors.New("pi: cannot rewind while a turn runs or input is queued")
	}
	entry, err := s.userEntry(ctx, turnID)
	if err != nil {
		return err
	}
	return s.rewindTo(ctx, entry, "rewind")
}

// rewindTo rewinds to entry in place through the bridge, else by forking
// there. verb names the operation in the error an extension's cancel
// causes.
func (s *Session) rewindTo(ctx context.Context, entry, verb string) error {
	if !s.bridgeDisabled() && s.bridgeLoaded.Load() {
		return s.rewindInPlace(ctx, entry)
	}
	_, cancelled, err := s.forkAt(ctx, entry)
	if err == nil && cancelled {
		err = errors.New("pi: an extension cancelled the " + verb)
	}
	return err
}

// rewindInPlace runs the bridge's rewind command. Pi runs extension
// commands at once and answers "handled" after the command finished, and
// the bridge's answer arrives before that response.
func (s *Session) rewindInPlace(ctx context.Context, entry string) error {
	nonce := providerutil.NewUUID()
	payload, err := json.Marshal(map[string]string{"token": s.token, "nonce": nonce, "targetId": entry})
	if err != nil {
		return err
	}
	ch := make(chan rewindResult, 1)
	s.mu.Lock()
	s.rewinds[nonce] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.rewinds, nonce)
		s.mu.Unlock()
	}()
	raw, err := s.Command(ctx, "prompt", map[string]any{"message": "/" + rewindCommand + " " + base64.RawURLEncoding.EncodeToString(payload)})
	if err != nil {
		return err
	}
	var data struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(raw, &data)
	var res rewindResult
	select {
	case res = <-ch:
	default:
		return fmt.Errorf("pi: the bridge did not answer the rewind (disposition %q)", data.Disposition)
	}
	switch {
	case res.Cancelled:
		return errors.New("pi: an extension cancelled the rewind")
	case !res.OK:
		return fmt.Errorf("pi: rewind: %s", res.Error)
	}
	return s.refresh(ctx)
}

// truncateAfter keeps the conversation up to and including the turn whose
// prompt is entry, by rewinding to the next prompt on the active branch:
// in place through the bridge, else by forking there, which leaves the
// untruncated copy in its own session file.
func (s *Session) truncateAfter(ctx context.Context, entry string) error {
	points, err := s.ForkPoints(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(points, func(m ForkPoint) bool { return m.EntryID == entry })
	switch {
	case i < 0:
		return fmt.Errorf("pi: ForkAtTurn %s is not a prompt on the active branch", entry)
	case i == len(points)-1:
		return nil
	}
	// In place when it can, so the fork keeps its ID (Config.SessionID)
	// and no second copy is written.
	return s.rewindTo(ctx, points[i+1].EntryID, "fork")
}

// builtinCommand reports whether in is "/compact" or "/autocompact", which
// Pi runs only in its terminal UI, unless an extension defines the name.
func (s *Session) builtinCommand(in agent.Input) (name, args string, ok bool) {
	for _, p := range in.Parts {
		if p.Type != agent.PartText {
			return "", "", false
		}
	}
	name, args, found := slashCommand(in)
	if !found || name != "compact" && name != "autocompact" {
		return "", "", false
	}
	s.mu.Lock()
	overridden := slices.ContainsFunc(s.info.Commands, func(c agent.SlashCommand) bool { return c.Name == name && c.Scope != "builtin" })
	s.mu.Unlock()
	return name, strings.TrimSpace(args), !overridden
}

// extensionCommand reports whether input runs an extension command, which
// Pi runs with the rest of the prompt as its arguments.
func (s *Session) extensionCommand(in agent.Input) bool {
	name, _, found := slashCommand(in)
	if !found {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.info.Commands, func(c agent.SlashCommand) bool { return c.Name == name && c.Kind == "extension" })
}

// slashCommand splits input "/name args" into its name and arguments.
func slashCommand(in agent.Input) (name, args string, ok bool) {
	text, found := strings.CutPrefix(strings.TrimSpace(in.PlainText()), "/")
	if !found {
		return "", "", false
	}
	name, args, _ = strings.Cut(text, " ")
	return name, args, true
}

// builtinFailure is the result of built-in command turn id that failed
// with err: interrupted when an interrupt stopped it, else failed.
func (s *Session) builtinFailure(id string, err error) *agent.TurnResult {
	if interrupted, _ := s.rt.Interrupting(); interrupted == id {
		return &agent.TurnResult{Status: agent.TurnInterrupted, StopReason: "aborted", Err: &agent.TurnError{Code: "aborted", Err: agent.ErrInterrupted}}
	}
	return &agent.TurnResult{Status: agent.TurnFailed, StopReason: "error", Err: &agent.TurnError{Code: "error", Message: err.Error()}}
}

// runBuiltin runs a built-in command for turn id.
func (s *Session) runBuiltin(id, name, args string) {
	ctx := s.rt.Context()
	res := &agent.TurnResult{Status: agent.TurnSucceeded, StopReason: "handled"}
	var err error
	switch name {
	case "compact":
		var r *CompactionResult
		if r, err = s.compact(ctx, id, args); err == nil {
			res.Text = r.Summary
			res.Usage = r.usage()
		}
	case "autocompact":
		var enabled bool
		switch strings.ToLower(args) {
		case "on":
			enabled = true
		case "off":
		case "", "toggle":
			var st *State
			if st, err = s.State(ctx); err == nil {
				enabled = !st.AutoCompactionEnabled
			}
		default:
			err = errors.New("usage: /autocompact [on|off|toggle]")
		}
		if err == nil {
			err = s.SetAutoCompaction(ctx, enabled)
		}
		if err == nil {
			res.Text = "Auto-compaction disabled"
			if enabled {
				res.Text = "Auto-compaction enabled"
			}
		}
	}
	if err != nil {
		res = s.builtinFailure(id, err)
	}
	s.forgetBuiltin(id)
	s.rt.FinishTurn(id, res)
}

// mcpToolName returns Pi's name for an MCP server tool.
func mcpToolName(server, tool string) string {
	clean := func(r rune) rune {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}
	return "mcp__" + strings.Map(clean, server) + "__" + strings.Map(clean, tool)
}

// refreshContextUsage reports Pi's context-window estimate, which stays
// right after a compaction, unlike the last message's usage. It sends a
// command, so it must not run on the reader goroutine.
func (s *Session) refreshContextUsage(turnID string, u agent.Usage) {
	ctx, cancel := context.WithTimeout(s.rt.Context(), 10*time.Second)
	defer cancel()
	st, err := s.Stats(ctx)
	if err != nil || st.ContextUsage == nil || st.ContextUsage.Tokens == nil {
		return
	}
	window := st.ContextUsage.ContextWindow
	if *st.ContextUsage.Tokens == u.ContextTokens && (window == 0 || window == u.ContextWindow) {
		return
	}
	u.ContextTokens = *st.ContextUsage.Tokens
	if window > 0 {
		u.ContextWindow = window
	}
	if current, _ := s.rt.Running(); current != "" && current != turnID {
		// A later turn started; its own usage reports supersede this.
		return
	}
	s.rt.Emit(&agent.UsageUpdated{EventMeta: agent.EventMeta{TurnID: turnID}, Usage: u})
}

// SubagentSessionFile returns the session file of a subagent task, as
// reported by subagent extensions such as pi-subagents, or "" when
// unknown.
func (s *Session) SubagentSessionFile(taskID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subagentFiles[taskID]
}
