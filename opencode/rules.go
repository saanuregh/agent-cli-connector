package opencode

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// Permission rules
//
// How OpenCode 2.0.32 decides. A request names an action and one or more
// resources. OpenCode lists the agent's rules (GET /api/agent, which
// includes the config's "permissions"; an agent it does not list has
// unknownAgentRules), then the session's rules. For each resource the
// last rule whose action and resource globs match decides; no match means
// ask. The request is denied when any resource is denied. Otherwise the
// project's saved "always" approvals are added after the session's rules,
// and the request asks when any resource still asks. A saved approval
// therefore outranks every agent and session ask, but never a deny; the
// rules below do not change that. globMatch is OpenCode's glob matcher.
//
// What this library writes. buildRules computes a session's rules for the
// active agent in this order, so that later layers outrank earlier ones:
//
//  1. Base: the mode's rules. default asks before shell, edit, and
//     external_directory; acceptEdits, plan, and readOnly ask before shell
//     and external_directory; bypassPermissions allows everything. An
//     empty mode keeps the rules the session had when it was opened and
//     skips layer 2.
//  2. Agent denies: the agent's rules from its first deny on, each rule
//     other than a deny followed by the base rules narrowed to it. Every
//     request the agent and config deny stays denied, and every other
//     request gets the same answer as with the base alone. So no mode
//     lifts an agent or config deny: bypassPermissions allows everything
//     the agent and config do not deny.
//  3. AllowedTools and AdditionalDirectories: allows.
//  4. This session's tool rules: MCP EnabledTools (deny "<server>_*", then
//     allow the listed tools) and ApprovedTools (allow), the deny of
//     other sessions' host tools, this session's host tool allow, and the
//     asks of host tools with NeedsApproval.
//  5. Mode denies: plan denies edits, except, for the plan agent, its own
//     plan paths that no later rule of the agent denies; readOnly denies
//     edits.
//  6. DisallowedTools: denies.
//  7. SessionOptions.Permissions: the caller's last word.
//
// A tool's action is "<server>_<tool>", so "<server>_*" would also match
// the tools of a server named "<server>_<more>". A session refuses such a
// pair of server names when the shorter one has EnabledTools or
// ApprovedTools, among its own servers and those the directory configures.
//
// Layers 3 to 7 outrank agent denies: 3, 4, 6, and 7 are the caller's
// explicit settings, and 5 only adds denies and the plan agent's own
// allows.
//
// AllowSession grants. A grant answers one ask, for one resource. Each
// time the rules are built, it is placed against the rule that asks for
// that resource now, its answering rule, found across the agent's and the
// session's rules (none matching is OpenCode's own ask):
//
//   - When the resource is no longer asked about, such as under a later
//     mode's deny or allow, the grant writes nothing.
//   - The grant allows the first of OpenCode's save patterns for the
//     resource that matches it and provably matches nothing the answering
//     rule does not (globCovers). Otherwise it allows the asked resource
//     itself; if that contains "*" or "?" and is not "*" itself, which as
//     a pattern would match more than was asked, the grant writes nothing
//     and the request is allowed once.
//   - When the answering rule is a session rule, the grant goes right after
//     it. Otherwise the grant goes first in the session's rules, followed
//     by the agent's rules after the answering rule that match the same
//     action, so they keep deciding what they decided.
//
// What a grant can do: allow the requests that its pattern matches and
// that its answering rule decided. What it cannot do: allow a request that
// is denied, change a request another rule decides, including another
// rule's ask, or allow anything outside its answering rule.
// TestRulesInvariants checks this against a port of OpenCode's
// evaluation, and TestLiveRulesInvariants against OpenCode itself.
//
// Agents. The rules are built for the agent the session runs, which
// this library follows. A session opened without an agent runs the
// server's default agent (default_agent, else build), which OpenCode picks
// each time it runs; the rules follow it before each input and on every
// rule write. SetAgent, plan mode, and session.agent.selected events pin
// an agent, because OpenCode cannot return a session to the default; an
// agent switch reported by an event gets the current mode's rules for the
// new agent. The agents' rules are read again before each rule write,
// because OpenCode reloads its config and agents while it runs, and an
// agent.updated event for the session's directory, which such a reload
// sends, writes the rules of the session and its subagents again. An
// agent selected while a write is in flight gets its rules when the write
// ends. After the event stream reconnects, events may have been lost, so
// the session's agent is read again, unless an agent selection overtakes
// that read, and every rule is written again. Background rewrites are
// coalesced: one waiting to start covers every later request. Rule
// writes stop when the session closes; Close waits for those in flight.
//
// Subagent sessions. OpenCode copies the parent's rules into a subagent
// session it creates and keeps them apart afterwards. This library starts
// from the rules the session.created event reports, follows the
// subagent's agent through its events and subagent calls, and writes its
// rules for that agent, with the parent's grants followed by the
// subagent's: when it first sees the session, when the agent changes, and
// on every later rule change of the parent. A failed subagent write is
// logged and repeated on the next write; it does not fail the parent's
// change. Until the first write, which follows the creation event, the
// subagent has the rules OpenCode copied; OpenCode offers no way to set
// them at creation.
//
// AllowAlways is OpenCode's "always" reply: OpenCode saves the request's
// save patterns for the project, which then outrank every session ask, as
// above. It writes no session grant, which could only repeat what the
// saved approval allows. A request without save patterns saves nothing,
// so "always" then allows it once.
//
// Exact duplicate rules are dropped, keeping the last occurrence, which
// does not change any evaluation.

