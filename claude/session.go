package claude

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// Session is a live Claude Code session. It implements agent.Session and
// adds Claude-specific controls.
type Session struct {
	provider *Provider
	cfg      agent.Config
	opts     SessionOptions
	rt       *providerkit.Runtime
	tools    *mcpserver.Server
	// tasks holds the IDs of tasks reported in TaskUpdated, for StopTask.
	tasks sync.Map

	// procMu guards proc and tr, which Rewind replaces.
	procMu sync.Mutex
	proc   *cliProcess
	tr     *translator
	// swapMu is held for writing while Rewind replaces the process, so
	// Send waits for the new one.
	swapMu sync.RWMutex

	hookMu    sync.Mutex
	hooks     map[string]hookRef
	nextID    atomic.Uint64
	closing   atomic.Bool
	closeOnce sync.Once

	ctrlMu   sync.Mutex
	waiters  map[string]chan controlResponse
	inflight map[string]context.CancelFunc
	// seen holds every CLI control request ID delivered to the host, so a
	// prompt listed again in an initialize response is not asked twice.
	seen map[string]bool
	// origins maps a CLI control request to the process that sent it, so
	// the answer goes back to that process.
	origins map[string]*cliProcess

	infoMu sync.Mutex
	info   agent.SessionInfo
	init   *InitializeResponse
	// capabilities is the CLI's protocol capability list from the latest
	// initialize response or system/init message.
	capabilities []string

	interruptPending atomic.Bool
	// spare marks a session launched by Prewarm with --await-claim.
	spare bool
	// turnItems records the items of each turn for Rewind.
	turnItems turnLog
}

// turnLog records turn starts and items in emission order, so a rewind can
// report every item it drops: the target turn's, later turns', and items a
// running turn produced after a steered message the rewind targets.
type turnLog struct {
	mu  sync.Mutex
	seq []logEntry
}

// logEntry is a turn start (item empty) or an item of a turn.
type logEntry struct {
	turn, item string
}

func (l *turnLog) add(turnID, itemID string) {
	if itemID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if turnID != "" && l.startLocked(turnID) < 0 {
		l.seq = append(l.seq, logEntry{turn: turnID})
	}
	if !slices.ContainsFunc(l.seq, func(e logEntry) bool { return e.item == itemID }) {
		l.seq = append(l.seq, logEntry{turn: turnID, item: itemID})
	}
}

func (l *turnLog) startLocked(turnID string) int {
	return slices.IndexFunc(l.seq, func(e logEntry) bool { return e.item == "" && e.turn == turnID })
}

// turnOf returns the turn that produced itemID, or "".
func (l *turnLog) turnOf(itemID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startLocked(itemID) >= 0 {
		return itemID
	}
	for _, e := range l.seq {
		if e.item == itemID {
			return e.turn
		}
	}
	return ""
}

