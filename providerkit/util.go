package providerkit

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// ReadPart returns the bytes and media type of an image or file part,
// reading Path when Data is empty.
func ReadPart(data []byte, mediaType, path string) ([]byte, string, error) {
	if len(data) == 0 && path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		data = b
	}
	if mediaType == "" && path != "" {
		mediaType = providerutil.MediaType(path)
	}
	return data, mediaType, nil
}

// InheritConfig fills the empty fields of cfg for a session that continues
// another one, such as a fork: the model, effort, permission mode, and
// working directory from live (the running session's current settings),
// else from base, and every other setting from base. Resume, Fork,
// ForkAtTurn, SessionID, and Title are never inherited. Handlers are
// inherited only when cfg sets none.
func InheritConfig(cfg, base agent.Config, live agent.SessionInfo) agent.Config {
	cfg.Cwd = cmp.Or(cfg.Cwd, base.Cwd, live.Cwd)
	cfg.Model = cmp.Or(cfg.Model, live.Model, base.Model)
	cfg.Effort = cmp.Or(cfg.Effort, live.Effort, base.Effort)
	cfg.PermissionMode = cmp.Or(cfg.PermissionMode, live.PermissionMode, base.PermissionMode)
	cfg.SystemPrompt = cmp.Or(cfg.SystemPrompt, base.SystemPrompt)
	cfg.AppendSystemPrompt = cmp.Or(cfg.AppendSystemPrompt, base.AppendSystemPrompt)
	if cfg.OutputSchema == nil {
		cfg.OutputSchema = base.OutputSchema
	}
	cfg.Ephemeral = cfg.Ephemeral || base.Ephemeral
	if cfg.MCPServers == nil {
		cfg.MCPServers = base.MCPServers
	}
	if cfg.Tools == nil {
		cfg.Tools = base.Tools
	}
	if cfg.AllowedTools == nil {
		cfg.AllowedTools = base.AllowedTools
	}
	if cfg.DisallowedTools == nil {
		cfg.DisallowedTools = base.DisallowedTools
	}
	if cfg.AdditionalDirectories == nil {
		cfg.AdditionalDirectories = base.AdditionalDirectories
	}
	if cfg.Env == nil {
		cfg.Env = base.Env
	}
	if cfg.UnsetEnv == nil {
		cfg.UnsetEnv = base.UnsetEnv
	}
	if h := cfg.Handlers; h.Permission == nil && h.UserInput == nil && h.Elicitation == nil {
		cfg.Handlers = base.Handlers
	}
	if cfg.Logger == nil {
		cfg.Logger = base.Logger
	}
	if cfg.WireTap == nil {
		cfg.WireTap = base.WireTap
	}
	if cfg.EventBufferLimit == 0 {
		cfg.EventBufferLimit = base.EventBufferLimit
	}
	if cfg.ProviderOptions == nil {
		cfg.ProviderOptions = base.ProviderOptions
	}
	return cfg
}

// SessionOptions returns the provider's session options from
// Config.ProviderOptions, which may hold T, *T, or nil. Any other type is
// an error, so options meant for another provider are not silently lost.
func SessionOptions[T any](provider string, v any) (T, error) {
	return options[T](provider, "Config", v)
}

// TurnOptions returns the provider's turn options from
// Input.ProviderOptions, with the same rules as SessionOptions.
func TurnOptions[T any](provider string, v any) (T, error) {
	return options[T](provider, "Input", v)
}

func options[T any](provider, holder string, v any) (T, error) {
	var zero T
	switch o := v.(type) {
	case nil:
		return zero, nil
	case *T:
		if o == nil {
			return zero, nil
		}
		return *o, nil
	case T:
		return o, nil
	}
	return zero, fmt.Errorf("%s: %s.ProviderOptions holds %T; want *%T", provider, holder, v, zero)
}

// IgnoredConfig handles the Config fields a provider cannot honor, named in
// ignored: with cfg.Strict it returns an error wrapping
// agent.ErrUnsupported; otherwise it returns the Notice to emit, or nil
// when nothing is ignored. Emit the notice before Started and it follows
// SessionStarted.
func IgnoredConfig(provider string, cfg agent.Config, ignored []string) (*agent.Notice, error) {
	if len(ignored) == 0 {
		return nil, nil
	}
	list := strings.Join(ignored, ", ")
	if cfg.Strict {
		return nil, fmt.Errorf("%w (Config.Strict is set)", Unsupported(provider, list))
	}
	return &agent.Notice{Level: agent.NoticeWarning, Code: "config_ignored", Message: provider + " ignores " + list}, nil
}

// CheckPermissionMode returns nil for an empty mode, which selects the
// harness default, or a mode listed in supported; otherwise it returns an
// *agent.UnsupportedError.
func CheckPermissionMode(provider string, mode agent.PermissionMode, supported []agent.PermissionMode) error {
	if mode == "" || slices.Contains(supported, mode) {
		return nil
	}
	return Unsupported(provider, "permission mode "+string(mode))
}

// CheckInput refuses input with an unknown Delivery or Part.Type, so a
// misspelled value is not silently treated as a default.
func CheckInput(provider string, in agent.Input) error {
	switch in.Delivery {
	case "", agent.DeliverQueue, agent.DeliverSteer, agent.DeliverInterrupt:
	default:
		return fmt.Errorf("%s: unknown Input.Delivery %q", provider, in.Delivery)
	}
	for i, p := range in.Parts {
		switch p.Type {
		case agent.PartText, agent.PartImage, agent.PartFile, agent.PartSkill, agent.PartMention:
		default:
			return fmt.Errorf("%s: Input.Parts[%d] has unknown type %q", provider, i, p.Type)
		}
	}
	return nil
}

// CheckUpdatedInput refuses a permission answer that allows the call with
// PermissionResponse.UpdatedInput, for harnesses that cannot replace a
// call's arguments: running the original call instead would ignore what
// the host asked for.
func CheckUpdatedInput(provider string, resp agent.PermissionResponse) error {
	if len(resp.UpdatedInput) == 0 || resp.ProviderResponse != nil {
		return nil
	}
	switch resp.Decision {
	case agent.DecisionDeny, agent.DecisionCancel:
		return nil
	}
	return Unsupported(provider, "PermissionResponse.UpdatedInput")
}