// unknownAgentRules are the rules OpenCode uses for an agent it does not
// list.
var unknownAgentRules = []PermissionRule{rule("*", "deny")}

// permissionGrant is an AllowSession answer for one resource: allow
// Action on Asked, or on the first of Patterns, OpenCode's save patterns
// for it, that the rules model accepts.
type permissionGrant struct {
	Action   string
	Asked    string
	Patterns []string
}

func (g permissionGrant) equal(o permissionGrant) bool {
	return g.Action == o.Action && g.Asked == o.Asked && slices.Equal(g.Patterns, o.Patterns)
}

// newGrants returns the grants of an AllowSession answer for a request
// about asked, with OpenCode's save patterns. save pairs with asked by
// index when the lengths match; otherwise every save pattern is offered
// for every resource.
func newGrants(action string, save, asked []string) []permissionGrant {
	out := make([]permissionGrant, 0, len(asked))
	for i, res := range asked {
		patterns := save
		if len(save) == len(asked) {
			patterns = save[i : i+1]
		}
		out = append(out, permissionGrant{Action: action, Asked: res, Patterns: slices.Clone(patterns)})
	}
	return out
}

// buildRules returns the session rules for mode, the active agent, and
// grants; see the model above. It returns nil when there are none.
func (s *Session) buildRules(mode agent.PermissionMode, agentName string, grants []permissionGrant) []PermissionRule {
	s.mu.Lock()
	inherited := slices.Clone(s.inherited)
	agentRules, known := s.agentRules[agentName]
	if !known && s.agentsLoaded {
		agentRules = unknownAgentRules
	}
	agentRules = slices.Clone(agentRules)
	s.mu.Unlock()
	// 1. Base. Every base rule has resource "*" and an action without
	// wildcards or "*" itself, which keepAgentDenies relies on.
	var base []PermissionRule
	switch mode {
	case agent.PermissionDefault:
		base = []PermissionRule{rule("shell", "ask"), rule("edit", "ask"), rule("external_directory", "ask")}
	case agent.PermissionAcceptEdits, agent.PermissionPlan, agent.PermissionReadOnly:
		base = []PermissionRule{rule("shell", "ask"), rule("external_directory", "ask")}
	case agent.PermissionBypass:
		base = []PermissionRule{rule("*", "allow")}
	}
	var rules []PermissionRule
	if mode == "" {
		rules = inherited
	} else {
		// 2. Agent denies.
		rules = keepAgentDenies(agentRules, base)
	}
	// 3. Allowed tools and directories.
	for _, name := range s.cfg.AllowedTools {
		rules = append(rules, rule(name, "allow"))
	}
	for _, dir := range s.cfg.AdditionalDirectories {
		dir = providerutil.AbsDir(dir)
		rules = append(rules,
			PermissionRule{Action: "external_directory", Resource: dir, Effect: "allow"},
			PermissionRule{Action: "external_directory", Resource: filepath.Join(dir, "*"), Effect: "allow"})
	}
	// 4. Tool rules.
	for _, name := range slices.Sorted(maps.Keys(s.cfg.MCPServers)) {
		srv := s.cfg.MCPServers[name]
		if len(srv.EnabledTools) > 0 {
			// Server names are already clean (mcpServerName), and no other
			// server's name starts with name+"_" (mcpNameCollision).
			rules = append(rules, rule(name+"_*", "deny"))
			for _, tool := range srv.EnabledTools {
				rules = append(rules, rule(toolAction(name, tool), "allow"))
			}
		}
		for _, tool := range srv.ApprovedTools {
			rules = append(rules, rule(toolAction(name, tool), "allow"))
		}
	}
	if s.host != nil || s.srv != nil && s.srv.shared {
		rules = append(rules, rule(hostToolPrefix+"*", "deny"))
	}
	if s.host != nil {
		rules = append(rules, rule(s.host.name+"_*", "allow"))
		for _, t := range s.cfg.Tools {
			if t.NeedsApproval {
				rules = append(rules, rule(toolAction(s.host.name, t.Name), "ask"))
			}
		}
	}
	// 5. Mode denies.
	switch mode {
	case agent.PermissionPlan:
		rules = append(rules, rule("edit", "deny"))
		if agentName == "plan" {
			rules = append(rules, planPathRules(agentRules)...)
		}
	case agent.PermissionReadOnly:
		rules = append(rules, rule("edit", "deny"))
	}
	// 6. Disallowed tools.
	for _, name := range s.cfg.DisallowedTools {
		rules = append(rules, rule(name, "deny"))
	}
	// 7. The caller's rules.
	rules = append(rules, s.opts.Permissions...)
	rules = dedupeRules(rules)
	for _, g := range grants {
		rules = insertGrant(agentRules, rules, g)
	}
	rules = dedupeRules(rules)
	if len(rules) == 0 {
		return nil
	}
	return rules
}