// drop forgets turnID and everything recorded after it, and returns the
// turns started from then on and every dropped item ID, the turn IDs (user
// message items) included. An unknown turnID drops nothing.
func (l *turnLog) drop(turnID string) (turns, ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.startLocked(turnID)
	if i < 0 {
		return nil, nil
	}
	for _, e := range l.seq[i:] {
		id := e.item
		if id == "" {
			turns = append(turns, e.turn)
			id = e.turn
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	l.seq = l.seq[:i]
	return turns, ids
}

// first returns the oldest recorded turn, or "".
func (l *turnLog) first() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.seq {
		if e.item == "" {
			return e.turn
		}
	}
	return ""
}

// reset forgets every turn.
func (l *turnLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq = nil
}

// cliProcess is one run of the CLI. Rewind retires it and starts another.
type cliProcess struct {
	*providerkit.Process
	// done is closed after the read loop ended.
	done    chan struct{}
	retired atomic.Bool
	// state is processAdopted, or processProvisional while Rewind starts
	// the process and decides whether to keep it; processOrphaned means it
	// exited while provisional, so its read loop left ending the session
	// to Rewind.
	state atomic.Int32
}

// cliProcess states.
const (
	processAdopted int32 = iota
	processProvisional
	processOrphaned
)

var errRestarted = errors.New("claude: the CLI process was restarted by Rewind")

type hookRef struct {
	event HookEvent
	fn    HookFunc
}

var (
	_ agent.Session     = (*Session)(nil)
	_ agent.Titler      = (*Session)(nil)
	_ agent.Compacter   = (*Session)(nil)
	_ agent.Forker      = (*Session)(nil)
	_ agent.TaskStopper = (*Session)(nil)
	_ agent.Rewinder    = (*Session)(nil)
)

// startSession launches a session. A spare session (Prewarm) is launched
// with --await-claim and is not started: Spare.Claim starts it.
func (p *Provider) startSession(ctx context.Context, cfg agent.Config, opts SessionOptions, spare bool) (*Session, error) {
	if cfg.PermissionMode == agent.PermissionReadOnly {
		return nil, providerkit.Unsupported(Name, "permission mode readOnly")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
		if len(cfg.MCPServers[name].EnabledTools) > 0 {
			return nil, providerkit.Unsupported(Name, "MCPServers["+name+"].EnabledTools")
		}
	}
	if cfg.Fork && cfg.Resume == "" && !opts.Continue {
		return nil, errors.New("claude: Fork requires Resume")
	}
	if cfg.SessionID != "" && !isUUID(cfg.SessionID) {
		return nil, errors.New("claude: SessionID must be a UUID")
	}
	if opts.Continue && cfg.Resume == "" {
		latest, err := latestSessionID(p.configDir(cfg), cfg.Cwd)
		if err == nil && latest != "" {
			cfg.Resume = latest
		}
	}

	if cfg.ForkAtTurn != "" {
		if !cfg.Fork || cfg.Resume == "" {
			return nil, errors.New("claude: ForkAtTurn requires Fork and Resume")
		}
		if opts.ResumeSessionAt == "" {
			at, err := forkPoint(ctx, p.configDir(cfg), cfg.Resume, cfg.Cwd, cfg.ForkAtTurn)
			if err != nil {
				return nil, err
			}
			opts.ResumeSessionAt = at
		}
	}

	s := &Session{
		provider: p,
		cfg:      cfg,
		opts:     opts,
		rt:       providerkit.NewRuntime(Name, cfg),
		hooks:    make(map[string]hookRef),
		waiters:  make(map[string]chan controlResponse),
		inflight: make(map[string]context.CancelFunc),
		origins:  make(map[string]*cliProcess),
		seen:     make(map[string]bool),
		spare:    spare,
	}
	if len(cfg.Tools) > 0 {
		s.tools = mcpserver.New(s.toolServerName(), "1.0.0", cfg.Tools)
	}

	sessionID := cfg.SessionID
	switch {
	case cfg.Resume != "" && !cfg.Fork:
		sessionID = cfg.Resume
	case sessionID == "":
		sessionID = providerutil.NewUUID()
	}
	s.rt.SetSessionID(sessionID)
	s.tr = newTranslator(s)

	init, err := s.launch(ctx, cfg, opts, sessionID, false)
	if err != nil {
		s.shutdown()
		return nil, err
	}
	if needsApproval(cfg.Tools) && versionBefore(init.Version, minToolPolicyVersion) {
		s.shutdown()
		return nil, fmt.Errorf("claude: host tools with NeedsApproval need Claude Code %s or newer to be enforced (found %s)", minToolPolicyVersion, init.Version)
	}

	s.infoMu.Lock()
	s.init = init
	s.info = agent.SessionInfo{
		ID:             sessionID,
		Provider:       Name,
		Cwd:            cfg.Cwd,
		Model:          cmp.Or(cfg.Model, init.defaultModel()),
		Effort:         cmp.Or(cfg.Effort, init.appliedEffort),
		PermissionMode: cmp.Or(cfg.PermissionMode, agent.PermissionMode(init.CurrentPermissionMode)),
		Title:          cfg.Title,
		Version:        init.Version,
		Commands:       init.slashCommands(),
		Raw:            init.Raw,
	}
	s.infoMu.Unlock()

	if !spare {
		s.rt.Started(&agent.SessionStarted{EventMeta: agent.EventMeta{Raw: init.Raw}, Info: s.Info(), Resumed: cfg.Resume != ""})
	}
	return s, nil
}

// appliedEffort returns the effort the CLI applies to the current model,
// or "" when its settings do not say.
func (s *Session) appliedEffort(ctx context.Context) agent.Effort {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := s.Control(ctx, "get_settings", nil)
	if err != nil {
		return ""
	}
	var f struct {
		Applied struct {
			Effort string `json:"effort"`
		} `json:"applied"`
	}
	_ = json.Unmarshal(raw, &f)
	return agent.Effort(f.Applied.Effort)
}

// launch starts a CLI process, makes it the session's process, and runs
// the initialize handshake. It then registers cfg.MCPServers through
// mcp_set_servers when mcpViaControl says so.
//
// A provisional process does not end the session when it exits; Rewind
// uses one to try a restart and adopts it once the restart worked.
func (s *Session) launch(ctx context.Context, cfg agent.Config, opts SessionOptions, sessionID string, provisional bool) (*InitializeResponse, error) {
	args, err := s.buildArgs(cfg, opts, sessionID)
	if err != nil {
		return nil, err
	}
	proc, err := providerkit.StartProcess(ctx, providerkit.ProcessOptions{
		Provider: Name,
		Spawner:  s.provider.opts.Spawner,
		Command: agent.Command{
			Path:     s.provider.path(),
			Args:     args,
			Env:      s.buildEnv(),
			UnsetEnv: slices.Concat(parentSessionEnv, sdkUnsetEnv, cfg.UnsetEnv),
			Dir:      cfg.Cwd,
		},
		Logger:  s.rt.Logger(),
		WireTap: cfg.WireTap,
	})
	if err != nil {
		return nil, err
	}
	p := &cliProcess{Process: proc, done: make(chan struct{})}
	if provisional {
		p.state.Store(processProvisional)
	}
	s.procMu.Lock()
	if s.closing.Load() {
		s.procMu.Unlock()
		// The process is reaped only after its output is read.
		go func() { _ = proc.ReadLines(func([]byte) {}) }()
		proc.Stop(s.provider.shutdownGrace())
		return nil, s.rt.ClosedErr()
	}
	s.proc = p
	s.procMu.Unlock()
	go s.readLoop(p)

	startCtx, cancel := context.WithTimeout(ctx, s.provider.startupTimeout())
	defer cancel()
	raw, err := s.Control(startCtx, "initialize", s.initializeRequest())
	if err != nil {
		exitErr := p.ExitError()
		var ee *agent.ExitError
		if s.translator().sessionMissing.Load() || errors.As(exitErr, &ee) && strings.Contains(ee.Stderr, noConversationText) {
			return nil, fmt.Errorf("claude: resume %s: %w", cfg.Resume, agent.ErrSessionNotFound)
		}
		if s.translator().dropsTurnRejected.Load() || errors.As(exitErr, &ee) && strings.Contains(ee.Stderr, dropsTurnRejectedText) {
			return nil, errDropsTurnRejected
		}
		if exitErr != nil {
			return nil, fmt.Errorf("claude: initialize: %w", exitErr)
		}
		return nil, fmt.Errorf("claude: initialize: %w", err)
	}
	init, err := s.decodeInitialize(raw)
	if err != nil {
		return nil, fmt.Errorf("claude: decode initialize response: %w", err)
	}
	if !opts.skipAppliedEffort {
		// The CLI reports its effort only when a turn starts; its settings
		// already hold the level it applies.
		init.appliedEffort = s.appliedEffort(startCtx)
	}
	if mcpViaControl(cfg, opts) && len(mcpConfig(cfg.MCPServers)) > 0 {
		res, err := s.SetMCPServers(startCtx, cfg.MCPServers)
		if err != nil {
			return nil, fmt.Errorf("claude: register MCP servers: %w", err)
		}
		for _, name := range slices.Sorted(maps.Keys(res.Errors)) {
			s.rt.Emit(&agent.Notice{Level: agent.NoticeWarning, Code: "mcp_server_failed", Message: name + ": " + res.Errors[name]})
		}
	}
	return init, nil
}

// adopt makes a provisional process an ordinary one, so its exit ends the
// session. It ends the session itself when the process already exited.
func (s *Session) adopt(p *cliProcess) {
	if p.state.CompareAndSwap(processProvisional, processAdopted) {
		return
	}
	if p.state.Load() == processOrphaned && !s.closing.Load() {
		s.translator().failRunningTasks()
		s.rt.Finish(p.ExitError())
	}
}

// current returns the running CLI process.
func (s *Session) current() *cliProcess {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	return s.proc
}

func (s *Session) translator() *translator {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	return s.tr
}

// write sends one message to the running CLI process.
func (s *Session) write(v any) error {
	p := s.current()
	if p == nil {
		return s.rt.ClosedErr()
	}
	return p.WriteJSON(v)
}

func (s *Session) toolServerName() string {
	return cmp.Or(s.opts.ToolServerName, "host")
}

// ID returns the Claude session ID.
func (s *Session) ID() string { return s.rt.SessionID() }

// Provider returns "claude".
func (s *Session) Provider() string { return Name }

// Info returns the latest session metadata.
func (s *Session) Info() agent.SessionInfo {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	info := s.info
	info.ID = s.rt.SessionID()
	return info
}

// Capabilities reports the supported uniform features, without streaming
// under SessionOptions.NoPartialMessages and without permission requests
// under NoPermissionPrompts or PermissionPromptTool.
func (s *Session) Capabilities() agent.Capabilities {
	caps := s.provider.Capabilities()
	if s.opts.NoPartialMessages {
		caps.Streaming = false
		caps.ReasoningStream = false
	}
	if s.opts.NoPermissionPrompts || s.opts.PermissionPromptTool != "" {
		caps.PermissionPrompt = false
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

// CLICapabilities returns the protocol capabilities the CLI reports, such
// as "interrupt_cancel_queued_v1", from the initialize response and the
// latest system/init message. It is empty for CLIs that predate the list.
func (s *Session) CLICapabilities() []string {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	return slices.Clone(s.capabilities)
}

func (s *Session) setCapabilities(caps []string) {
	if caps == nil {
		return
	}
	s.infoMu.Lock()
	s.capabilities = slices.Clone(caps)
	s.infoMu.Unlock()
}

// emit queues ev and records the items each turn produces for Rewind. The
// runtime stamps the turn before the item is recorded.
func (s *Session) emit(ev agent.Event) {
	var item *agent.Item
	switch ev := ev.(type) {
	case *agent.ItemStarted:
		item = &ev.Item
	case *agent.ItemUpdated:
		item = &ev.Item
	case *agent.ItemCompleted:
		item = &ev.Item
	}
	if item != nil {
		// Items carry their turn, as history items do.
		m := ev.Meta()
		m.TurnID = cmp.Or(m.TurnID, item.TurnID, s.eventTurn())
		item.TurnID = m.TurnID
	}
	s.rt.Emit(ev)
	if t, ok := ev.(*agent.TaskUpdated); ok {
		s.tasks.Store(t.Task.ID, true)
	}
	if item != nil {
		s.turnItems.add(ev.Meta().TurnID, item.ID)
	}
}

// eventTurn returns the turn the runtime stamps on an event without one:
// the running turn, else the oldest unfinished one.
func (s *Session) eventTurn() string {
	if id, _ := s.rt.Running(); id != "" {
		return id
	}
	return s.rt.CurrentTurnID()
}

// Initialization returns the initialize response: commands, models,
// agents, account, and output styles.
func (s *Session) Initialization() *InitializeResponse {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	return s.init
}

// Run sends input and waits for the turn to finish.
func (s *Session) Run(ctx context.Context, in agent.Input) (*agent.TurnResult, error) {
	return providerkit.Run(ctx, s, in)
}

// Send submits input. While a turn runs, DeliverQueue sends the message
// with priority "later", DeliverSteer with "next" (folded into the running
// turn at the next tool boundary), and DeliverInterrupt with "now".
func (s *Session) Send(ctx context.Context, in agent.Input) (*agent.Turn, error) {
	s.swapMu.RLock()
	defer s.swapMu.RUnlock()
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	if len(in.OutputSchema) > 0 {
		return nil, providerkit.Unsupported(Name, "per-turn output schema (use agent.Config.OutputSchema)")
	}
	if err := providerkit.CheckInput(Name, in); err != nil {
		return nil, err
	}
	topts, err := providerkit.TurnOptions[TurnOptions](Name, in.ProviderOptions)
	if err != nil {
		return nil, err
	}
	busy := s.rt.Busy()
	queued := busy && (topts.Priority == "later" || topts.Priority == "" && (in.Delivery == "" || in.Delivery == agent.DeliverQueue))
	if queued && (in.Model != "" || in.Effort != "") {
		// The CLI keeps the queue, so the switch would reach the running
		// turn rather than wait for this input.
		return nil, providerkit.Unsupported(Name, "Input.Model or Input.Effort on input queued behind a running turn")
	}
	if in.Model != "" {
		if err := s.SetModel(ctx, in.Model); err != nil {
			return nil, err
		}
	}
	if in.Effort != "" {
		if err := s.SetEffort(ctx, in.Effort); err != nil {
			return nil, err
		}
	}
	content, err := encodeContent(in.Parts)
	if err != nil {
		return nil, err
	}
	// A steered message joins the running turn; the CLI lists its uuid in
	// that turn's result, which completes this handle too.
	uuid, turn, err := s.newTurn(in.ClientID)
	if err != nil {
		return nil, err
	}
	msg := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "",
		"uuid":               uuid,
	}
	switch {
	case topts.Priority != "":
		msg["priority"] = topts.Priority
	case busy && in.Delivery == agent.DeliverSteer:
		msg["priority"] = "next"
	case busy && in.Delivery == agent.DeliverInterrupt:
		msg["priority"] = "now"
	case busy:
		msg["priority"] = "later"
	}
	if topts.NoQuery {
		msg["shouldQuery"] = false
	}
	if topts.Verbatim || s.opts.VerbatimPrompts {
		msg["client_composed"] = true
	}
	if len(topts.PastedContent) > 0 {
		msg["pasted_content"] = topts.PastedContent
	}
	if topts.Origin != nil {
		msg["origin"] = topts.Origin
	}
	maps.Copy(msg, topts.Extra)

	// The item goes out before the write: the CLI can finish the turn
	// before Send returns, and the turn's items must include the message.
	s.emit(&agent.ItemCompleted{
		EventMeta: agent.EventMeta{TurnID: uuid},
		Item: agent.Item{
			ID:       uuid,
			Kind:     agent.ItemUserMessage,
			Status:   agent.ItemDone,
			Text:     in.PlainText(),
			ClientID: in.ClientID,
		},
	})
	if err := s.write(msg); err != nil {
		s.emit(&agent.ItemsRemoved{EventMeta: agent.EventMeta{TurnID: uuid}, IDs: []string{uuid}})
		s.rt.AbandonTurn(uuid, err)
		return nil, err
	}
	if topts.NoQuery {
		// The message joins the transcript without starting a turn.
		s.rt.FinishTurn(uuid, &agent.TurnResult{Status: agent.TurnSucceeded, StopReason: "no_query"})
	}
	return turn, nil
}

// newTurn registers the turn for a user message and returns its message
// uuid and handle. A ClientID becomes the uuid unless a running or
// recently ended turn already has it, such as on a retry: that turn's
// handle must not answer the new message.
func (s *Session) newTurn(clientID string) (string, *agent.Turn, error) {
	if isUUID(clientID) && !s.rt.HasTurn(clientID) {
		turn, err := s.rt.NewTurn(clientID)
		if err != nil {
			return "", nil, err
		}
		// The runtime hands back a recently ended turn's handle for its ID.
		select {
		case <-turn.Done():
		default:
			return clientID, turn, nil
		}
	}
	uuid := providerutil.NewUUID()
	turn, err := s.rt.NewTurn(uuid)
	if err != nil {
		return "", nil, err
	}
	return uuid, turn, nil
}

// SendMessage writes a raw stream-json user message, for message fields
// this package does not model. The message is sent as is; no turn handle
// is created.
func (s *Session) SendMessage(msg map[string]any) error {
	if _, ok := msg["type"]; !ok {
		msg["type"] = "user"
	}
	return s.write(msg)
}

// imageMediaTypes are the image types the Claude API accepts.
var imageMediaTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// encodeContent renders input parts as message content blocks. The CLI
// expands a slash command only in the last text block, so a skill part
// becomes a final "/name args" block whose arguments are the input's text
// parts; the other blocks keep their order before it. Likewise a text part
// starting with "/name" is moved after the attachments that follow it.
func encodeContent(parts []agent.Part) ([]map[string]any, error) {
	skill := slices.IndexFunc(parts, func(p agent.Part) bool { return p.Type == agent.PartSkill })
	var args []string
	out := make([]map[string]any, 0, len(parts))
	for i, p := range parts {
		switch p.Type {
		case agent.PartText:
			if skill >= 0 {
				if t := strings.TrimSpace(p.Text); t != "" {
					args = append(args, t)
				}
				continue
			}
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case agent.PartImage:
			if p.URL != "" {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": p.URL}})
				continue
			}
			data, mediaType, err := providerkit.ReadPart(p.Data, p.MediaType, p.Path)
			if err != nil {
				return nil, fmt.Errorf("claude: image %s: %w", p.Path, err)
			}
			if !slices.Contains(imageMediaTypes, mediaType) {
				return nil, fmt.Errorf("claude: image media type %q is not supported; use one of %s", mediaType, strings.Join(imageMediaTypes, ", "))
			}
			out = append(out, map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": mediaType, "data": base64.StdEncoding.EncodeToString(data),
			}})
		case agent.PartFile:
			mediaType := p.MediaType
			if mediaType == "" && p.Path != "" {
				mediaType = providerutil.MediaType(p.Path)
			}
			if mediaType == "application/pdf" || len(p.Data) > 0 {
				data, mt, err := providerkit.ReadPart(p.Data, mediaType, p.Path)
				if err != nil {
					return nil, fmt.Errorf("claude: file %s: %w", p.Path, err)
				}
				out = append(out, map[string]any{"type": "document", "source": map[string]any{
					"type": "base64", "media_type": mt, "data": base64.StdEncoding.EncodeToString(data),
				}})
				continue
			}
			out = append(out, map[string]any{"type": "text", "text": "@" + p.Path})
		case agent.PartSkill:
			if i != skill {
				out = append(out, map[string]any{"type": "text", "text": "/" + p.Name})
			}
		case agent.PartMention:
			out = append(out, map[string]any{"type": "text", "text": "/" + p.Name})
		default:
			return nil, providerkit.Unsupported(Name, "input part "+string(p.Type))
		}
	}
	if skill >= 0 {
		out = append(out, map[string]any{"type": "text", "text": strings.TrimSpace("/" + parts[skill].Name + " " + strings.Join(args, "\n\n"))})
	} else {
		out = commandLast(out)
	}
	if len(out) == 0 {
		return nil, errors.New("claude: input has no content")
	}
	return out, nil
}

