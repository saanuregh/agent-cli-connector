package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// Name is the provider name.
const Name = "claude"

// Provider starts Claude Code sessions.
type Provider struct {
	opts Options
}

var (
	_ agent.Provider       = (*Provider)(nil)
	_ agent.ModelLister    = (*Provider)(nil)
	_ agent.SessionLister  = (*Provider)(nil)
	_ agent.HistoryReader  = (*Provider)(nil)
	_ agent.SessionDeleter = (*Provider)(nil)
	_ agent.Checker        = (*Provider)(nil)
)

// New returns a provider.
func New(opts Options) *Provider {
	return &Provider{opts: opts}
}

// Name returns "claude".
func (p *Provider) Name() string { return Name }

// Capabilities reports the supported uniform features.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		Streaming:       true,
		ReasoningStream: true,
		Images:          true,
		Steer:           true,
		Queue:           true,
		Interrupt:       true,
		SetModel:        true,
		SetEffort:       true,
		PermissionModes: []agent.PermissionMode{
			agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionPlan,
			agent.PermissionBypass, agent.PermissionDontAsk, agent.PermissionAuto,
		},
		PermissionPrompt: true,
		UserQuestions:    true,
		Elicitation:      true,
		HostTools:        true,
		MCPServers:       true,
		Resume:           true,
		Fork:             true,
		SystemPrompt:     true,
		OutputSchema:     false,
		Plans:            true,
		Tasks:            true,
		Subagents:        true,
		CostReporting:    true,
		RateLimits:       true,
		Hooks:            true,
		Goals:            true,
		ForkAtTurn:       true,
	}
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
	return p.startSession(ctx, cfg, opts, false)
}

// ListModels starts a short-lived process and returns the models from the
// initialize handshake. Like Check, the process loads no MCP servers,
// hooks, or tools, but it reads user settings, so model overrides and
// gateway variables there apply.
func (p *Provider) ListModels(ctx context.Context) ([]agent.Model, error) {
	s, cleanup, err := p.isolatedSession(ctx, []string{"user"})
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// Raw keeps each model's native entry, fields this package does not
	// model included.
	var native struct {
		Models []json.RawMessage `json:"models"`
	}
	if init := s.Initialization(); init != nil {
		_ = json.Unmarshal(init.Raw, &native)
	}
	supported := s.SupportedModels()
	models := make([]agent.Model, 0, len(supported))
	for i, m := range supported {
		var raw json.RawMessage
		if len(native.Models) == len(supported) {
			raw = native.Models[i]
		} else {
			raw, _ = json.Marshal(m)
		}
		efforts := make([]agent.Effort, 0, len(m.SupportedEffortLevels))
		for _, e := range m.SupportedEffortLevels {
			efforts = append(efforts, agent.Effort(e))
		}
		resolved := ""
		if m.ResolvedModel != m.Value {
			resolved = m.ResolvedModel
		}
		models = append(models, agent.Model{
			ID:          m.Value,
			DisplayName: m.DisplayName,
			Description: m.Description,
			IsDefault:   m.Value == "default",
			ResolvedID:  resolved,
			Efforts:     efforts,
			Raw:         raw,
		})
	}
	return models, nil
}

func (p *Provider) path() string {
	return cmp.Or(p.opts.Path, "claude")
}

func (p *Provider) startupTimeout() time.Duration {
	if p.opts.StartupTimeout > 0 {
		return p.opts.StartupTimeout
	}
	return defaultStartupTimeout
}

func (p *Provider) shutdownGrace() time.Duration {
	if p.opts.ShutdownGrace > 0 {
		return p.opts.ShutdownGrace
	}
	return 5 * time.Second
}