// keepAgentDenies returns base followed by the agent's rules from its
// first deny on, each rule other than a deny followed by the base rules
// narrowed to it. A request the agent denies is decided by one of those
// denies; any other request the agent's tail matches is decided by the
// last base rule that matches it, or else by the agent's own rule, which
// is what base alone gives. Narrowing is exact because every base rule
// has resource "*" and an action that is "*" or has no wildcards.
func keepAgentDenies(agentRules, base []PermissionRule) []PermissionRule {
	rules := slices.Clone(base)
	first := slices.IndexFunc(agentRules, func(r PermissionRule) bool { return r.Effect == "deny" })
	if first < 0 {
		return rules
	}
	for _, r := range agentRules[first:] {
		rules = append(rules, r)
		if r.Effect == "deny" {
			continue
		}
		for _, b := range base {
			action := r.Action
			switch {
			case b.Action == "*":
			case globMatch(r.Action, b.Action):
				action = b.Action
			default:
				continue
			}
			rules = append(rules, PermissionRule{Action: action, Resource: r.Resource, Effect: b.Effect})
		}
	}
	return rules
}

// planPathRules returns the plan agent's allows of its plan paths, each
// followed by the agent's later denies of the same action, so that a
// config deny of a plan path still decides.
func planPathRules(agentRules []PermissionRule) []PermissionRule {
	var rules []PermissionRule
	for i, r := range agentRules {
		if r.Effect != "allow" || r.Resource == "*" || r.Action != "edit" && r.Action != "external_directory" {
			continue
		}
		rules = append(rules, r)
		for _, later := range agentRules[i+1:] {
			if later.Effect == "deny" && globMatch(later.Action, r.Action) {
				rules = append(rules, PermissionRule{Action: r.Action, Resource: later.Resource, Effect: "deny"})
			}
		}
	}
	return rules
}