// commandLast moves the first text block that starts with a slash command
// to the end when attachments follow it.
func commandLast(blocks []map[string]any) []map[string]any {
	i := slices.IndexFunc(blocks, func(b map[string]any) bool {
		text, _ := b["text"].(string)
		return b["type"] == "text" && isSlashCommand(text)
	})
	if i < 0 || !slices.ContainsFunc(blocks[i+1:], func(b map[string]any) bool { return b["type"] != "text" }) {
		return blocks
	}
	cmd := blocks[i]
	return append(slices.Delete(blocks, i, i+1), cmd)
}

// isSlashCommand reports text of the form "/name ...".
func isSlashCommand(text string) bool {
	rest, ok := strings.CutPrefix(strings.TrimLeft(text, " \t\n"), "/")
	if !ok || rest == "" {
		return false
	}
	c := rest[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Interrupt stops the running turn. Messages queued behind it still run
// afterwards; InterruptWithOptions can cancel them too. Background tasks
// keep running unless SessionOptions.NoPerTaskStopAffordance is set. With
// no turn running or queued it sends nothing.
func (s *Session) Interrupt(ctx context.Context) error {
	if !s.rt.Busy() {
		return nil
	}
	_, err := s.InterruptWithOptions(ctx, false)
	return err
}

// InterruptTurn stops one turn without touching the others. A turn whose
// message the CLI still queues is removed from the queue and ends with
// agent.ErrCancelled; a running turn is interrupted, and messages queued
// behind it stay queued. A turn that already ended is not an error.
func (s *Session) InterruptTurn(ctx context.Context, id string) error {
	if !s.rt.HasTurn(id) {
		return nil
	}
	// The CLI does not cancel a message it already started, so an
	// uncancelled turn that is still tracked is running. A CLI without the
	// cancel request answers with an error; interrupting is then the best
	// remaining choice.
	cancelled, err := s.CancelQueuedMessage(ctx, id)
	if _, refused := errors.AsType[*agent.ProtocolError](err); err != nil && !refused {
		return err
	}
	if cancelled || !s.rt.HasTurn(id) {
		return nil
	}
	_, err = s.InterruptWithOptions(ctx, false)
	return err
}

// InterruptReceipt lists queued messages that survive an interrupt.
type InterruptReceipt struct {
	StillQueued []string `json:"still_queued"`
	Cancelled   []string `json:"cancelled"`
}

// InterruptWithOptions stops the running turn. With cancelQueued, queued
// messages are cancelled too.
func (s *Session) InterruptWithOptions(ctx context.Context, cancelQueued bool) (*InterruptReceipt, error) {
	payload := map[string]any{}
	if cancelQueued {
		payload["cancel_queued"] = true
	}
	busy := s.rt.Busy()
	if busy {
		s.interruptPending.Store(true)
	}
	raw, err := s.Control(ctx, "interrupt", payload)
	if err != nil {
		s.interruptPending.Store(false)
		return nil, err
	}
	var r InterruptReceipt
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r)
	}
	for _, id := range r.Cancelled {
		s.rt.AbandonTurn(id, agent.ErrCancelled)
	}
	return &r, nil
}

