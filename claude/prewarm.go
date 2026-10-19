package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"

	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Spare is a Claude Code process started ahead of time and parked until a
// session needs it, so the session skips the CLI's start-up (alpha in the
// CLI). Claim binds it to a session; Close discards it. A Spare is single
// use.
type Spare struct {
	s *Session
	// dir is the parking directory Prewarm created, removed after the
	// claim; empty when Config.Cwd was given.
	dir string

	mu      sync.Mutex
	claimed bool
	closed  bool
}

// Prewarm starts a spare process with --await-claim and runs the
// initialize handshake. cfg holds the settings fixed for the process's
// life: the tools, MCP servers, hooks, system prompt, provider options,
// and environment. Resume, Fork, ForkAtTurn, and SessionOptions.Continue
// describe an existing session and are rejected. With an empty Cwd the
// process waits in a new temporary directory, which is removed once the
// spare is claimed or closed. When the claim cannot remove it yet, as on
// Windows while the process works in it, it is removed once the process
// exited.
func (p *Provider) Prewarm(ctx context.Context, cfg agent.Config) (*Spare, error) {
	opts, err := providerkit.SessionOptions[SessionOptions](Name, cfg.ProviderOptions)
	if err != nil {
		return nil, err
	}
	if cfg.Resume != "" || cfg.Fork || cfg.ForkAtTurn != "" || opts.Continue {
		return nil, errors.New("claude: Prewarm cannot resume or fork a session; pass those options to Start")
	}
	dir := ""
	if cfg.Cwd == "" {
		d, err := os.MkdirTemp("", "claude-spare-")
		if err != nil {
			return nil, fmt.Errorf("claude: create spare directory: %w", err)
		}
		dir, cfg.Cwd = d, d
	}
	s, err := p.startSession(ctx, cfg, opts, true)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Spare{s: s, dir: dir}, nil
}

// Done is closed when the spare's process exited, claimed or not. A spare
// that exits before its claim should be replaced.
func (sp *Spare) Done() <-chan struct{} { return sp.s.Done() }

// Err returns why the spare's process exited, or nil while it runs and
// after Close.
func (sp *Spare) Err() error { return sp.s.Err() }