// dedupeRules drops exact duplicates, keeping the last occurrence.
func dedupeRules(rules []PermissionRule) []PermissionRule {
	seen := map[PermissionRule]bool{}
	out := make([]PermissionRule, 0, len(rules))
	for i := len(rules) - 1; i >= 0; i-- {
		if !seen[rules[i]] {
			seen[rules[i]] = true
			out = append(out, rules[i])
		}
	}
	slices.Reverse(out)
	return out
}

// insertGrant places grant g into the session rules, after the agent's
// rules agentRules; see the model above.
func insertGrant(agentRules, rules []PermissionRule, g permissionGrant) []PermissionRule {
	if strings.ContainsAny(g.Action, "*?") {
		return rules
	}
	all := append(slices.Clone(agentRules), rules...)
	at := -1
	for i, r := range all {
		if globMatch(r.Action, g.Action) && globMatch(r.Resource, g.Asked) {
			at = i
		}
	}
	answer := PermissionRule{Action: g.Action, Resource: "*", Effect: "ask"}
	if at >= 0 {
		answer = all[at]
	}
	if answer.Effect != "ask" {
		return rules
	}
	pattern, ok := grantPattern(g, answer.Resource)
	if !ok {
		return rules
	}
	grant := PermissionRule{Action: g.Action, Resource: pattern, Effect: "allow"}
	if at >= len(agentRules) {
		return slices.Insert(slices.Clone(rules), at-len(agentRules)+1, grant)
	}
	head := []PermissionRule{grant}
	for _, r := range agentRules[at+1:] {
		if globMatch(r.Action, g.Action) {
			head = append(head, PermissionRule{Action: g.Action, Resource: r.Resource, Effect: r.Effect})
		}
	}
	return append(head, rules...)
}

// grantPattern returns the resource pattern a grant allows under an
// answering rule with resource ruleResource, and false when none is
// narrow enough.
func grantPattern(g permissionGrant, ruleResource string) (string, bool) {
	for _, p := range g.Patterns {
		if globMatch(p, g.Asked) && globCovers(ruleResource, p) {
			return p, true
		}
	}
	if g.Asked != "*" && strings.ContainsAny(g.Asked, "*?") {
		return "", false
	}
	return g.Asked, globCovers(ruleResource, g.Asked)
}

// OpenCode matches globs as JavaScript regular expressions over UTF-16
// code units. globRegexp reproduces that over runes: every code unit u is
// the rune unitBase+u, so "?" matches one unit as in JavaScript. The two
// symbols stand for an inner pattern's wildcards in globCovers; no mapped
// unit equals them.
const (
	unitBase   = 0xF0000
	symbolAny  = '\U0010FFFD' // "*" of an inner pattern
	symbolUnit = '\U0010FFFC' // "?" of an inner pattern
)

var globCache sync.Map

// codeUnits returns s with "\" read as "/", as UTF-16 code units.
func codeUnits(s string) []uint16 {
	return utf16.Encode([]rune(strings.ReplaceAll(s, `\`, "/")))
}

func unitRune(u uint16) rune { return rune(unitBase + int(u)) }

// globRegexp is OpenCode's rb pattern: "\" reads as "/", "*" matches any
// text, "?" any one code unit, a trailing " *" also matches nothing, and
// everything else is literal and case-sensitive.
func globRegexp(pattern string) *regexp.Regexp {
	if re, ok := globCache.Load(pattern); ok {
		return re.(*regexp.Regexp)
	}
	units := codeUnits(pattern)
	trailing := len(units) >= 2 && units[len(units)-2] == ' ' && units[len(units)-1] == '*'
	if trailing {
		units = units[:len(units)-2]
	}
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for _, u := range units {
		switch u {
		case '*':
			b.WriteString(`.*`)
		case '?':
			// Any unit, but not a whole inner "*", which can be empty or long.
			b.WriteString(`[^` + string(symbolAny) + `]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(unitRune(u))))
		}
	}
	if trailing {
		b.WriteString(`(?:`)
		b.WriteString(regexp.QuoteMeta(string(unitRune(' '))))
		b.WriteString(`.*)?`)
	}
	b.WriteString(`$`)
	re, _ := globCache.LoadOrStore(pattern, regexp.MustCompile(b.String()))
	return re.(*regexp.Regexp)
}