// SetModel switches the model. An empty model restores the default.
func (s *Session) SetModel(ctx context.Context, model string) error {
	payload := map[string]any{}
	if model != "" {
		payload["model"] = model
	}
	if _, err := s.Control(ctx, "set_model", payload); err != nil {
		return err
	}
	s.updateInfo(func(i *agent.SessionInfo) { i.Model = model })
	return nil
}

// SetEffort changes the reasoning effort through the flag settings layer.
func (s *Session) SetEffort(ctx context.Context, effort agent.Effort) error {
	var level any = string(nativeEffort(effort))
	if effort == "" {
		level = nil
	}
	if err := s.ApplyFlagSettings(ctx, map[string]any{"effortLevel": level}); err != nil {
		return err
	}
	s.updateInfo(func(i *agent.SessionInfo) { i.Effort = effort })
	return nil
}

// SetPermissionMode changes the permission mode. Switching to
// agent.PermissionBypass needs SessionOptions.AllowBypassPermissions unless
// the session started in that mode.
func (s *Session) SetPermissionMode(ctx context.Context, mode agent.PermissionMode) error {
	if mode == agent.PermissionReadOnly {
		return providerkit.Unsupported(Name, "permission mode readOnly")
	}
	if mode == agent.PermissionBypass && !s.bypassAllowed(s.cfg, s.opts) {
		return errors.New("claude: bypassPermissions needs SessionOptions.AllowBypassPermissions when the session starts in another mode")
	}
	if _, err := s.Control(ctx, "set_permission_mode", map[string]any{"mode": string(mode)}); err != nil {
		return err
	}
	s.updateInfo(func(i *agent.SessionInfo) { i.PermissionMode = mode })
	return nil
}