// configDir returns the Claude config directory a session uses.
func (p *Provider) configDir(cfg agent.Config) string {
	if d := cfg.Env["CLAUDE_CONFIG_DIR"]; d != "" {
		return d
	}
	if d := p.opts.Env["CLAUDE_CONFIG_DIR"]; d != "" {
		return d
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// nativeEffort maps uniform effort levels Claude lacks to the nearest one.
func nativeEffort(e agent.Effort) agent.Effort {
	switch e {
	case agent.EffortNone, agent.EffortMinimal:
		return agent.EffortLow
	}
	return e
}

func flag(name, value string) string { return "--" + name + "=" + value }

func (s *Session) buildArgs(cfg agent.Config, o SessionOptions, sessionID string) ([]string, error) {
	args := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json"}

	if t := o.Thinking; t != nil {
		switch t.Type {
		case "enabled":
			if t.BudgetTokens > 0 {
				args = append(args, flag("max-thinking-tokens", strconv.Itoa(t.BudgetTokens)))
			} else {
				args = append(args, "--thinking=adaptive")
			}
		case "disabled":
			args = append(args, "--thinking=disabled")
		case "adaptive", "":
			args = append(args, "--thinking=adaptive")
		default:
			return nil, fmt.Errorf("claude: unknown thinking type %q", t.Type)
		}
		if t.Type != "disabled" && t.Display != "" {
			args = append(args, flag("thinking-display", t.Display))
		}
	}
	if cfg.Effort != "" {
		args = append(args, flag("effort", string(nativeEffort(cfg.Effort))))
	}
	if o.MaxTurns > 0 {
		args = append(args, flag("max-turns", strconv.Itoa(o.MaxTurns)))
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, flag("max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64)))
	}
	if o.TaskBudgetTokens > 0 {
		args = append(args, flag("task-budget", strconv.Itoa(o.TaskBudgetTokens)))
	}
	if cfg.Model != "" {
		args = append(args, flag("model", cfg.Model))
	}
	if o.Agent != "" {
		args = append(args, flag("agent", o.Agent))
	}
	if len(o.Betas) > 0 {
		args = append(args, flag("betas", strings.Join(o.Betas, ",")))
	}
	if len(cfg.OutputSchema) > 0 {
		args = append(args, flag("json-schema", string(cfg.OutputSchema)))
	}
	if o.DebugFile != "" {
		args = append(args, flag("debug-file", o.DebugFile))
	} else if o.Debug {
		args = append(args, "--debug")
	}
	args = append(args, flag("permission-prompt-tool", cmp.Or(o.PermissionPromptTool, "stdio")))
	if o.NoPermissionPrompts {
		args = append(args, flag("permission-prompts", "none"))
	}
	if cfg.Resume != "" {
		args = append(args, flag("resume", cfg.Resume))
	}

	allowed := append([]string(nil), cfg.AllowedTools...)
	for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
		for _, tool := range cfg.MCPServers[name].ApprovedTools {
			allowed = append(allowed, "mcp__"+name+"__"+tool)
		}
	}
	if s.tools != nil {
		for _, t := range cfg.Tools {
			if !t.NeedsApproval {
				allowed = append(allowed, "mcp__"+s.tools.Name()+"__"+t.Name)
			}
		}
	}
	if o.Skills != nil {
		if allSkills(o.Skills) {
			allowed = append(allowed, "Skill")
		} else {
			for _, name := range o.Skills {
				allowed = append(allowed, "Skill("+name+")")
			}
		}
	}
	if len(allowed) > 0 {
		args = append(args, flag("allowedTools", strings.Join(allowed, ",")))
	}
	if len(cfg.DisallowedTools) > 0 {
		args = append(args, flag("disallowedTools", strings.Join(cfg.DisallowedTools, ",")))
	}
	if o.BuiltinTools != nil {
		args = append(args, flag("tools", strings.Join(o.BuiltinTools, ",")))
	}
	if servers := mcpConfig(cfg.MCPServers); len(servers) > 0 && !mcpViaControl(cfg, o) {
		data, err := json.Marshal(map[string]any{"mcpServers": servers})
		if err != nil {
			return nil, fmt.Errorf("claude: encode MCP config: %w", err)
		}
		args = append(args, "--mcp-config", string(data))
	}
	if o.SettingSources != nil {
		args = append(args, flag("setting-sources", strings.Join(o.SettingSources, ",")))
	}
	if o.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	if cfg.PermissionMode != "" {
		args = append(args, flag("permission-mode", string(cfg.PermissionMode)))
	}
	if s.bypassAllowed(cfg, o) {
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if o.FallbackModel != "" {
		if o.FallbackModel == cfg.Model {
			return nil, fmt.Errorf("claude: FallbackModel must differ from Model")
		}
		args = append(args, flag("fallback-model", o.FallbackModel))
	}
	if o.IncludeHookEvents {
		args = append(args, "--include-hook-events")
	}
	if !o.NoPartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if o.ProjectConfigRoot != "" {
		args = append(args, flag("project-config-root", o.ProjectConfigRoot))
	}
	for _, dir := range cfg.AdditionalDirectories {
		args = append(args, flag("add-dir", dir))
	}
	for _, pl := range o.Plugins {
		if pl.SkipMCPDiscovery {
			args = append(args, flag("plugin-dir-no-mcp", pl.Path))
		} else {
			args = append(args, flag("plugin-dir", pl.Path))
		}
	}
	if cfg.Fork {
		args = append(args, "--fork-session")
	}
	if o.ResumeSessionAt != "" {
		args = append(args, flag("resume-session-at", o.ResumeSessionAt))
	}
	if o.ResumeDropsTurn != "" {
		args = append(args, flag("resume-drops-turn", o.ResumeDropsTurn))
	}
	if cfg.Resume == "" || cfg.Fork {
		args = append(args, flag("session-id", sessionID))
	}
	if cfg.Ephemeral {
		args = append(args, "--no-session-persistence")
	}
	if o.ManagedSettings != nil {
		data, err := settingsArg(o.ManagedSettings)
		if err != nil {
			return nil, err
		}
		args = append(args, "--managed-settings", data)
	}
	settings, err := mergedSettings(o.Settings, o.Sandbox)
	if err != nil {
		return nil, err
	}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	if s.spare {
		args = append(args, "--await-claim")
	}
	args = append(args, s.provider.opts.ExtraArgs...)
	args = append(args, o.ExtraArgs...)
	return args, nil
}

func settingsArg(v any) (string, error) {
	if str, ok := v.(string); ok {
		return str, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("claude: encode settings: %w", err)
	}
	return string(data), nil
}

// mergedSettings combines the settings option with the sandbox option the
// way the TypeScript SDK does: the sandbox block is merged into inline
// settings; a settings file path cannot be combined with it.
func mergedSettings(settings any, sandbox map[string]any) (string, error) {
	if sandbox == nil {
		if settings == nil {
			return "", nil
		}
		return settingsArg(settings)
	}
	obj := map[string]any{}
	switch s := settings.(type) {
	case nil:
	case string:
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return "", fmt.Errorf("claude: Sandbox cannot be combined with a settings file path; put the sandbox in the file")
		}
	default:
		data, err := json.Marshal(s)
		if err != nil {
			return "", fmt.Errorf("claude: encode settings: %w", err)
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			return "", fmt.Errorf("claude: settings must be an object: %w", err)
		}
	}
	merged := map[string]any{}
	if existing, ok := obj["sandbox"].(map[string]any); ok {
		maps.Copy(merged, existing)
	}
	maps.Copy(merged, sandbox)
	if enabled, _ := merged["enabled"].(bool); enabled {
		if _, ok := merged["failIfUnavailable"]; !ok {
			merged["failIfUnavailable"] = true
		}
	}
	obj["sandbox"] = merged
	data, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// mcpConfig renders MCP servers in the CLI's config shape. ToolTimeout
// becomes the per-server "timeout" in milliseconds; the CLI has no
// per-server startup timeout or working directory (see buildEnv for
// StartupTimeout; Cwd is not sent). ApprovedTools of http and sse servers
// become "always_allow" per-tool policies; the CLI reads none for stdio
// servers.
func mcpConfig(servers map[string]agent.MCPServer) map[string]any {
	out := make(map[string]any, len(servers))
	for name, srv := range servers {
		if srv.Enabled != nil && !*srv.Enabled {
			continue
		}
		var entry map[string]any
		switch srv.TransportOrDefault() {
		case agent.MCPHTTP, agent.MCPSSE:
			entry = map[string]any{"type": string(srv.TransportOrDefault()), "url": srv.URL}
			if len(srv.Headers) > 0 {
				entry["headers"] = srv.Headers
			}
			if len(srv.ApprovedTools) > 0 {
				policies := make([]map[string]any, 0, len(srv.ApprovedTools))
				for _, tool := range srv.ApprovedTools {
					policies = append(policies, map[string]any{"name": tool, "permission_policy": "always_allow"})
				}
				entry["tools"] = policies
			}
		default:
			entry = map[string]any{"type": "stdio", "command": srv.Command}
			if len(srv.Args) > 0 {
				entry["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				entry["env"] = srv.Env
			}
		}
		if srv.ToolTimeout > 0 {
			entry["timeout"] = srv.ToolTimeout.Milliseconds()
		}
		if srv.AlwaysLoad {
			entry["alwaysLoad"] = true
		}
		out[name] = entry
	}
	return out
}

// mcpViaControl reports whether cfg.MCPServers are registered with the
// mcp_set_servers control request instead of the --mcp-config flag: when
// SessionOptions.MCPViaControl asks for it, and whenever a server has
// values that may be secrets, which other local users could read from the
// command line: headers, environment variables, arguments, or a URL with
// credentials or a query string.
func mcpViaControl(cfg agent.Config, o SessionOptions) bool {
	if o.MCPViaControl {
		return true
	}
	for _, srv := range cfg.MCPServers {
		if (srv.Enabled == nil || *srv.Enabled) && (len(srv.Headers) > 0 || len(srv.Env) > 0 || len(srv.Args) > 0 || urlMayHoldSecret(srv.URL)) {
			return true
		}
	}
	return false
}

// urlMayHoldSecret reports whether a server URL has credentials or a query
// string, or cannot be parsed to tell.
func urlMayHoldSecret(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery
}

// bypassAllowed reports whether the CLI is launched so that it accepts
// bypassPermissions mode.
func (s *Session) bypassAllowed(cfg agent.Config, o SessionOptions) bool {
	return o.AllowBypassPermissions || cfg.PermissionMode == agent.PermissionBypass ||
		slices.Contains(o.ExtraArgs, "--allow-dangerously-skip-permissions") ||
		slices.Contains(s.provider.opts.ExtraArgs, "--allow-dangerously-skip-permissions")
}

// parentSessionEnv marks a process as running inside a Claude Code
// session; inherited, it makes the CLI behave as a nested session.
var parentSessionEnv = []string{"CLAUDECODE", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_AGENT_SDK_VERSION"}

// sdkUnsetEnv are inherited variables the TypeScript SDK removes too:
// Node.js options and debug switches meant for the host, and a progress
// summary switch that SessionOptions.AgentProgressSummaries owns.
var sdkUnsetEnv = []string{"CLAUDE_CODE_AGENT_PROGRESS_SUMMARIES", "NODE_OPTIONS", "DEBUG"}

// toolServerSettings returns the settings of the in-process host tool
// server that initialize (sdkMcpServerConfigs) and every mcp_set_servers
// entry for it carry: the tool timeout and the "always_ask" policy of
// tools that need approval. It returns nil when there are none.
func (s *Session) toolServerSettings() map[string]any {
	if s.tools == nil {
		return nil
	}
	settings := map[string]any{}
	if s.opts.ToolTimeout > 0 {
		settings["timeout"] = s.opts.ToolTimeout.Milliseconds()
	}
	var policies []map[string]any
	for _, t := range s.cfg.Tools {
		if t.NeedsApproval {
			policies = append(policies, map[string]any{"name": t.Name, "permission_policy": "always_ask"})
		}
	}
	if len(policies) > 0 {
		settings["tools"] = policies
	}
	if len(settings) == 0 {
		return nil
	}
	return settings
}

// mcpStartupTimeout returns the longest StartupTimeout of the enabled
// servers. The CLI only has the process-wide MCP_TIMEOUT for it.
func mcpStartupTimeout(servers map[string]agent.MCPServer) time.Duration {
	var d time.Duration
	for _, srv := range servers {
		if srv.Enabled == nil || *srv.Enabled {
			d = max(d, srv.StartupTimeout)
		}
	}
	return d
}

func pluginConfigs(plugins []Plugin) []map[string]any {
	out := make([]map[string]any, 0, len(plugins))
	for _, p := range plugins {
		entry := map[string]any{"type": "local", "path": p.Path}
		if p.SkipMCPDiscovery {
			entry["skipMcpDiscovery"] = true
		}
		out = append(out, entry)
	}
	return out
}

func (s *Session) buildEnv() []string {
	env := map[string]string{
		"CLAUDE_CODE_ENTRYPOINT":              "sdk-ts",
		"CLAUDE_AGENT_SDK_VERSION":            ProtocolVersion,
		"CLAUDE_CODE_SDK_READS_SESSION_STATE": "1",
	}
	if app := s.provider.opts.ClientApp; app != "" {
		env["CLAUDE_AGENT_SDK_CLIENT_APP"] = app
	}
	if s.opts.EnableFileCheckpointing {
		env["CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING"] = "true"
	}
	if d := mcpStartupTimeout(s.cfg.MCPServers); d > 0 {
		env["MCP_TIMEOUT"] = strconv.FormatInt(d.Milliseconds(), 10)
	}
	if q, ok := s.opts.ToolConfig["askUserQuestion"].(map[string]any); ok {
		if f, ok := q["previewFormat"].(string); ok && f != "" {
			env["CLAUDE_CODE_QUESTION_PREVIEW_FORMAT"] = f
		}
	}
	maps.Copy(env, s.provider.opts.Env)
	maps.Copy(env, s.cfg.Env)
	return providerkit.EnvList(env)
}

func (s *Session) initializeRequest() map[string]any {
	cfg, o := s.cfg, s.opts
	req := map[string]any{}
	if hooks := s.registerHooks(); len(hooks) > 0 {
		req["hooks"] = hooks
	}
	if s.tools != nil {
		req["sdkMcpServers"] = []string{s.tools.Name()}
		if settings := s.toolServerSettings(); settings != nil {
			req["sdkMcpServerConfigs"] = map[string]any{s.tools.Name(): settings}
		}
	}
	if len(cfg.OutputSchema) > 0 {
		req["jsonSchema"] = cfg.OutputSchema
	}
	switch {
	case len(o.SystemPromptBlocks) > 0:
		req["systemPrompt"] = o.SystemPromptBlocks
	case cfg.SystemPrompt != "":
		req["systemPrompt"] = []string{cfg.SystemPrompt}
	}
	if cfg.AppendSystemPrompt != "" {
		req["appendSystemPrompt"] = cfg.AppendSystemPrompt
	}
	if o.PlanModeInstructions != "" {
		req["planModeInstructions"] = o.PlanModeInstructions
	}
	if o.SystemPromptSnapshot != nil {
		req["systemPromptSnapshot"] = *o.SystemPromptSnapshot
	}
	if o.AppendSubagentSystemPrompt != "" {
		req["appendSubagentSystemPrompt"] = o.AppendSubagentSystemPrompt
	}
	if len(o.ToolAliases) > 0 {
		req["toolAliases"] = o.ToolAliases
	}
	if o.ExcludeDynamicSections {
		req["excludeDynamicSections"] = true
	}
	if len(o.Agents) > 0 {
		req["agents"] = o.Agents
	}
	if cfg.Title != "" {
		req["title"] = cfg.Title
	}
	if o.Skills != nil && !allSkills(o.Skills) {
		req["skills"] = o.Skills
	}
	if o.PromptSuggestions {
		req["promptSuggestions"] = true
	}
	if o.AgentProgressSummaries {
		req["agentProgressSummaries"] = true
	}
	req["forwardSubagentText"] = !o.HideSubagentText
	if len(o.SupportedDialogKinds) > 0 {
		req["supportedDialogKinds"] = o.SupportedDialogKinds
	}
	if !o.NoPerTaskStopAffordance {
		req["perTaskStopAffordance"] = true
	}
	maps.Copy(req, o.InitializeExtra)
	return req
}

// registerHooks assigns callback IDs to hook functions and returns the
// initialize payload describing them. It reuses IDs on re-initialize.
func (s *Session) registerHooks() map[string]any {
	all := s.opts.Hooks
	if s.opts.TrackSubagentEffort {
		all = maps.Clone(all)
		if all == nil {
			all = map[HookEvent][]HookMatcher{}
		}
		observe := HookMatcher{Hooks: []HookFunc{s.observeSubagentEffort}}
		for _, event := range []HookEvent{HookPreToolUse, HookPostToolUse, HookSubagentStop} {
			all[event] = append(slices.Clone(all[event]), observe)
		}
	}
	if len(all) == 0 {
		return nil
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	clear(s.hooks)
	out := make(map[string]any, len(all))
	n := 0
	for _, event := range slices.Sorted(maps.Keys(all)) {
		matchers := all[event]
		list := make([]map[string]any, 0, len(matchers))
		for _, m := range matchers {
			ids := make([]string, 0, len(m.Hooks))
			for _, fn := range m.Hooks {
				id := "hook_" + strconv.Itoa(n)
				n++
				s.hooks[id] = hookRef{event: event, fn: fn}
				ids = append(ids, id)
			}
			entry := map[string]any{"hookCallbackIds": ids}
			if m.Matcher != "" {
				entry["matcher"] = m.Matcher
			}
			if m.Timeout > 0 {
				entry["timeout"] = int(m.Timeout.Seconds())
			}
			list = append(list, entry)
		}
		out[string(event)] = list
	}
	return out
}

// observeSubagentEffort is the hook TrackSubagentEffort registers. It
// changes nothing; it reads the effort level a subagent runs at, which the
// CLI reports only to hooks.
func (s *Session) observeSubagentEffort(_ context.Context, in HookInput) (HookOutput, error) {
	if in.AgentID != "" && in.Effort != nil && in.Effort.Level != "" {
		s.translator().subagentEffort(in.AgentID, agent.Effort(in.Effort.Level))
	}
	return HookOutput{}, nil
}

// allSkills reports whether skills selects every skill.
func allSkills(skills []string) bool { return len(skills) == 1 && skills[0] == "all" }