// globMatch reports whether OpenCode's glob pattern matches s.
func globMatch(pattern, s string) bool {
	if pattern == "*" || pattern == s {
		return true
	}
	units := codeUnits(s)
	text := make([]rune, len(units))
	for i, u := range units {
		text[i] = unitRune(u)
	}
	return globRegexp(pattern).MatchString(string(text))
}

// globCovers reports whether every text that inner matches is matched by
// outer. It is sound but not complete: false can mean "not proven". It
// matches outer against inner with inner's wildcards as symbols that only
// outer's wildcards can match, so outer matches every text inner stands
// for.
func globCovers(outer, inner string) bool {
	if outer == "*" || outer == inner {
		return true
	}
	re := globRegexp(outer)
	symbolic := func(units []uint16) string {
		text := make([]rune, len(units))
		for i, u := range units {
			switch u {
			case '*':
				text[i] = symbolAny
			case '?':
				text[i] = symbolUnit
			default:
				text[i] = unitRune(u)
			}
		}
		return string(text)
	}
	units := codeUnits(inner)
	if n := len(units); n >= 2 && units[n-2] == ' ' && units[n-1] == '*' {
		// A trailing " *" also matches the text without it.
		if !re.MatchString(symbolic(units[:n-2])) {
			return false
		}
	}
	return re.MatchString(symbolic(units))
}

func rule(action, effect string) PermissionRule {
	return PermissionRule{Action: action, Resource: "*", Effect: effect}
}

// toolAction is the permission action of an MCP tool.
func toolAction(server, tool string) string {
	return cleanToolName(server) + "_" + cleanToolName(tool)
}

// cleanToolName is a name as OpenCode puts it in a tool action: every
// UTF-16 code unit other than a letter, digit, "_", or "-" becomes "_".
func cleanToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		n := utf16.RuneLen(r)
		if n < 1 {
			n = 1
		}
		b.WriteString(strings.Repeat("_", n))
	}
	return b.String()
}

// loadAgents reads the agents' rules and the default agent afresh:
// OpenCode reloads its config and agents while it runs. A server that just
// started can list no agents for a moment; when it still lists none, the
// rules read before are kept.
func (s *Session) loadAgents(ctx context.Context) error {
	var agents []AgentInfo
	for attempt := 0; attempt < 8 && len(agents) == 0; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		var err error
		if agents, _, err = s.listAgents(ctx); err != nil {
			return fmt.Errorf("opencode: list agents for permission rules: %w", err)
		}
	}
	if len(agents) == 0 {
		s.rt.Logger().Warn("opencode: the server lists no agents; permission rules keep the agent rules read before")
		return nil
	}
	rules := make(map[string][]PermissionRule, len(agents))
	for _, a := range agents {
		rules[a.ID] = a.Permissions
	}
	def := defaultAgentOf(agents)
	s.mu.Lock()
	s.agentRules, s.agentsLoaded, s.defaultAgent = rules, true, def
	s.mu.Unlock()
	return nil
}

// followDefaultAgent writes the rules again when the session runs the
// server's default agent and that changed, before input runs it: OpenCode
// picks the agent of such a session when it runs, and can list its config's
// agents only a moment after it starts. The agents are read without
// rulesMu, so input waits for rule writes only when the agent changed. A
// failed read fails the input.
func (s *Session) followDefaultAgent(ctx context.Context) error {
	s.mu.Lock()
	follows := s.agentFollowsDefault
	s.mu.Unlock()
	if !follows {
		return nil
	}
	agents, _, err := s.listAgents(ctx)
	if err != nil {
		return fmt.Errorf("opencode: list agents for permission rules: %w", err)
	}
	def := defaultAgentOf(agents)
	s.mu.Lock()
	same := def == "" || def == s.agentName && !s.rulesStale
	s.mu.Unlock()
	if same {
		return nil
	}
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	return s.writeRulesLocked(ctx, s.Info().PermissionMode)
}

