package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// ProviderSpec tells agentd how to build a provider and decode its
// options.
type ProviderSpec struct {
	Name string
	// New builds the provider from its Options JSON, which may be empty.
	New func(options json.RawMessage, b ProviderBridge) (agent.Provider, error)
	// SessionOptions decodes the provider's SessionOptions JSON into the
	// value Config.ProviderOptions takes. Nil means the provider has none.
	SessionOptions func(options json.RawMessage, b SessionBridge) (any, error)
	// TurnOptions decodes the provider's TurnOptions JSON into the value
	// Input.ProviderOptions takes. Nil means the provider has none.
	TurnOptions func(options json.RawMessage) (any, error)
	// Options, SessionOptionsType, and TurnOptionsType are zero values of
	// the option types, for the protocol schema; nil when absent.
	Options, SessionOptionsType, TurnOptionsType any
}

// ProviderBridge reaches the client for provider-level callbacks.
type ProviderBridge struct {
	Init ProviderInit
	// Call sends a reverse request to the client and returns its result.
	Call func(ctx context.Context, method string, params any) (json.RawMessage, error)
	// WireTap sends a wire/tap notification for provider-level traffic.
	WireTap func(dir agent.WireDirection, line []byte)
	// Logger receives provider-level diagnostics; nil discards them.
	Logger *slog.Logger
	// Spawner starts the harness processes; nil means locally. A custom
	// ProviderSpec.New must apply it itself.
	Spawner agent.Spawner
}

