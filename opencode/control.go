package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/providerkit"
)

func (s *Session) path(suffix string) string {
	return sessionPath(s.ID(), suffix)
}

// sessionPath returns the route of session id followed by suffix.
func sessionPath(id, suffix string) string {
	return "/api/session/" + url.PathEscape(id) + suffix
}

// SetTitle renames the session.
func (s *Session) SetTitle(ctx context.Context, title string) error {
	if err := s.Call(ctx, "PATCH", s.path(""), map[string]any{"title": title}, nil); err != nil {
		return err
	}
	info := s.setInfo(func(i *agent.SessionInfo) { i.Title = title })
	s.rt.Emit(&agent.SessionUpdated{Info: info})
	return nil
}

// Compact compacts the conversation as a turn of its own and waits for it.
// OpenCode takes no instructions for compaction, so instructions is
// ignored.
func (s *Session) Compact(ctx context.Context, instructions string) error {
	_, err := s.Run(ctx, agent.Text("/compact"))
	if errors.Is(err, agent.ErrInterrupted) {
		return nil
	}
	return err
}

// ForkSession copies the conversation into a new session; cfg.ForkAtTurn
// keeps it only through that turn. The fork refuses to start while a turn
// runs, because it would copy a half-finished execution.
// Empty fields of cfg are taken from this session.
func (s *Session) ForkSession(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	if !s.rt.Idle() {
		return nil, errors.New("opencode: cannot fork while a turn runs")
	}
	cfg = providerkit.InheritConfig(cfg, s.cfg, s.Info())
	cfg.Resume, cfg.Fork = s.ID(), true
	if cfg.ForkAtTurn != "" {
		cfg.ForkAtTurn = s.turnMessage(cfg.ForkAtTurn)
	}
	return s.provider.StartSession(ctx, cfg)
}

// turnMessage returns the user message that started turn id, or id itself
// when it names a message.
func (s *Session) turnMessage(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg := s.tr.turnMsg[id]; msg != "" {
		return msg
	}
	return id
}

// forkBoundary returns the message to fork before so that the fork keeps
// sid's conversation through turn: the message that starts the next turn
// (see messageTurns), or "" when the turn is the last one. Steers
// delivered into the turn stay.
func (s *Session) forkBoundary(ctx context.Context, sid, turn string) (string, error) {
	msgs, err := allMessages(ctx, s.c, sid)
	if err != nil {
		return "", err
	}
	turnOf := messageTurns(msgs)
	idx := slices.IndexFunc(msgs, func(raw json.RawMessage) bool { return requestID(raw) == turn })
	if idx < 0 {
		return "", fmt.Errorf("opencode: turn %s is not in session %s", turn, sid)
	}
	for j := idx + 1; j < len(msgs); j++ {
		if turnOf[j] != turnOf[idx] && requestID(msgs[j]) == turnOf[j] {
			return turnOf[j], nil
		}
	}
	return "", nil
}

// Rewind removes turnID and every later turn from the conversation in
// place; files are not restored and the session ID stays. turnID is a
// TurnResult.TurnID of this session or a message ID from its history.
func (s *Session) Rewind(ctx context.Context, turnID string) error {
	if !s.rt.Idle() {
		return errors.New("opencode: cannot rewind while a turn runs")
	}
	msg := s.turnMessage(turnID)
	if err := s.Call(ctx, "POST", s.path("/revert/stage"), map[string]any{"messageID": msg, "files": false}, nil); err != nil {
		return fmt.Errorf("opencode: rewind to %s: %w", turnID, err)
	}
	if err := s.Call(ctx, "POST", s.path("/revert/commit"), nil, nil); err != nil {
		// OpenCode commits a staged revert with the next prompt; clear it.
		// The clear runs an empty execution, which is no turn.
		s.mu.Lock()
		s.tr.skipExec = true
		s.mu.Unlock()
		_ = s.Call(ctx, "DELETE", s.path("/revert"), nil, nil)
		return fmt.Errorf("opencode: rewind to %s: %w", turnID, err)
	}
	return nil
}

// StopTask stops a subagent: taskID is the subagent's session ID. A
// subagent is known from its start until the turn that ran it ends, or
// until it finishes when it runs in the background; StopTask on one this
// session no longer knows, such as a finished subagent, returns an
// "unknown task" error.
func (s *Session) StopTask(ctx context.Context, taskID string) error {
	s.mu.Lock()
	_, known := s.tr.children[taskID]
	s.mu.Unlock()
	if !known {
		return fmt.Errorf("opencode: unknown task %s", taskID)
	}
	return s.Call(ctx, "POST", sessionPath(taskID, "/interrupt"), nil, nil)
}

// AgentInfo describes an OpenCode agent.
type AgentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Mode is "primary", "subagent", or "all".
	Mode        string           `json:"mode"`
	Hidden      bool             `json:"hidden"`
	Permissions []PermissionRule `json:"permissions"`
	// Raw is the native agent without its request settings.
	Raw json.RawMessage `json:"-"`
}

// Agents lists the agents of the session's directory.
func (s *Session) Agents(ctx context.Context) ([]AgentInfo, error) {
	agents, raws, err := s.listAgents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		agents[i].Raw = publicAgent(raws[i])
	}
	return agents, nil
}