// defaultAgentOf returns the default agent of agents: OpenCode lists its
// default agent, default_agent or else build, first among the primary
// agents.
func defaultAgentOf(agents []AgentInfo) string {
	for _, a := range agents {
		if a.Mode != "subagent" && !a.Hidden {
			return a.ID
		}
	}
	return ""
}

// subagentRules is the rule state of a subagent session. OpenCode copies
// the parent's rules into a subagent session when it creates it and keeps
// them apart afterwards, so this library writes them itself.
type subagentRules struct {
	agent  string
	grants []permissionGrant
	// rules are the rules last written, unless known is false.
	rules []PermissionRule
	known bool
}

// rulesTimeout bounds a rule write, which runs past its caller's context
// so that the server and this library agree on the rules in place.
const rulesTimeout = 30 * time.Second

// patchRules writes the rules of session sid. Once sent, the write is not
// cancelled with ctx: a write cancelled after the server applied it would
// leave rules nobody reports. sent reports whether the write went out, so
// that its outcome is unknown when err is not nil.
func (s *Session) patchRules(ctx context.Context, sid string, rules []PermissionRule) (sent bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.closing.Load() {
		// A resumed session may own the rules now, or an ephemeral one is
		// being deleted.
		return false, s.rt.ClosedErr()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rulesTimeout)
	defer cancel()
	if rules == nil {
		rules = []PermissionRule{}
	}
	return true, s.c.Call(ctx, "PATCH", sessionPath(sid, ""), map[string]any{"permissions": rules}, nil)
}

// putRulesLocked writes rules as this session's rules and records them, or
// marks the rules stale when a failed write may have reached the server.
// The caller holds rulesMu.
func (s *Session) putRulesLocked(ctx context.Context, rules []PermissionRule) error {
	sent, err := s.patchRules(ctx, s.ID(), rules)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.rulesStale = s.rulesStale || sent
		return err
	}
	s.rules, s.rulesStale = rules, false
	return nil
}

// writeRules writes the rules for mode unless they are already in place.
func (s *Session) writeRules(ctx context.Context, mode agent.PermissionMode) error {
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	return s.writeRulesLocked(ctx, mode)
}

// writeRulesLocked is writeRules for callers that hold rulesMu. It also
// writes the rules of the known subagent sessions; a failure there is
// logged and leaves that session's rules to the next write, so it does not
// undo this session's change.
func (s *Session) writeRulesLocked(ctx context.Context, mode agent.PermissionMode) error {
	if err := s.loadAgents(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	grants := slices.Clone(s.grants)
	agentName := s.agentName
	if s.agentFollowsDefault && s.defaultAgent != "" {
		// The agent becomes the new default only with its rules written,
		// so a failed write is tried again.
		agentName = s.defaultAgent
	}
	s.mu.Unlock()
	rules := s.buildRules(mode, agentName, grants)
	s.mu.Lock()
	same := !s.rulesStale && slices.Equal(rules, s.rules)
	s.mu.Unlock()
	if !same {
		if err := s.putRulesLocked(ctx, rules); err != nil {
			return fmt.Errorf("opencode: set permission rules: %w", err)
		}
	}
	s.mu.Lock()
	s.rulesAgent = agentName
	if s.agentFollowsDefault {
		s.agentName = agentName
	}
	// An agent selected while the rules were written gets its rules now.
	again := s.agentName != agentName
	ids := slices.Sorted(maps.Keys(s.subagentRules))
	s.mu.Unlock()
	if again {
		s.rewriteRules()
	}
	// The subagents' rules follow this session's, which are written now,
	// so their writes run past the caller's context too, each bounded.
	var wg sync.WaitGroup
	limit := make(chan struct{}, subagentWrites)
	for _, id := range ids {
		wg.Add(1)
		limit <- struct{}{}
		go func() {
			defer func() { <-limit; wg.Done() }()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rulesTimeout)
			defer cancel()
			if err := s.writeSubagentRulesLocked(ctx, id, mode); err != nil {
				s.rt.Logger().Warn("opencode: cannot set subagent permission rules", "session", id, "error", err)
			}
		}()
	}
	wg.Wait()
	return nil
}

