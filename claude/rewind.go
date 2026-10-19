package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// RewindResult describes a finished rewind.
type RewindResult struct {
	// Native reports that the CLI rewound in place with the
	// rewind_conversation control request; false means the process was
	// restarted on a truncated resume.
	Native bool
	// TargetMessageID is the prompt UUID of the first dropped turn.
	TargetMessageID string
	// PrefillText is that prompt's text, for an "edit and resend" UI.
	PrefillText string
	// PrecedingAssistantID is the last kept assistant message, when known.
	PrecedingAssistantID string
	// RemovedItemIDs lists the item IDs the ItemsRemoved event reported.
	RemovedItemIDs []string
	// SessionID is the session ID after the rewind.
	SessionID string
}

// RewindRefusedError reports a rewind the CLI refused. The conversation is
// unchanged.
type RewindRefusedError struct {
	// Reason is the CLI's reason code, such as "commands_queued",
	// "unseen_later_turn", or "target_splits_tool_call", or
	// "resume_drops_turn_rejected" when a restart's truncated resume was
	// refused because the dropped range held messages of other turns.
	Reason  string
	Message string
}

func (e *RewindRefusedError) Error() string {
	return "claude: rewind refused: " + cmp.Or(e.Message, e.Reason)
}

// errDropsTurnRejected reports that the CLI refused a --resume-drops-turn
// resume at startup.
var errDropsTurnRejected = errors.New("claude: the CLI refused the truncated resume")

// rewindFallbackReasons are refusals of rewind_conversation after which a
// restart on a truncated resume can still drop the turn: the target is
// only in the transcript (such as before a compaction), or the in-place
// rewind could not be recorded.
var rewindFallbackReasons = []string{"target_not_found", "stale_target", "persist_failed"}

// Rewind drops turnID and every later turn; see RewindWithResult.
func (s *Session) Rewind(ctx context.Context, turnID string) error {
	_, err := s.RewindWithResult(ctx, turnID)
	return err
}

