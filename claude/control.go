package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// InitializeResponse is the CLI's answer to the initialize handshake.
type InitializeResponse struct {
	Commands              []slashCommandWire `json:"commands"`
	Agents                []AgentInfo        `json:"agents"`
	OutputStyle           string             `json:"output_style"`
	AvailableOutputStyles []string           `json:"available_output_styles"`
	Models                []ModelInfo        `json:"models"`
	Account               AccountInfo        `json:"account"`
	Version               string             `json:"claude_code_version"`
	FastModeState         string             `json:"fast_mode_state"`
	// CurrentPermissionMode is the mode the CLI started in.
	CurrentPermissionMode string `json:"current_permission_mode"`
	// Capabilities lists the CLI's protocol capabilities; see
	// Session.CLICapabilities.
	Capabilities []string `json:"capabilities"`
	// Raw holds the full response.
	Raw json.RawMessage `json:"-"`

	// appliedEffort is the effort the CLI's settings apply, read right
	// after initialize.
	appliedEffort agent.Effort
}

type slashCommandWire struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ArgumentHint string   `json:"argumentHint"`
	Aliases      []string `json:"aliases"`
	Builtin      bool     `json:"builtin"`
	Plugin       string   `json:"plugin"`
}

// defaultModel returns the model the CLI picks when none is set: the
// "default" entry's resolved model, or "" when it reports none.
func (r *InitializeResponse) defaultModel() string {
	for _, m := range r.Models {
		if m.Value == "default" {
			return m.ResolvedModel
		}
	}
	return ""
}

func (r *InitializeResponse) slashCommands() []agent.SlashCommand {
	return convertCommands(r.Commands)
}

func convertCommands(in []slashCommandWire) []agent.SlashCommand {
	out := make([]agent.SlashCommand, 0, len(in))
	for _, c := range in {
		kind, scope := "command", "builtin"
		if !c.Builtin {
			kind, scope = "skill", ""
		}
		if c.Plugin != "" {
			scope = "plugin"
		}
		out = append(out, agent.SlashCommand{Name: c.Name, Description: c.Description, ArgumentHint: c.ArgumentHint, Kind: kind, Scope: scope})
	}
	return out
}

// AgentInfo describes an available subagent.
type AgentInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
}

// ModelInfo describes a model the CLI offers.
type ModelInfo struct {
	Value                    string   `json:"value"`
	ResolvedModel            string   `json:"resolvedModel"`
	DisplayName              string   `json:"displayName"`
	Description              string   `json:"description"`
	SupportsEffort           bool     `json:"supportsEffort"`
	SupportedEffortLevels    []string `json:"supportedEffortLevels"`
	SupportsAdaptiveThinking bool     `json:"supportsAdaptiveThinking"`
	SupportsThinkingOff      bool     `json:"supportsThinkingOff"`
	SupportsFastMode         bool     `json:"supportsFastMode"`
	SupportsAutoMode         bool     `json:"supportsAutoMode"`
}

// AccountInfo describes the signed-in account.
type AccountInfo struct {
	Email            string `json:"email"`
	Organization     string `json:"organization"`
	SubscriptionType string `json:"subscriptionType"`
	TokenSource      string `json:"tokenSource"`
	APIKeySource     string `json:"apiKeySource"`
	APIProvider      string `json:"apiProvider"`
}

func (s *Session) controlInto(ctx context.Context, subtype string, payload, out any) error {
	raw, err := s.Control(ctx, subtype, payload)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("claude: decode %s response: %w", subtype, err)
	}
	return nil
}

// Reinitialize sends the initialize request again, for example after a
// transport gap. Permission prompts and dialogs the CLI lists as pending in
// its answer are delivered again, unless they are still open here.
func (s *Session) Reinitialize(ctx context.Context) (*InitializeResponse, error) {
	raw, err := s.Control(ctx, "initialize", s.initializeRequest())
	if err != nil {
		return nil, err
	}
	r, err := s.decodeInitialize(raw)
	if err != nil {
		return nil, err
	}
	s.infoMu.Lock()
	s.init = r
	s.infoMu.Unlock()
	return r, nil
}

// applyInitLocked records a new initialize response; s.infoMu must be held.
func (s *Session) applyInitLocked(init *InitializeResponse) {
	s.init = init
	s.info.Version = cmp.Or(init.Version, s.info.Version)
	s.info.Commands = init.slashCommands()
	s.info.Raw = init.Raw
}

// decodeInitialize decodes an initialize response and applies its capabilities.
func (s *Session) decodeInitialize(raw json.RawMessage) (*InitializeResponse, error) {
	var r InitializeResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	r.Raw = raw
	s.setCapabilities(r.Capabilities)
	return &r, nil
}