// listAgents lists the agents without Raw, along with each agent's native
// JSON at the same index.
func (s *Session) listAgents(ctx context.Context) ([]AgentInfo, []json.RawMessage, error) {
	var list []json.RawMessage
	if err := s.data(ctx, "GET", withQuery("/api/agent", locationQuery(s.cwd)), nil, &list); err != nil {
		return nil, nil, err
	}
	agents := make([]AgentInfo, 0, len(list))
	raws := make([]json.RawMessage, 0, len(list))
	for _, raw := range list {
		var a AgentInfo
		if json.Unmarshal(raw, &a) == nil {
			agents = append(agents, a)
			raws = append(raws, raw)
		}
	}
	return agents, raws, nil
}

// publicAgent returns an agent's JSON without its request settings, which
// can carry provider credentials.
func publicAgent(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	delete(m, "request")
	out, _ := json.Marshal(m)
	return out
}

// SetAgent switches the session's primary agent, such as "build", "plan",
// or a custom agent. Switching to "plan" is plan mode.
func (s *Session) SetAgent(ctx context.Context, name string) error {
	if s.rt.Finished() {
		return s.rt.ClosedErr()
	}
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	s.mu.Lock()
	previous, previousAgent, mode := s.info.PermissionMode, s.agentName, s.info.PermissionMode
	switch {
	case name == "plan":
		mode = agent.PermissionPlan
	case mode == agent.PermissionPlan:
		// Leaving plan restores the mode before it.
		mode = s.lastMode
	}
	s.mu.Unlock()
	if err := s.switchAgent(ctx, name); err != nil {
		return err
	}
	// The mode's rules are written with the agent, so the reported mode
	// is always the one the rules enforce.
	s.setInfo(func(i *agent.SessionInfo) { i.PermissionMode = mode })
	if err := s.writeRulesLocked(ctx, mode); err != nil {
		s.setInfo(func(i *agent.SessionInfo) { i.PermissionMode = previous })
		back := context.WithoutCancel(ctx)
		_ = s.switchAgent(back, previousAgent)
		// A write that failed may have been applied.
		_ = s.writeRulesLocked(back, previous)
		return err
	}
	s.rt.Emit(&agent.SessionUpdated{Info: s.Info()})
	return nil
}

// PermissionRules returns the session's permission rules.
func (s *Session) PermissionRules(ctx context.Context) ([]PermissionRule, error) {
	var native nativeSession
	if err := s.data(ctx, "GET", s.path(""), nil, &native); err != nil {
		return nil, err
	}
	return native.Permissions, nil
}

// SetPermissionRules replaces the session's permission rules. Every later
// rule write replaces them again: permission mode changes, SetAgent,
// AllowSession answers, a config reload (agent.updated), an agent switch
// by another client, a reconnection of the event stream, and, for a
// session that runs the server's default agent, a change of that agent
// noticed before an input.
func (s *Session) SetPermissionRules(ctx context.Context, rules []PermissionRule) error {
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	return s.putRulesLocked(ctx, rules)
}

// SetInstructions sets the session instruction entry key, which OpenCode
// appends to the system prompt. An empty value removes the entry.
func (s *Session) SetInstructions(ctx context.Context, key, value string) error {
	path := "/api/experimental/session/" + url.PathEscape(s.ID()) + "/instructions/entries/" + url.PathEscape(key)
	if value == "" {
		err := s.Call(ctx, "DELETE", path, nil, nil)
		if httpStatus(err) == 404 {
			return nil
		}
		return err
	}
	return s.Call(ctx, "PUT", path, map[string]any{"value": value}, nil)
}

// Inbox lists input OpenCode has queued but not delivered yet.
func (s *Session) Inbox(ctx context.Context) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := s.data(ctx, "GET", s.path("/inbox"), nil, &out)
	return out, err
}

// CancelInbox takes back a queued input by its inbox ID.
func (s *Session) CancelInbox(ctx context.Context, inboxID string) error {
	return s.Call(ctx, "DELETE", s.path("/inbox/"+url.PathEscape(inboxID)), nil, nil)
}

// Shell runs a shell command in the session, as "!command" does in
// OpenCode's terminal UI. Its output is recorded in the conversation.
func (s *Session) Shell(ctx context.Context, command string) error {
	return s.Call(ctx, "POST", s.path("/shell"), map[string]any{"command": command}, nil)
}

// Synthetic adds a message to the conversation on the user's behalf
// without starting a turn of this library's; OpenCode delivers it like
// steered input.
func (s *Session) Synthetic(ctx context.Context, text, description string) error {
	body := map[string]any{"text": text}
	if description != "" {
		body["description"] = description
	}
	return s.Call(ctx, "POST", s.path("/synthetic"), body, nil)
}

// SideQuestion asks the session's model a side question with the conversation
// as context; the conversation is not changed.
func (s *Session) SideQuestion(ctx context.Context, question string) (string, error) {
	var out struct {
		Text string `json:"text"`
	}
	err := s.data(ctx, "POST", s.path("/generate"), map[string]any{"prompt": question}, &out)
	return out.Text, err
}

// Diff returns the file changes the session made, as OpenCode's
// FileDiffInfo list.
func (s *Session) Diff(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := s.data(ctx, "GET", s.path("/diff"), nil, &out)
	return out, err
}

// Export returns the session in OpenCode's transfer format.
func (s *Session) Export(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := s.data(ctx, "GET", "/api/experimental/session/"+url.PathEscape(s.ID())+"/export", nil, &out)
	return out, err
}

// Messages returns the session's native messages, oldest first.
func (s *Session) Messages(ctx context.Context) ([]json.RawMessage, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	return allMessages(ctx, s.c, s.ID())
}