// RewindWithResult drops turnID and every later turn and reports the
// dropped prompt's text. turnID is a TurnResult.TurnID, an item ID from
// this session, or a message UUID from the transcript (the turn containing
// that message is dropped).
//
// It asks the CLI to rewind in place (rewind_conversation), which keeps
// the process, its background tasks, and the session ID. When the CLI does
// not support that or cannot find the turn in memory, it restarts the CLI
// with --resume, --resume-session-at set to the transcript entry before
// that turn's prompt, and --resume-drops-turn set to the prompt; rewinding
// the first turn that way starts a new, empty session with a new ID, which
// a SessionUpdated event reports. A restart needs a persisted session (not
// Ephemeral), withdraws pending requests, and stops background tasks; the
// Session value, its event stream, and its settings stay valid. When the
// restarted CLI refuses the truncated resume because messages of other
// turns would be lost, the session resumes unchanged and the error is a
// *RewindRefusedError.
//
// Either way an ItemsRemoved event lists the dropped turns' items this
// session reported, and for a turn from before this process, the item IDs
// ReadHistory gives that turn's messages. File changes are not undone; see
// RewindFiles. Rewind fails while a turn runs.
func (s *Session) RewindWithResult(ctx context.Context, turnID string) (*RewindResult, error) {
	s.swapMu.Lock()
	defer s.swapMu.Unlock()
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	if s.rt.Busy() {
		return nil, errors.New("claude: cannot rewind while a turn runs")
	}

	// A live turn is known by its ID or one of its items; anything else is
	// resolved through the transcript, read before the CLI changes it.
	target := s.turnItems.turnOf(turnID)
	if target == "" {
		target = s.turnItems.turnOf(s.translator().messageOf(turnID))
	}
	var g *transcriptGraph
	var historic []string
	if !s.cfg.Ephemeral {
		var err error
		g, err = loadTranscript(ctx, s.provider.configDir(s.cfg), s.ID(), s.cfg.Cwd)
		if err != nil && target == "" {
			return nil, err
		}
		if g != nil && target == "" {
			i, err := g.promptIndex(s.translator().messageOf(turnID))
			if err != nil {
				return nil, err
			}
			target = g.entries[i].UUID
			historic = g.itemIDsFrom(i)
		}
	}
	if target == "" {
		target = turnID
	}

	res, err := s.nativeRewind(ctx, target)
	var refused *RewindRefusedError
	switch {
	case err == nil:
	case errors.As(err, &refused) && !slices.Contains(rewindFallbackReasons, refused.Reason):
		return nil, err
	case errors.Is(err, errRewindUnsupported) || refused != nil:
		if g == nil {
			return nil, err
		}
		if res, err = s.restartRewind(ctx, g, target); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	turns, ids := s.turnItems.drop(target)
	if len(turns) == 0 {
		// A turn from before this process: every live turn came later.
		if first := s.turnItems.first(); first != "" {
			turns, ids = s.turnItems.drop(first)
		}
	}
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	for _, id := range historic {
		if !have[id] {
			have[id] = true
			ids = append(ids, id)
		}
	}
	s.translator().forget(turns, ids)
	if len(ids) > 0 {
		s.rt.Emit(&agent.ItemsRemoved{IDs: ids})
	}
	res.RemovedItemIDs = ids
	res.SessionID = s.ID()
	return res, nil
}

// errRewindUnsupported reports a CLI without rewind_conversation.
var errRewindUnsupported = errors.New("claude: the CLI cannot rewind in place")

// nativeRewind sends rewind_conversation for the turn whose prompt is
// target. A refusal is a *RewindRefusedError; a CLI that does not know
// the request, or answers in an unknown shape, gives errRewindUnsupported.
func (s *Session) nativeRewind(ctx context.Context, target string) (*RewindResult, error) {
	raw, err := s.Control(ctx, "rewind_conversation", map[string]any{"target_message_uuid": target})
	if pe, ok := errors.AsType[*agent.ProtocolError](err); ok {
		return nil, fmt.Errorf("%w: %s", errRewindUnsupported, pe.Message)
	}
	if err != nil {
		return nil, err
	}
	var r struct {
		Rewound                *bool  `json:"rewound"`
		TargetMessageUUID      string `json:"targetMessageUuid"`
		PrefillText            string `json:"prefillText"`
		PrecedingAssistantUUID string `json:"precedingAssistantUuid"`
		Error                  string `json:"error"`
		Reason                 string `json:"reason"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Rewound == nil {
		return nil, errRewindUnsupported
	}
	if !*r.Rewound {
		return nil, &RewindRefusedError{Reason: r.Reason, Message: r.Error}
	}
	return &RewindResult{
		Native:               true,
		TargetMessageID:      cmp.Or(r.TargetMessageUUID, target),
		PrefillText:          r.PrefillText,
		PrecedingAssistantID: r.PrecedingAssistantUUID,
	}, nil
}

// restartRewind drops the turn whose prompt is target by restarting the
// CLI on a truncated resume of the transcript g.
func (s *Session) restartRewind(ctx context.Context, g *transcriptGraph, target string) (*RewindResult, error) {
	prompt, err := g.promptIndex(target)
	if err != nil {
		return nil, err
	}
	res := &RewindResult{TargetMessageID: g.entries[prompt].UUID, PrefillText: promptText(g.entries[prompt].Message)}
	resumeAt := ""
	if p := g.parent(prompt); p >= 0 {
		resumeAt = g.entries[p].UUID
	}
	for i := g.parent(prompt); i >= 0; i = g.parent(i) {
		if e := g.entries[i]; e.Type == "assistant" && !e.IsSidechain {
			res.PrecedingAssistantID = e.UUID
			break
		}
	}

	sessionID := s.ID()
	s.retireProcess()
	cfg, opts := s.cfg, s.opts
	cfg.Fork, cfg.ForkAtTurn = false, ""
	opts.Continue = false
	if resumeAt == "" {
		sessionID = providerutil.NewUUID()
		cfg.Resume, opts.ResumeSessionAt, opts.ResumeDropsTurn = "", "", ""
	} else {
		cfg.Resume, opts.ResumeSessionAt, opts.ResumeDropsTurn = sessionID, resumeAt, res.TargetMessageID
	}
	s.rt.SetSessionID(sessionID)
	init, err := s.launch(ctx, cfg, opts, sessionID, true)
	var refused error
	if errors.Is(err, errDropsTurnRejected) {
		// The refusal is deterministic: resume the whole conversation.
		s.retireProcess()
		opts.ResumeSessionAt, opts.ResumeDropsTurn = "", ""
		init, err = s.launch(ctx, cfg, opts, sessionID, true)
		refused = &RewindRefusedError{Reason: "resume_drops_turn_rejected", Message: "the dropped range holds messages of other turns; the session resumed unchanged"}
	}
	if err != nil {
		err = fmt.Errorf("claude: restart after rewind: %w", err)
		s.rt.Finish(err)
		s.shutdown()
		return nil, err
	}
	s.adopt(s.current())
	s.infoMu.Lock()
	s.applyInitLocked(init)
	s.info.ID = sessionID
	info := s.info
	s.infoMu.Unlock()
	s.rt.Emit(&agent.SessionUpdated{EventMeta: agent.EventMeta{Raw: init.Raw}, Info: info})
	if refused != nil {
		return nil, refused
	}
	return res, nil
}

// retireProcess stops the running CLI process, withdraws its pending
// requests, ends its tasks and open items, and gives the session a fresh
// translator that keeps the item lookups.
func (s *Session) retireProcess() {
	old := s.current()
	old.retired.Store(true)
	old.Stop(s.provider.shutdownGrace())
	<-old.done
	s.ctrlMu.Lock()
	for id, p := range s.origins {
		if p == old {
			delete(s.origins, id)
		}
	}
	for _, cancel := range s.inflight {
		cancel()
	}
	// The next process has its own request IDs; its prompts are new.
	clear(s.seen)
	s.ctrlMu.Unlock()
	s.rt.WithdrawAll()
	// A turn the CLI started on its own after the idle check dies with the
	// process; nothing else would ever finish it.
	for _, id := range s.rt.TurnIDs() {
		s.rt.AbandonTurn(id, errRestarted)
	}
	prev := s.translator()
	prev.stop()
	// The old process's background work dies with it; nothing else would
	// end its tasks and open calls.
	prev.endRunningTasks(agent.TaskStopped, "the claude process restarted")
	prev.endOpenItems(agent.ItemCancelled, false, "")
	tr := newTranslator(s)
	tr.keepLookups(prev)
	s.procMu.Lock()
	s.tr = tr
	s.procMu.Unlock()
}

// forkPoint returns the --resume-session-at value that keeps a persisted
// session up to and including turnID.
func forkPoint(ctx context.Context, configDir, sessionID, cwd, turnID string) (string, error) {
	g, err := loadTranscript(ctx, configDir, sessionID, cwd)
	if err != nil {
		return "", err
	}
	return g.forkPoint(turnID)
}

// transcriptGraph is a session transcript's chain entries, linked by
// parentUuid. Rewinds and in-place resumes add branches, so the
// conversation is the chain ending at the latest entry, not file order.
type transcriptGraph struct {
	entries []transcriptEntry
	byID    map[string]int
}

func loadTranscript(ctx context.Context, configDir, sessionID, cwd string) (*transcriptGraph, error) {
	path, err := findTranscript(configDir, sessionID, cwd)
	if err != nil {
		return nil, err
	}
	return readTranscriptGraph(ctx, path)
}

func readTranscriptGraph(ctx context.Context, path string) (*transcriptGraph, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g := &transcriptGraph{byID: map[string]int{}}
	err := eachTranscriptEntry(path, isNotAPIRequestLine, func(e transcriptEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.UUID != "" {
			if _, dup := g.byID[e.UUID]; !dup {
				g.byID[e.UUID] = len(g.entries)
				g.entries = append(g.entries, e)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// parent returns the index of e's parent entry, or -1. A compact boundary
// has no parent but links to the conversation before it.
func (g *transcriptGraph) parent(i int) int {
	e := g.entries[i]
	id := e.ParentUUID
	if id == "" {
		id = e.LogicalParentUUID
	}
	if j, ok := g.byID[id]; ok && id != "" {
		return j
	}
	return -1
}

// linked reports whether the entries carry parentUuid links at all.
func (g *transcriptGraph) linked() bool {
	for _, e := range g.entries {
		if e.ParentUUID != "" || e.LogicalParentUUID != "" {
			return true
		}
	}
	return false
}

// leaf returns the latest main-conversation message, or -1.
func (g *transcriptGraph) leaf() int {
	for i, e := range slices.Backward(g.entries) {

		if (e.Type == "user" || e.Type == "assistant") && !e.IsSidechain {
			return i
		}
	}
	return -1
}

// chain returns the entries from the root to leaf, in order.
func (g *transcriptGraph) chain(leaf int) []int {
	var out []int
	seen := map[int]bool{}
	for i := leaf; i >= 0 && !seen[i]; i = g.parent(i) {
		seen[i] = true
		out = append(out, i)
	}
	slices.Reverse(out)
	return out
}

// isPrompt reports whether entry i starts a turn: a user message with
// content of its own rather than tool results.
func (g *transcriptGraph) isPrompt(i int) bool {
	e := g.entries[i]
	if e.Type != "user" || e.IsMeta || e.IsCompactSummary || e.IsSidechain {
		return false
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &m) != nil {
		return false
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		return text != ""
	}
	var blocks []contentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return true
		}
	}
	return false
}

// find resolves a message UUID, also accepting the "uuid/n" item IDs
// ReadHistory derives for later content blocks.
func (g *transcriptGraph) find(id string) (int, error) {
	if i, ok := g.byID[id]; ok {
		return i, nil
	}
	if base, _, ok := strings.Cut(id, "/"); ok {
		if i, ok := g.byID[base]; ok {
			return i, nil
		}
	}
	return -1, fmt.Errorf("claude: turn %s not found in the session transcript", id)
}

// promptIndex returns the prompt entry of the turn containing id.
func (g *transcriptGraph) promptIndex(id string) (int, error) {
	i, err := g.find(id)
	if err != nil {
		return -1, err
	}
	seen := map[int]bool{}
	for ; i >= 0 && !seen[i]; i = g.parent(i) {
		seen[i] = true
		if g.isPrompt(i) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("claude: no prompt found for %s in the session transcript", id)
}

// itemIDsFrom returns the item IDs ReadHistory gives the conversation from
// entry i to the latest message, when i is on that conversation.
func (g *transcriptGraph) itemIDsFrom(i int) []string {
	chain := g.chain(g.leaf())
	pos := slices.Index(chain, i)
	if pos < 0 {
		return nil
	}
	var ids []string
	for _, j := range chain[pos:] {
		e := g.entries[j]
		if e.IsSidechain {
			continue
		}
		if e.Type == "system" && e.Subtype == "compact_boundary" {
			ids = append(ids, e.UUID)
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		ids = append(ids, e.UUID)
		var m struct {
			Content []contentBlock `json:"content"`
		}
		_ = json.Unmarshal(e.Message, &m)
		for k, b := range m.Content {
			if k > 0 {
				ids = append(ids, historyItemID(e.UUID, k))
			}
			if b.ID != "" {
				ids = append(ids, b.ID, b.ID+"/plan")
			}
			if b.ToolUseID != "" {
				ids = append(ids, b.ToolUseID+"/plan")
			}
		}
	}
	return ids
}

// forkPoint returns the entry to resume at so that the conversation is
// kept up to and including id: through the end of the turn when id is a
// prompt, otherwise through that message.
func (g *transcriptGraph) forkPoint(id string) (string, error) {
	target, err := g.find(id)
	if err != nil {
		return "", err
	}
	if !g.isPrompt(target) {
		return g.entries[target].UUID, nil
	}
	// The turn continues along the latest branch below the prompt.
	below := map[int]bool{target: true}
	descends := func(i int) bool {
		var path []int
		result := false
		for j := i; j >= 0 && len(path) <= len(g.entries); j = g.parent(j) {
			if v, ok := below[j]; ok {
				result = v
				break
			}
			path = append(path, j)
		}
		for _, j := range path {
			below[j] = result
		}
		return result
	}
	leaf := target
	for i := len(g.entries) - 1; i > target; i-- {
		e := g.entries[i]
		if (e.Type == "user" || e.Type == "assistant") && !e.IsSidechain && descends(i) {
			leaf = i
			break
		}
	}
	chain := g.chain(leaf)
	pos := slices.Index(chain, target)
	for k := pos + 1; k < len(chain); k++ {
		if g.isPrompt(chain[k]) {
			return g.entries[chain[k-1]].UUID, nil
		}
	}
	return g.entries[chain[len(chain)-1]].UUID, nil
}