// subagentWrites bounds the subagent rule writes in flight.
const subagentWrites = 8

// writeSubagentRulesLocked writes the rules of subagent session id for
// mode: the parent's rules built for the subagent's agent, with the
// parent's grants and then its own. The caller holds rulesMu and has read
// the agents. A failed write leaves the session's rules unknown, so the
// next write sends them again.
func (s *Session) writeSubagentRulesLocked(ctx context.Context, id string, mode agent.PermissionMode) error {
	s.mu.Lock()
	sub := s.subagentRules[id]
	if sub == nil {
		s.mu.Unlock()
		return nil
	}
	name := sub.agent
	s.mu.Unlock()
	path := sessionPath(id, "")
	if name == "" {
		var native nativeSession
		err := s.c.data(ctx, "GET", path, nil, &native)
		if errors.Is(err, agent.ErrSessionNotFound) || httpStatus(err) == 404 {
			s.forgetSubagent(id)
			return nil
		}
		if err != nil {
			s.mu.Lock()
			sub.known = false
			s.mu.Unlock()
			return fmt.Errorf("opencode: read subagent session: %w", err)
		}
		name = native.Agent
		s.mu.Lock()
		if native.Agent == "" {
			name = s.defaultAgent
		}
		if sub.agent == "" {
			sub.agent = name
		}
		name = sub.agent
		s.mu.Unlock()
	}
	s.mu.Lock()
	grants := append(slices.Clone(s.grants), sub.grants...)
	written, known := sub.rules, sub.known
	s.mu.Unlock()
	rules := s.buildRules(mode, name, grants)
	if known && slices.Equal(rules, written) {
		return nil
	}
	_, err := s.patchRules(ctx, id, rules)
	if errors.Is(err, agent.ErrSessionNotFound) || httpStatus(err) == 404 {
		s.forgetSubagent(id)
		return nil
	}
	s.mu.Lock()
	sub.rules, sub.known = rules, err == nil
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("opencode: set subagent permission rules: %w", err)
	}
	return nil
}

func (s *Session) forgetSubagent(id string) {
	s.mu.Lock()
	delete(s.subagentRules, id)
	s.mu.Unlock()
}

// addSubagentLocked starts tracking the rules of subagent session id,
// running agent if known, with rules, the rules OpenCode gave it, when
// known. The caller holds s.mu. It reports whether the session is new;
// the caller then schedules writeSubagent.
func (s *Session) addSubagentLocked(id, agentName string, rules *[]PermissionRule) bool {
	if id == "" {
		return false
	}
	if sub := s.subagentRules[id]; sub != nil {
		return false
	}
	if s.subagentRules == nil {
		s.subagentRules = map[string]*subagentRules{}
	}
	sub := &subagentRules{agent: agentName}
	if rules != nil {
		sub.rules, sub.known = slices.Clone(*rules), true
	}
	s.subagentRules[id] = sub
	return true
}

// setSubagentAgentLocked records the agent subagent session id runs now,
// as its events or its subagent call report it. The caller holds s.mu. It
// reports whether the agent changed; the caller then schedules
// writeSubagent.
func (s *Session) setSubagentAgentLocked(id, agentName string) bool {
	sub := s.subagentRules[id]
	if sub == nil || agentName == "" || sub.agent == agentName {
		return false
	}
	sub.agent = agentName
	return true
}