// Claim binds the spare to a session in cfg.Cwd (required) and returns it,
// started: its first event is SessionStarted. The claim carries
// PermissionMode, Env (only variables the CLI accepts at claim time, such
// as fresh tokens), AdditionalDirectories, AppendSystemPrompt, Title, and
// SessionOptions.Agents; Model and Effort are applied right after it.
// Other fields were fixed by Prewarm: set ones are reported in a Notice with
// Code "config_ignored", or refused with Config.Strict; DisallowedTools,
// UnsetEnv, and a read-only permission mode are restrictions the spare's
// process cannot take on, so they are always refused. When the CLI refuses
// the claim, such as for a permission mode it cannot take, the process is
// closed and the error says why; start the session with Start instead. A
// Model or Effort the CLI rejects leaves the session running without it
// and is reported as a Notice with Code "option_not_applied".
func (sp *Spare) Claim(ctx context.Context, cfg agent.Config) (*Session, error) {
	// Invalid arguments leave the spare unclaimed for another try.
	if cfg.Cwd == "" {
		return nil, errors.New("claude: Claim needs Config.Cwd")
	}
	if cfg.Resume != "" || cfg.Fork || cfg.ForkAtTurn != "" {
		return nil, errors.New("claude: a claimed spare cannot resume or fork a session")
	}
	if cfg.PermissionMode == agent.PermissionReadOnly {
		return nil, providerkit.Unsupported(Name, "permission mode readOnly")
	}
	if len(cfg.DisallowedTools) > 0 {
		return nil, providerkit.Unsupported(Name, "DisallowedTools on a claimed spare")
	}
	if len(cfg.UnsetEnv) > 0 {
		return nil, providerkit.Unsupported(Name, "UnsetEnv on a claimed spare")
	}
	opts, err := providerkit.SessionOptions[SessionOptions](Name, cfg.ProviderOptions)
	if err != nil {
		return nil, err
	}
	notice, err := providerkit.IgnoredConfig(Name, cfg, claimIgnored(cfg))
	if err != nil {
		return nil, err
	}
	sp.mu.Lock()
	if sp.claimed || sp.closed {
		sp.mu.Unlock()
		return nil, errors.New("claude: the spare was already claimed or closed")
	}
	sp.claimed = true
	sp.mu.Unlock()
	cwd := providerutil.AbsDir(cfg.Cwd)
	s := sp.s
	req := map[string]any{"cwd": cwd, "include_initialize": true, "sdk_mcp_servers": []string{}}
	if s.tools != nil {
		req["sdk_mcp_servers"] = []string{s.tools.Name()}
	}
	if cfg.PermissionMode != "" {
		req["permission_mode"] = string(cfg.PermissionMode)
	}
	if len(cfg.Env) > 0 {
		req["env"] = cfg.Env
	}
	if len(cfg.AdditionalDirectories) > 0 {
		req["additional_directories"] = cfg.AdditionalDirectories
	}
	if cfg.AppendSystemPrompt != "" {
		req["append_system_prompt"] = cfg.AppendSystemPrompt
	}
	if cfg.Title != "" {
		req["title"] = cfg.Title
	}
	if len(opts.Agents) > 0 {
		req["agents"] = opts.Agents
	}
	var r struct {
		Status     string          `json:"status"`
		Cwd        string          `json:"cwd"`
		SessionID  string          `json:"session_id"`
		Initialize json.RawMessage `json:"initialize"`
	}
	if err := s.controlInto(ctx, "claim_session", req, &r); err != nil {
		sp.discard()
		return nil, fmt.Errorf("claude: claim spare: %w", err)
	}
	if r.Status != "" && r.Status != "ok" {
		sp.discard()
		return nil, fmt.Errorf("claude: claim spare: status %s", r.Status)
	}
	sp.removeDir(s.current())
	if err := s.registerMCPServers(ctx, s.cfg, s.opts); err != nil {
		sp.discard()
		return nil, err
	}

	// Record the claimed settings so a restart (Rewind's fallback)
	// relaunches the same session instead of another spare.
	s.spare = false
	s.cfg.Cwd = cmp.Or(r.Cwd, cwd)
	if cfg.PermissionMode != "" {
		s.cfg.PermissionMode = cfg.PermissionMode
	}
	if len(cfg.AdditionalDirectories) > 0 {
		s.cfg.AdditionalDirectories = cfg.AdditionalDirectories
	}
	s.cfg.Title = cmp.Or(cfg.Title, s.cfg.Title)
	s.cfg.AppendSystemPrompt = cmp.Or(cfg.AppendSystemPrompt, s.cfg.AppendSystemPrompt)
	if len(cfg.Env) > 0 {
		env := maps.Clone(s.cfg.Env)
		if env == nil {
			env = map[string]string{}
		}
		maps.Copy(env, cfg.Env)
		s.cfg.Env = env
	}
	if len(opts.Agents) > 0 {
		s.opts.Agents = opts.Agents
	}
	if r.SessionID != "" {
		s.rt.SetSessionID(r.SessionID)
	}
	var init *InitializeResponse
	if len(r.Initialize) > 0 {
		init, _ = s.decodeInitialize(r.Initialize)
	}
	s.infoMu.Lock()
	if init != nil {
		s.applyInitLocked(init)
	}
	s.info.ID = s.rt.SessionID()
	s.info.Cwd = s.cfg.Cwd
	s.info.PermissionMode = s.cfg.PermissionMode
	s.info.Title = s.cfg.Title
	info := s.info
	s.infoMu.Unlock()
	if notice != nil {
		s.rt.Emit(notice)
	}
	s.rt.Started(&agent.SessionStarted{EventMeta: agent.EventMeta{Raw: r.Initialize}, Info: info})

	if cfg.Model != "" {
		if err := s.SetModel(ctx, cfg.Model); err != nil {
			s.rt.Emit(&agent.Notice{Level: agent.NoticeWarning, Code: "option_not_applied", Message: "model: " + err.Error()})
		}
	}
	if cfg.Effort != "" {
		if err := s.SetEffort(ctx, cfg.Effort); err != nil {
			s.rt.Emit(&agent.Notice{Level: agent.NoticeWarning, Code: "option_not_applied", Message: "effort: " + err.Error()})
		}
	}
	return s, nil
}

// Close discards an unclaimed spare and waits for its process to exit. It
// does nothing after a claim; close the claimed Session instead.
func (sp *Spare) Close() error {
	sp.mu.Lock()
	if sp.claimed || sp.closed {
		sp.mu.Unlock()
		return nil
	}
	sp.closed = true
	sp.mu.Unlock()
	sp.discard()
	return nil
}

func (sp *Spare) discard() {
	sp.s.shutdown()
	_ = os.RemoveAll(sp.dir)
}

// removeDir removes the parking directory of a claimed spare whose process
// p still runs. Windows refuses to remove a directory that a running
// process has as its working directory, so a failed removal is retried
// once p exited.
func (sp *Spare) removeDir(p *cliProcess) {
	if sp.dir == "" || os.RemoveAll(sp.dir) == nil || p == nil {
		return
	}
	go func() {
		<-p.done
		_ = os.RemoveAll(sp.dir)
	}()
}

// claimIgnored names the fields of a Claim config that Prewarm fixed, so
// the claim cannot change them.
func claimIgnored(cfg agent.Config) []string {
	var ignored []string
	add := func(set bool, name string) {
		if set {
			ignored = append(ignored, name)
		}
	}
	add(cfg.SystemPrompt != "", "SystemPrompt")
	add(len(cfg.OutputSchema) > 0, "OutputSchema")
	add(cfg.SessionID != "", "SessionID")
	add(cfg.Ephemeral, "Ephemeral")
	add(len(cfg.MCPServers) > 0, "MCPServers")
	add(len(cfg.Tools) > 0, "Tools")
	add(len(cfg.AllowedTools) > 0, "AllowedTools")
	h := cfg.Handlers
	add(h.Permission != nil || h.UserInput != nil || h.Elicitation != nil, "Handlers")
	return ignored
}
