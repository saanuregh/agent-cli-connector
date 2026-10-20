package claude

import (
	"context"
	"os"
	"runtime"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

// managedMCPConfig is the policy file next to which the CLI refuses
// --strict-mcp-config.
var managedMCPConfig = managedMCPConfigPath(runtime.GOOS)

// managedMCPConfigPath returns where the CLI looks for managed-mcp.json
// on goos, as https://code.claude.com/docs/en/managed-mcp lists.
func managedMCPConfigPath(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-mcp.json"
	case "windows":
		return `C:\Program Files\ClaudeCode\managed-mcp.json`
	default:
		return "/etc/claude-code/managed-mcp.json"
	}
}

// Check reports the CLI version from "claude --version" and the sign-in
// state from the account in a short-lived initialize handshake that loads
// no MCP servers, hooks, or tools. A missing executable gives Installed
// false and Err set, not an error. ctx bounds the whole check.
func (p *Provider) Check(ctx context.Context) (*agent.ProviderStatus, error) {
	st, err := providerkit.VersionStatus(ctx, p.opts.Spawner, p.path(), providerkit.EnvList(p.opts.Env))
	if err != nil || !st.Installed || st.Err != nil {
		return st, err
	}
	s, cleanup, err := p.isolatedSession(ctx, []string{})
	if err != nil {
		return providerkit.CheckFailed(ctx, st, err)
	}
	defer cleanup()
	init := s.Initialization()
	if st.Version == "" {
		st.Version = init.Version
	}
	st.Auth = authStatus(init)
	return st, nil
}

// authStatus derives the sign-in state from the initialize response.
func authStatus(init *InitializeResponse) *agent.AuthStatus {
	a := init.Account
	raw := providerutil.Raw(a)
	st := &agent.AuthStatus{Email: a.Email, Plan: a.SubscriptionType, Raw: raw}
	switch {
	case a.APIProvider != "" && a.APIProvider != "firstParty":
		st.Method = a.APIProvider
	case a.APIKeySource != "" && a.APIKeySource != "none":
		st.Method = "apiKey"
	case a.TokenSource != "" && a.TokenSource != "none" || a.Email != "" || a.SubscriptionType != "":
		st.Method = "oauth"
	}
	st.Authenticated = st.Method != ""
	return st
}

// isolatedSession starts a short-lived session in a temporary directory
// that loads only the given setting sources and no MCP servers, hooks, or
// tools. cleanup closes it and removes the directory.
func (p *Provider) isolatedSession(ctx context.Context, settingSources []string) (*Session, func(), error) {
	dir, err := os.MkdirTemp("", "agent-claude-")
	if err != nil {
		return nil, nil, err
	}
	_, noManagedMCP := p.fileSystem().Stat(managedMCPConfig)
	s, err := p.StartSession(ctx, agent.Config{
		Cwd:       dir,
		Ephemeral: true,
		Env:       map[string]string{"ENABLE_CLAUDEAI_MCP_SERVERS": "false", "CLAUDE_CODE_AUTO_CONNECT_IDE": "0"},
		ProviderOptions: &SessionOptions{
			SettingSources:  settingSources,
			Settings:        map[string]any{"disableAllHooks": true},
			BuiltinTools:    []string{},
			StrictMCPConfig: noManagedMCP != nil,

			skipAppliedEffort: true,
		},
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	return s, func() {
		s.Close()
		os.RemoveAll(dir)
	}, nil
}