func (s *Session) updateInfo(change func(*agent.SessionInfo)) {
	s.infoMu.Lock()
	change(&s.info)
	info := s.info
	s.infoMu.Unlock()
	s.rt.Emit(&agent.SessionUpdated{Info: info})
}

// Close ends the session and waits for the process to exit.
func (s *Session) Close() error {
	s.shutdown()
	return nil
}

func (s *Session) shutdown() {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
		if p := s.current(); p != nil {
			p.Stop(s.provider.shutdownGrace())
			<-p.done
		}
		s.rt.Finish(nil)
		s.translator().stop()
	})
}

// Control sends a control request and returns the response payload. Use it
// for control requests this package has no method for.
func (s *Session) Control(ctx context.Context, subtype string, payload any) (json.RawMessage, error) {
	if s.rt.Finished() {
		return nil, s.rt.ClosedErr()
	}
	body := map[string]any{}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("claude: encode %s: %w", subtype, err)
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, fmt.Errorf("claude: %s payload must be an object: %w", subtype, err)
		}
	}
	body["subtype"] = subtype
	id := "req_" + strconv.FormatUint(s.nextID.Add(1), 10)
	ch := make(chan controlResponse, 1)
	s.ctrlMu.Lock()
	s.waiters[id] = ch
	s.ctrlMu.Unlock()
	defer func() {
		s.ctrlMu.Lock()
		delete(s.waiters, id)
		s.ctrlMu.Unlock()
	}()
	p := s.current()
	if p == nil {
		return nil, s.rt.ClosedErr()
	}
	if err := p.WriteJSON(map[string]any{"type": "control_request", "request_id": id, "request": body}); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if subtype == "initialize" {
			s.redeliver(p, resp)
		}
		if resp.Subtype != "success" {
			return nil, &agent.ProtocolError{Provider: Name, Method: subtype, Message: resp.Error, ErrorCode: resp.ErrorCode}
		}
		return resp.Response, nil
	case <-ctx.Done():
		_ = p.WriteJSON(map[string]any{"type": "control_cancel_request", "request_id": id})
		return nil, ctx.Err()
	case <-p.done:
		if p.retired.Load() {
			return nil, errRestarted
		}
		if err := p.ExitError(); err != nil {
			return nil, err
		}
		return nil, s.rt.ClosedErr()
	}
}