// SetMaxThinkingTokens limits thinking tokens; nil clears the limit.
// display is "summarized", "omitted", "highlights", or empty to keep the
// current display.
func (s *Session) SetMaxThinkingTokens(ctx context.Context, tokens *int, display string) error {
	payload := map[string]any{"max_thinking_tokens": tokens}
	if display != "" {
		payload["thinking_display"] = display
	}
	_, err := s.Control(ctx, "set_max_thinking_tokens", payload)
	return err
}

// ApplyFlagSettings merges settings into the flag settings layer. A nil
// value clears a key.
func (s *Session) ApplyFlagSettings(ctx context.Context, settings map[string]any) error {
	_, err := s.Control(ctx, "apply_flag_settings", map[string]any{"settings": settings})
	return err
}

// UpdateSettings writes allow-listed keys to a settings file ("localSettings"
// or "userSettings") through the CLI's own writer.
func (s *Session) UpdateSettings(ctx context.Context, source string, settings map[string]any) error {
	_, err := s.Control(ctx, "update_settings", map[string]any{"source": source, "settings": settings})
	return err
}

// Settings returns the effective settings with their sources.
func (s *Session) Settings(ctx context.Context) (json.RawMessage, error) {
	return s.Control(ctx, "get_settings", nil)
}

// MCPServerDetail is the status of one MCP server.
type MCPServerDetail struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	Scope      string `json:"scope"`
	Source     string `json:"source"`
	ServerInfo *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"tools"`
	Config json.RawMessage `json:"config"`
}

// MCPServerStatus reports every configured MCP server.
func (s *Session) MCPServerStatus(ctx context.Context) ([]MCPServerDetail, error) {
	var r struct {
		Servers []MCPServerDetail `json:"mcpServers"`
	}
	err := s.controlInto(ctx, "mcp_status", nil, &r)
	return r.Servers, err
}

// MCPSetServersResult reports the outcome of SetMCPServers.
type MCPSetServersResult struct {
	Added   []string          `json:"added"`
	Removed []string          `json:"removed"`
	Errors  map[string]string `json:"errors"`
}

// SetMCPServers replaces the dynamically added MCP servers. The in-process
// host tool server stays registered with its tool policies and timeout.
// ApprovedTools apply to http and sse servers only; see the package
// documentation.
func (s *Session) SetMCPServers(ctx context.Context, servers map[string]agent.MCPServer) (*MCPSetServersResult, error) {
	cfg := mcpConfig(servers)
	if s.tools != nil {
		// Without its settings the CLI would drop the host server's tool
		// policies.
		entry := map[string]any{"type": "sdk", "name": s.tools.Name()}
		maps.Copy(entry, s.toolServerSettings())
		cfg[s.tools.Name()] = entry
	}
	var r MCPSetServersResult
	err := s.controlInto(ctx, "mcp_set_servers", map[string]any{"servers": cfg}, &r)
	return &r, err
}

// ReconnectMCPServer reconnects an MCP server.
func (s *Session) ReconnectMCPServer(ctx context.Context, name string) error {
	_, err := s.Control(ctx, "mcp_reconnect", map[string]any{"serverName": name})
	return err
}

// ToggleMCPServer enables or disables an MCP server.
func (s *Session) ToggleMCPServer(ctx context.Context, name string, enabled bool) error {
	_, err := s.Control(ctx, "mcp_toggle", map[string]any{"serverName": name, "enabled": enabled})
	return err
}

// ReadMCPResource reads a ui:// resource from an MCP server.
func (s *Session) ReadMCPResource(ctx context.Context, server, uri string) (json.RawMessage, error) {
	return s.Control(ctx, "mcp_read_resource", map[string]any{"serverName": server, "uri": uri})
}

// MCPAuthenticate starts OAuth for an MCP server. redirectURI may be empty.
func (s *Session) MCPAuthenticate(ctx context.Context, server, redirectURI string) (json.RawMessage, error) {
	payload := map[string]any{"serverName": server}
	if redirectURI != "" {
		payload["redirectUri"] = redirectURI
	}
	return s.Control(ctx, "mcp_authenticate", payload)
}

// MCPClearAuth removes stored OAuth credentials of an MCP server.
func (s *Session) MCPClearAuth(ctx context.Context, server string) (json.RawMessage, error) {
	return s.Control(ctx, "mcp_clear_auth", map[string]any{"serverName": server})
}

// MCPSubmitOAuthCallback completes an MCP OAuth flow with the callback URL.
func (s *Session) MCPSubmitOAuthCallback(ctx context.Context, server, callbackURL string) (json.RawMessage, error) {
	return s.Control(ctx, "mcp_oauth_callback_url", map[string]any{"serverName": server, "callbackUrl": callbackURL})
}

// SetMCPPermissionOverride pins an MCP server to "default" or "auto"
// permission handling; empty clears the override.
func (s *Session) SetMCPPermissionOverride(ctx context.Context, server, mode string) (warning string, err error) {
	var m any
	if mode != "" {
		m = mode
	}
	var r struct {
		Warning string `json:"warning"`
	}
	err = s.controlInto(ctx, "set_mcp_permission_mode_override", map[string]any{"serverName": server, "mode": m}, &r)
	return r.Warning, err
}

// ContextUsage returns the context-window breakdown. full counts every
// category with the token-count API; otherwise estimates are used.
func (s *Session) ContextUsage(ctx context.Context, full bool) (json.RawMessage, error) {
	detail := "summary"
	if full {
		detail = "full"
	}
	return s.Control(ctx, "get_context_usage", map[string]any{"detail": detail})
}

// RateLimits reads the claude.ai plan usage windows through get_usage.
// It returns no windows when plan limits do not apply, such as with an API
// key. Model-scoped windows carry the model's display name in LimitID.
func (s *Session) RateLimits(ctx context.Context) ([]agent.RateLimit, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var r struct {
		Available bool `json:"rate_limits_available"`
		Limits    *struct {
			FiveHour    *window `json:"five_hour"`
			SevenDay    *window `json:"seven_day"`
			OAuthApps   *window `json:"seven_day_oauth_apps"`
			Opus        *window `json:"seven_day_opus"`
			Sonnet      *window `json:"seven_day_sonnet"`
			ModelScoped []struct {
				window
				DisplayName string `json:"display_name"`
			} `json:"model_scoped"`
		} `json:"rate_limits"`
	}
	if err := s.controlInto(ctx, "get_usage", map[string]any{"skip_behaviors": true}, &r); err != nil {
		return nil, err
	}
	out := []agent.RateLimit{}
	if !r.Available || r.Limits == nil {
		return out, nil
	}
	add := func(name, limitID string, w *window) {
		if w == nil || w.Utilization == nil {
			return
		}
		l := agent.RateLimit{Name: name, LimitID: limitID, UsedPercent: *w.Utilization, Window: cmp.Or(rateLimitWindows[name], 7*24*time.Hour), Status: agent.RateLimitOK,
			ResetsAt: providerutil.ParseTime(providerutil.Deref(w.ResetsAt))}
		if l.UsedPercent >= 100 {
			l.Status = agent.RateLimitExceeded
		}
		out = append(out, l)
	}
	add("five_hour", "", r.Limits.FiveHour)
	add("seven_day", "", r.Limits.SevenDay)
	add("seven_day_oauth_apps", "", r.Limits.OAuthApps)
	add("seven_day_opus", "", r.Limits.Opus)
	add("seven_day_sonnet", "", r.Limits.Sonnet)
	for i, m := range r.Limits.ModelScoped {
		add("seven_day_"+scopedName(m.DisplayName), m.DisplayName, &m.window)
		if i == 0 {
			// The CLI lists only overage-included models here; rate-limit
			// events name the first one "seven_day_overage_included".
			tr := s.translator()
			tr.mu.Lock()
			tr.overageLimit = m.DisplayName
			tr.mu.Unlock()
		}
	}
	return out, nil
}

// scopedName turns a model display name into a window name part, such as
// "opus_4_5" for "Opus 4.5".
func scopedName(display string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.ToLower(display) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			under = false
		} else if !under && b.Len() > 0 {
			b.WriteByte('_')
			under = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// PlanUsage returns session cost and claude.ai plan rate-limit usage. The
// CLI marks this API experimental.
func (s *Session) PlanUsage(ctx context.Context, skipBehaviors bool) (json.RawMessage, error) {
	payload := map[string]any{}
	if skipBehaviors {
		payload["skip_behaviors"] = true
	}
	return s.Control(ctx, "get_usage", payload)
}

// ReadFile reads a file through the session's read permissions. encoding is
// "utf-8" or "base64"; maxBytes zero means the CLI default.
func (s *Session) ReadFile(ctx context.Context, path string, maxBytes int, encoding string) (json.RawMessage, error) {
	payload := map[string]any{"path": path}
	if maxBytes > 0 {
		payload["max_bytes"] = maxBytes
	}
	if encoding != "" {
		payload["encoding"] = encoding
	}
	return s.Control(ctx, "read_file", payload)
}

// ReloadPlugins reloads plugins, optionally adding more. With
// holdOnCacheImpact the reload is held when it would invalidate the prompt
// cache.
func (s *Session) ReloadPlugins(ctx context.Context, add []Plugin, holdOnCacheImpact bool) (json.RawMessage, error) {
	payload := map[string]any{}
	if holdOnCacheImpact {
		payload["hold_on_cache_impact"] = true
	}
	if len(add) > 0 {
		payload["add_plugins"] = pluginConfigs(add)
	}
	return s.Control(ctx, "reload_plugins", payload)
}

// ReloadSkills reloads skills from disk.
func (s *Session) ReloadSkills(ctx context.Context) (json.RawMessage, error) {
	return s.Control(ctx, "reload_skills", nil)
}

// ReloadOutputStyles reloads output styles from disk.
func (s *Session) ReloadOutputStyles(ctx context.Context) (json.RawMessage, error) {
	return s.Control(ctx, "reload_output_styles", nil)
}

// Account returns the signed-in account from the initialize response.
func (s *Session) Account() AccountInfo {
	if init := s.Initialization(); init != nil {
		return init.Account
	}
	return AccountInfo{}
}

// SupportedModels returns the models from the initialize response.
func (s *Session) SupportedModels() []ModelInfo {
	if init := s.Initialization(); init != nil {
		return init.Models
	}
	return nil
}

// SupportedAgents returns the subagents from the initialize response.
func (s *Session) SupportedAgents() []AgentInfo {
	if init := s.Initialization(); init != nil {
		return init.Agents
	}
	return nil
}

// RewindFilesResult reports a file rewind.
type RewindFilesResult struct {
	CanRewind    bool     `json:"canRewind"`
	Error        string   `json:"error"`
	FilesChanged []string `json:"filesChanged"`
	Insertions   int      `json:"insertions"`
	Deletions    int      `json:"deletions"`
}

// RewindFiles restores tracked files to their state at a user message.
// It needs SessionOptions.EnableFileCheckpointing.
func (s *Session) RewindFiles(ctx context.Context, userMessageID string, dryRun bool) (*RewindFilesResult, error) {
	var r RewindFilesResult
	err := s.controlInto(ctx, "rewind_files", map[string]any{"user_message_id": userMessageID, "dry_run": dryRun}, &r)
	return &r, err
}

// SeedReadState tells the CLI the file at path was read when its
// modification time was mtime, so a later Edit does not demand a fresh
// Read.
func (s *Session) SeedReadState(ctx context.Context, path string, mtime time.Time) error {
	_, err := s.Control(ctx, "seed_read_state", map[string]any{"path": path, "mtime": mtime.UnixMilli()})
	return err
}

// StopTask stops a background task. taskID is a Task.ID this session
// reported; an unknown ID is an error.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	// The CLI accepts any ID, so an unknown one is refused here.
	if _, known := s.tasks.Load(taskID); !known {
		return fmt.Errorf("claude: unknown task %s", taskID)
	}
	_, err := s.Control(ctx, "stop_task", map[string]any{"task_id": taskID})
	return err
}

// BackgroundTasks moves running foreground tasks to the background. An
// empty toolUseID backgrounds all of them.
func (s *Session) BackgroundTasks(ctx context.Context, toolUseID string) (bool, error) {
	payload := map[string]any{}
	if toolUseID != "" {
		payload["tool_use_id"] = toolUseID
	}
	var r struct {
		Backgrounded *bool `json:"backgrounded"`
	}
	if err := s.controlInto(ctx, "background_tasks", payload, &r); err != nil {
		return false, err
	}
	return r.Backgrounded == nil || *r.Backgrounded, nil
}

// TaskOutput returns the output of a background task.
func (s *Session) TaskOutput(ctx context.Context, taskID string) (json.RawMessage, error) {
	return s.Control(ctx, "get_task_output", map[string]any{"task_id": taskID})
}

// PlanFile is the session's plan file, as written in plan mode.
type PlanFile struct {
	Exists  bool   `json:"exists"`
	Content string `json:"content"`
	// Path is the file's location; the CLI leaves it out for some remote
	// sessions.
	Path string `json:"path"`
}

// Plan reads the session's plan file through get_plan.
func (s *Session) Plan(ctx context.Context) (*PlanFile, error) {
	var r PlanFile
	if err := s.controlInto(ctx, "get_plan", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// FileSuggestions returns the @-mention completions the CLI offers for a
// partial path, as paths relative to the session's working directory.
func (s *Session) FileSuggestions(ctx context.Context, query string) ([]string, error) {
	var r struct {
		Suggestions []struct {
			Path string `json:"path"`
		} `json:"suggestions"`
	}
	if err := s.controlInto(ctx, "file_suggestions", map[string]any{"query": query}, &r); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.Suggestions))
	for _, sg := range r.Suggestions {
		out = append(out, sg.Path)
	}
	return out, nil
}

// CancelQueuedMessage removes a queued user message before it runs.
func (s *Session) CancelQueuedMessage(ctx context.Context, messageUUID string) (bool, error) {
	var r struct {
		Cancelled bool `json:"cancelled"`
	}
	err := s.controlInto(ctx, "cancel_async_message", map[string]any{"message_uuid": messageUUID}, &r)
	if r.Cancelled {
		s.rt.AbandonTurn(messageUUID, agent.ErrCancelled)
	}
	return r.Cancelled, err
}

// SetTitle renames the session.
func (s *Session) SetTitle(ctx context.Context, title string) error {
	_, err := s.Control(ctx, "rename_session", map[string]any{"title": title, "source": "host", "session_id": s.ID()})
	if err == nil {
		s.updateInfo(func(i *agent.SessionInfo) { i.Title = title })
	}
	return err
}

// GenerateTitle asks the model for a session title from a description.
// With persist the title is saved.
func (s *Session) GenerateTitle(ctx context.Context, description string, persist bool) (string, error) {
	var r struct {
		Title string `json:"title"`
	}
	err := s.controlInto(ctx, "generate_session_title", map[string]any{"description": description, "persist": persist}, &r)
	return r.Title, err
}

// SideQuestion asks a question about the conversation without adding it to
// the transcript. It returns the answer text, or "" when the CLI gave none.
func (s *Session) SideQuestion(ctx context.Context, question string) (string, error) {
	var r struct {
		Response *string `json:"response"`
	}
	if err := s.controlInto(ctx, "side_question", map[string]any{"question": question}, &r); err != nil {
		return "", err
	}
	return providerutil.Deref(r.Response), nil
}

// PermissionRules lists the permission rules in effect.
func (s *Session) PermissionRules(ctx context.Context) (json.RawMessage, error) {
	return s.Control(ctx, "list_permission_rules", nil)
}

// ExportConversation returns the conversation in the CLI's export format.
func (s *Session) ExportConversation(ctx context.Context) (json.RawMessage, error) {
	return s.Control(ctx, "export_conversation", nil)
}

// Status returns the CLI's /status data.
func (s *Session) Status(ctx context.Context) (json.RawMessage, error) {
	return s.Control(ctx, "get_status", nil)
}

// SetPromptSuggestionsPaused pauses or resumes prompt suggestions.
func (s *Session) SetPromptSuggestionsPaused(ctx context.Context, paused bool) error {
	_, err := s.Control(ctx, "set_prompt_suggestions_paused", map[string]any{"paused": paused})
	return err
}

// Compact compacts the conversation with the /compact command and waits
// for it to finish. instructions guide the summary.
func (s *Session) Compact(ctx context.Context, instructions string) error {
	text := strings.TrimSpace("/compact " + instructions)
	_, err := s.Run(ctx, agent.Input{Parts: []agent.Part{agent.TextPart(text)}, ProviderOptions: TurnOptions{}})
	return err
}

// ForkSession starts a new session that continues this conversation while
// leaving this one unchanged. It needs a persisted session (not
// Ephemeral). Empty fields of cfg are taken from this session.
func (s *Session) ForkSession(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	if s.cfg.Ephemeral {
		return nil, providerkit.Unsupported(Name, "forking an ephemeral session")
	}
	cfg = providerkit.InheritConfig(cfg, s.cfg, s.Info())
	if cfg.ForkAtTurn != "" {
		// A turn of this session forks at its latest main assistant
		// message, once that message has reached the transcript.
		if c := s.translator().cursor(cfg.ForkAtTurn); c != "" {
			if g, err := loadTranscript(ctx, s.provider.fileSystem(), s.provider.configDir(s.cfg), s.ID(), s.cfg.Cwd); err == nil {
				if _, ok := g.byID[c]; ok {
					cfg.ForkAtTurn = c
				}
			}
		}
	}
	cfg.Resume = s.ID()
	cfg.Fork = true
	return s.provider.Start(ctx, cfg)
}

// SetCwd moves the session to another working directory.
func (s *Session) SetCwd(ctx context.Context, path string) (json.RawMessage, error) {
	raw, err := s.Control(ctx, "set_cwd", map[string]any{"path": path})
	if err == nil {
		s.updateInfo(func(i *agent.SessionInfo) { i.Cwd = path })
	}
	return raw, err
}