// writeSubagent writes the rules of subagent session id for the current
// mode, in the background: the event that calls for it must not wait for
// the server.
func (s *Session) writeSubagent(id string) {
	s.inBackground(func() {
		ctx := s.rt.Context()
		s.rulesMu.Lock()
		defer s.rulesMu.Unlock()
		err := s.loadAgents(ctx)
		if err == nil {
			err = s.writeSubagentRulesLocked(ctx, id, s.Info().PermissionMode)
		}
		if err != nil && ctx.Err() == nil {
			s.rt.Logger().Warn("opencode: cannot set subagent permission rules", "session", id, "error", err)
		}
	})
}

// rewriteRules writes the rules for the current mode in the background,
// after the primary agent changed outside SetAgent and SetPermissionMode
// or the server reloaded its agents.
func (s *Session) rewriteRules() {
	s.mu.Lock()
	if s.rewritePending {
		// The pending rewrite has not started, so it sees this change.
		s.mu.Unlock()
		return
	}
	s.rewritePending = true
	s.mu.Unlock()
	s.inBackground(func() {
		ctx := s.rt.Context()
		s.rulesMu.Lock()
		defer s.rulesMu.Unlock()
		s.mu.Lock()
		s.rewritePending = false
		resync := s.resyncWanted
		s.resyncWanted = false
		s.mu.Unlock()
		if resync {
			s.readAgentLocked(ctx)
		}
		if err := s.writeRulesLocked(ctx, s.Info().PermissionMode); err != nil && ctx.Err() == nil && !s.closing.Load() {
			s.rt.Logger().Warn("opencode: cannot set permission rules for the new agent", "error", err)
		}
	})
}

// readAgentLocked reads the session's agent from the server, unless an
// agent selection overtakes the read. The caller holds rulesMu.
func (s *Session) readAgentLocked(ctx context.Context) {
	s.mu.Lock()
	changes := s.agentChanges
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, rulesTimeout)
	defer cancel()
	var native nativeSession
	if err := s.c.data(ctx, "GET", s.path(""), nil, &native); err != nil {
		if ctx.Err() == nil && !s.closing.Load() {
			s.rt.Logger().Warn("opencode: cannot read the session's agent after reconnecting", "error", err)
		}
		// Read again with the next rewrite.
		s.mu.Lock()
		s.resyncWanted = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentChanges != changes {
		return
	}
	s.agentFollowsDefault = native.Agent == ""
	if native.Agent != "" {
		s.agentName = native.Agent
	}
}

// resyncRules makes the agent and the rules follow the server again after
// events may have been lost: the background rewrite reads the session's
// agent first, the subagents read theirs again, and all rules are written.
func (s *Session) resyncRules() {
	s.mu.Lock()
	s.rulesStale, s.resyncWanted = true, true
	for _, sub := range s.subagentRules {
		sub.agent, sub.known = "", false
	}
	s.mu.Unlock()
	s.rewriteRules()
}

// grant records an AllowSession answer for a request about asked, with
// OpenCode's save patterns, and writes the rules; see rules.go. It goes to
// this session's grants, or to those of the subagent session that asked.
// When the write fails, the answer's new grants are dropped again.
func (s *Session) grant(ctx context.Context, sid, action string, save, asked []string) error {
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()
	if sid != s.ID() {
		// writeRulesLocked reads the agents itself.
		if err := s.loadAgents(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	mode := s.info.PermissionMode
	list := &s.grants
	if sid != s.ID() {
		s.addSubagentLocked(sid, "", nil)
		list = &s.subagentRules[sid].grants
	}
	var added []permissionGrant
	for _, g := range newGrants(action, save, asked) {
		if !slices.ContainsFunc(*list, g.equal) {
			*list = append(*list, g)
			added = append(added, g)
		}
	}
	s.mu.Unlock()
	var err error
	if sid != s.ID() {
		err = s.writeSubagentRulesLocked(ctx, sid, mode)
	} else {
		err = s.writeRulesLocked(ctx, mode)
	}
	if err != nil {
		s.mu.Lock()
		if sid == s.ID() || s.subagentRules[sid] != nil {
			*list = slices.DeleteFunc(*list, func(g permissionGrant) bool { return slices.ContainsFunc(added, g.equal) })
		}
		s.mu.Unlock()
	}
	return err
}