// redeliver handles the permission prompts and dialogs an initialize
// response carries again, such as ones a resumed session left open, like
// live control requests from p.
func (s *Session) redeliver(p *cliProcess, resp controlResponse) {
	for _, list := range [][]json.RawMessage{resp.PendingPermissionRequests, resp.PendingUserDialogRequests} {
		for _, raw := range list {
			var env envelope
			if json.Unmarshal(raw, &env) != nil || env.RequestID == "" {
				continue
			}
			var head controlRequestHeader
			_ = json.Unmarshal(env.Request, &head)
			if head.Subtype != "can_use_tool" && head.Subtype != "request_user_dialog" {
				continue
			}
			// A prompt already delivered, open or answered, is not asked twice.
			s.deliverControlRequest(p, env.RequestID, env.Request, raw)
		}
	}
}

// deliverControlRequest handles a control request once, recording the process
// that sent it so the answer goes back to that process.
func (s *Session) deliverControlRequest(p *cliProcess, requestID string, request json.RawMessage, line []byte) {
	s.ctrlMu.Lock()
	dup := s.seen[requestID]
	if !dup {
		s.seen[requestID] = true
		s.origins[requestID] = p
	}
	s.ctrlMu.Unlock()
	if !dup {
		go s.handleControlRequest(requestID, request, line)
	}
}