// SessionBridge reaches the client for session-level callbacks.
type SessionBridge struct {
	Start StartParams
	// Call sends a reverse request to the client for this session.
	Call func(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Session is the session's handle.
	Session string
}

// Defaults returns the four harness providers.
func Defaults() []ProviderSpec {
	return []ProviderSpec{
		{
			Name: claude.Name,
			New:  plainProvider(claude.Name, claude.New, func(o *claude.Options) *agent.Spawner { return &o.Spawner }),
			SessionOptions: func(raw json.RawMessage, b SessionBridge) (any, error) {
				opts, err := decodeOptions[claude.SessionOptions](raw)
				if err != nil {
					return nil, err
				}
				bindClaudeSession(&opts, b)
				return &opts, nil
			},
			TurnOptions:        decodeOptionalOptions[claude.TurnOptions],
			Options:            claude.Options{},
			SessionOptionsType: claude.SessionOptions{},
			TurnOptionsType:    claude.TurnOptions{},
		},
		{
			Name: codex.Name,
			New: func(raw json.RawMessage, b ProviderBridge) (agent.Provider, error) {
				opts, err := decodeOptions[codex.Options](raw)
				if err != nil {
					return nil, err
				}
				bindCodexProvider(&opts, b)
				if b.Spawner != nil {
					opts.Spawner = b.Spawner
				}
				return codex.New(opts), nil
			},
			SessionOptions:     sessionOptions[codex.SessionOptions](codex.Name),
			TurnOptions:        decodeOptionalOptions[codex.TurnOptions],
			Options:            codex.Options{},
			SessionOptionsType: codex.SessionOptions{},
			TurnOptionsType:    codex.TurnOptions{},
		},
		{
			Name:               pi.Name,
			New:                plainProvider(pi.Name, pi.New, func(o *pi.Options) *agent.Spawner { return &o.Spawner }),
			SessionOptions:     sessionOptions[pi.SessionOptions](pi.Name),
			Options:            pi.Options{},
			SessionOptionsType: pi.SessionOptions{},
		},
		{
			Name:               opencode.Name,
			New:                plainProvider(opencode.Name, opencode.New, func(o *opencode.Options) *agent.Spawner { return &o.Spawner }),
			SessionOptions:     sessionOptions[opencode.SessionOptions](opencode.Name),
			TurnOptions:        decodeOptionalOptions[opencode.TurnOptions],
			Options:            opencode.Options{},
			SessionOptionsType: opencode.SessionOptions{},
			TurnOptionsType:    opencode.TurnOptions{},
		},
	}
}

// bindClaudeSession forwards the Claude session callbacks the client
// asked for.
func bindClaudeSession(opts *claude.SessionOptions, b SessionBridge) {
	// The closures keep only what they use, not the whole start params.
	call, session := b.Call, b.Session
	if b.Start.OnUserDialog {
		opts.OnUserDialog = func(ctx context.Context, dialog claude.UserDialog) (json.RawMessage, error) {
			return call(ctx, "claude/userDialog", UserDialogCall{Session: session, Dialog: dialog})
		}
	}
	if b.Start.OnControlRequest {
		opts.OnControlRequest = func(ctx context.Context, subtype string, req json.RawMessage) (any, error) {
			return call(ctx, "claude/controlRequest", ControlRequestCall{Session: session, Subtype: subtype, Request: req})
		}
	}
	for event, matchers := range opts.Hooks {
		for i := range matchers {
			matcher := matchers[i].Matcher
			matchers[i].Hooks = []claude.HookFunc{func(ctx context.Context, in claude.HookInput) (claude.HookOutput, error) {
				var out claude.HookOutput
				raw, err := call(ctx, "claude/hook", HookCall{Session: session, Event: event, Matcher: matcher, Index: i,
					Payload: in.Raw, ToolUseID: in.ToolUseIDArg})
				if err != nil || json.Unmarshal(raw, &out) != nil {
					// No answer is no decision: the CLI goes on as if the
					// hook were absent.
					return claude.HookOutput{}, nil
				}
				return out, nil
			}}
		}
	}
}

// bindCodexProvider forwards the Codex provider callbacks the client asked
// for.
func bindCodexProvider(opts *codex.Options, b ProviderBridge) {
	if b.Init.OnAuthTokensRefresh {
		opts.OnAuthTokensRefresh = func(ctx context.Context, params json.RawMessage) (any, error) {
			return b.Call(ctx, "codex/authTokensRefresh", AuthTokensRefreshCall{Provider: codex.Name, Params: params})
		}
	}
	if b.WireTap != nil {
		opts.WireTap = b.WireTap
	}
	if b.Logger != nil {
		opts.Logger = b.Logger.With("provider", codex.Name)
	}
}

// refuseProviderCallbacks rejects provider callbacks only Codex offers.
func refuseProviderCallbacks(name string, b ProviderBridge) error {
	if b.Init.OnAuthTokensRefresh || b.Init.WireTap {
		return newError("invalid_params", name+": OnAuthTokensRefresh and provider WireTap are Codex options; use StartParams.WireTap for a session's traffic")
	}
	return nil
}

// refuseSessionCallbacks rejects session callbacks only Claude offers.
func refuseSessionCallbacks(name string, start StartParams) error {
	if start.OnUserDialog || start.OnControlRequest {
		return newError("invalid_params", name+": OnUserDialog and OnControlRequest are Claude options")
	}
	return nil
}

// decodeOptions decodes an options object. Empty input is the zero value.
func decodeOptions[T any](raw json.RawMessage) (T, error) {
	opts, err := decodeStrict[T](raw)
	if err != nil {
		return opts, newError("invalid_params", fmt.Sprintf("options: %v", err))
	}
	return opts, nil
}

// decodeStrict decodes JSON, refusing unknown fields so a misspelled name
// fails instead of being ignored, and values only this process can supply.
// Empty input and null are the zero value.
func decodeStrict[T any](raw json.RawMessage) (T, error) {
	var v T
	if isEmpty(raw) {
		return v, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	return v, rejectLocalOnly(reflect.ValueOf(&v).Elem(), reflect.TypeFor[T]().Name())
}

// localOnly are field types JSON can fill but that only make sense as
// values from this process: "Logger":{} decodes into a logger without a
// handler, which panics when used.
var localOnly = map[reflect.Type]bool{
	reflect.TypeFor[*slog.Logger](): true,
	reflect.TypeFor[*http.Client](): true,
}

// rejectLocalOnly fails when a localOnly field of v, at any depth of
// nested structs, was set.
func rejectLocalOnly(v reflect.Value, path string) error {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() || localOnly[v.Type()] {
			break
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if localOnly[f.Type] {
			if !fv.IsNil() {
				return fmt.Errorf("%s.%s cannot be set over the wire", path, f.Name)
			}
			continue
		}
		if err := rejectLocalOnly(fv, path+"."+f.Name); err != nil {
			return err
		}
	}
	return nil
}

func isEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}

// plainProvider returns a ProviderSpec.New for a provider without
// provider-level callbacks. spawner returns the Options' Spawner field.
func plainProvider[O any, P agent.Provider](name string, build func(O) P, spawner func(*O) *agent.Spawner) func(json.RawMessage, ProviderBridge) (agent.Provider, error) {
	return func(raw json.RawMessage, b ProviderBridge) (agent.Provider, error) {
		if err := refuseProviderCallbacks(name, b); err != nil {
			return nil, err
		}
		opts, err := decodeOptions[O](raw)
		if err != nil {
			return nil, err
		}
		if b.Spawner != nil {
			*spawner(&opts) = b.Spawner
		}
		return build(opts), nil
	}
}

// sessionOptions returns a ProviderSpec.SessionOptions for a provider
// without session callbacks, which only Claude has.
func sessionOptions[T any](name string) func(json.RawMessage, SessionBridge) (any, error) {
	return func(raw json.RawMessage, b SessionBridge) (any, error) {
		if err := refuseSessionCallbacks(name, b.Start); err != nil {
			return nil, err
		}
		return decodeOptionalOptions[T](raw)
	}
}

// decodeOptionalOptions decodes options into a *T, or returns nil when raw
// is empty, as Config.ProviderOptions and Input.ProviderOptions take them.
func decodeOptionalOptions[T any](raw json.RawMessage) (any, error) {
	if isEmpty(raw) {
		return nil, nil
	}
	opts, err := decodeOptions[T](raw)
	if err != nil {
		return nil, err
	}
	return &opts, nil
}