func (s *Session) readLoop(p *cliProcess) {
	defer close(p.done)
	err := p.ReadLines(func(line []byte) { s.handleLine(p, line) })
	if err != nil && !p.retired.Load() {
		s.rt.Logger().Warn("reading claude output failed", "error", err)
	}
	<-p.Exited()
	if p.retired.Load() || p.state.CompareAndSwap(processProvisional, processOrphaned) {
		return
	}
	if s.closing.Load() {
		s.rt.Finish(nil)
		return
	}
	s.translator().failRunningTasks()
	s.rt.Finish(p.ExitError())
}

func (s *Session) handleLine(p *cliProcess, line []byte) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		s.rt.Logger().Warn("ignoring malformed claude output", "error", err, "line", truncate(string(line), 200))
		return
	}
	switch env.Type {
	case "control_response":
		var resp controlResponse
		if err := json.Unmarshal(env.Response, &resp); err != nil {
			return
		}
		s.ctrlMu.Lock()
		ch := s.waiters[resp.RequestID]
		s.ctrlMu.Unlock()
		if ch != nil {
			ch <- resp
		}
	case "control_request":
		s.deliverControlRequest(p, env.RequestID, env.Request, line)
	case "control_cancel_request":
		s.ctrlMu.Lock()
		cancel := s.inflight[env.RequestID]
		delete(s.origins, env.RequestID)
		s.ctrlMu.Unlock()
		if cancel != nil {
			cancel()
		}
		s.rt.WithdrawRequest(env.RequestID)
	case "keep_alive", "transcript_mirror":
	default:
		s.translator().handle(env, line)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// respond writes the answer to a CLI-initiated control request.
func (s *Session) respond(requestID string, payload any, err error) error {
	resp := map[string]any{"request_id": requestID}
	if err != nil {
		resp["subtype"] = "error"
		resp["error"] = err.Error()
	} else {
		resp["subtype"] = "success"
		resp["response"] = payload
	}
	s.ctrlMu.Lock()
	p := s.origins[requestID]
	delete(s.origins, requestID)
	s.ctrlMu.Unlock()
	// A request withdrawn by the CLI or asked by a process Rewind
	// replaced must not be answered on the current process.
	if p == nil || p.retired.Load() {
		return errRestarted
	}
	return p.WriteJSON(map[string]any{"type": "control_response", "response": resp})
}

func (s *Session) handleControlRequest(requestID string, raw json.RawMessage, line []byte) {
	ctx, cancel := context.WithCancel(s.rt.Context())
	s.ctrlMu.Lock()
	origin := s.origins[requestID]
	if origin == nil || origin.retired.Load() {
		// Rewind replaced the asking process before this ran.
		delete(s.origins, requestID)
		s.ctrlMu.Unlock()
		cancel()
		return
	}
	s.inflight[requestID] = cancel
	s.ctrlMu.Unlock()
	defer func() {
		cancel()
		s.ctrlMu.Lock()
		delete(s.inflight, requestID)
		s.ctrlMu.Unlock()
	}()

	var head controlRequestHeader
	_ = json.Unmarshal(raw, &head)
	switch head.Subtype {
	case "can_use_tool":
		s.handlePermission(ctx, requestID, raw, line)
	case "hook_callback":
		payload, err := s.handleHook(ctx, raw)
		_ = s.respond(requestID, payload, err)
	case "mcp_message":
		payload, err := s.handleMCP(ctx, raw)
		_ = s.respond(requestID, payload, err)
	case "elicitation":
		s.handleElicitation(ctx, requestID, raw, line)
	case "request_user_dialog":
		s.handleUserDialog(ctx, requestID, raw)
	default:
		if fn := s.opts.OnControlRequest; fn != nil {
			payload, err := fn(ctx, head.Subtype, raw)
			if err == nil && payload == nil {
				payload = map[string]any{}
			}
			_ = s.respond(requestID, payload, err)
			return
		}
		_ = s.respond(requestID, nil, fmt.Errorf("unsupported control request subtype: %s", head.Subtype))
	}
}

func (s *Session) handleHook(ctx context.Context, raw json.RawMessage) (any, error) {
	var req struct {
		CallbackID string          `json:"callback_id"`
		Input      json.RawMessage `json:"input"`
		ToolUseID  string          `json:"tool_use_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	s.hookMu.Lock()
	ref, ok := s.hooks[req.CallbackID]
	s.hookMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no hook callback %q", req.CallbackID)
	}
	var in HookInput
	_ = json.Unmarshal(req.Input, &in)
	in.Raw = req.Input
	in.ToolUseIDArg = req.ToolUseID
	if in.Event == "" {
		in.Event = ref.event
	}
	out, err := ref.fn(ctx, in)
	if err != nil {
		return nil, err
	}
	return out.encode(in.Event)
}

func (s *Session) handleMCP(ctx context.Context, raw json.RawMessage) (any, error) {
	var req struct {
		ServerName string          `json:"server_name"`
		Message    json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if s.tools == nil || req.ServerName != s.tools.Name() {
		return nil, fmt.Errorf("SDK MCP server not found: %s", req.ServerName)
	}
	resp := s.tools.Handle(ctx, req.Message, s.ID())
	if resp == nil {
		resp = json.RawMessage(`{"jsonrpc":"2.0","result":{},"id":0}`)
	}
	return map[string]any{"mcp_response": resp}, nil
}

func (s *Session) handleUserDialog(ctx context.Context, requestID string, raw json.RawMessage) {
	if s.opts.OnUserDialog == nil {
		return
	}
	var req struct {
		DialogKind string          `json:"dialog_kind"`
		Payload    json.RawMessage `json:"payload"`
		ToolUseID  string          `json:"tool_use_id"`
	}
	_ = json.Unmarshal(raw, &req)
	res, err := s.opts.OnUserDialog(ctx, UserDialog{Kind: req.DialogKind, Payload: req.Payload, ToolUseID: req.ToolUseID, RequestID: requestID})
	if err == nil && res == nil {
		return
	}
	_ = s.respond(requestID, res, err)
}

// defaultStartupTimeout is used for startup when no timeout is configured.
const defaultStartupTimeout = 60 * time.Second

// minToolPolicyVersion is the first Claude Code version that honors the
// per-tool permission policies NeedsApproval relies on.
const minToolPolicyVersion = "2.1.301"

func needsApproval(tools []agent.Tool) bool {
	return slices.ContainsFunc(tools, func(t agent.Tool) bool { return t.NeedsApproval })
}

// versionBefore reports whether the dotted version v is older than min. An
// unknown version is not treated as older.
func versionBefore(v, min string) bool {
	if v == "" {
		return false
	}
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := range b {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(strings.TrimFunc(a[i], func(r rune) bool { return r < '0' || r > '9' }))
		}
		y, _ = strconv.Atoi(b[i])
		if x != y {
			return x < y
		}
	}
	return false
}
