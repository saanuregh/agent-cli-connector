// Command chat is a terminal client for exploring every provider through
// the uniform API.
//
//	go run ./examples/chat -provider claude
//	go run ./examples/chat -provider codex -prompt "Summarize this repository" -yes
//	go run ./examples/chat -provider pi
//	go run ./examples/chat -provider opencode
//
// Type a message to send it; input typed while a turn runs is queued. Lines
// starting with "/" are commands: /help lists them, including images, files,
// skills, structured output, and raw protocol lines. Flags cover the rest of
// agent.Config: -help lists them. Requests from the agent
// (permissions, questions, MCP elicitations) are printed and answered with
// commands, or with y and n for the oldest permission. Ctrl+C interrupts a
// running turn and exits when nothing runs.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func main() {
	providerName := flag.String("provider", "claude", "provider: claude, codex, pi, or opencode")
	model := flag.String("model", "", "model to use")
	effort := flag.String("effort", "", "reasoning effort, such as low or high")
	mode := flag.String("mode", string(agent.PermissionDefault), "permission mode, such as default, acceptEdits, plan, or bypassPermissions")
	prompt := flag.String("prompt", "", "send one prompt, print the reply, and exit")
	yes := flag.Bool("yes", false, "approve every tool call without asking")
	cwd := flag.String("cwd", "", "working directory")
	resume := flag.String("resume", "", "session ID to resume")
	ephemeral := flag.Bool("ephemeral", false, "ask the harness not to persist the session")
	events := flag.Bool("events", false, "print every event")
	debug := flag.Bool("debug", false, "log harness stderr")
	system := flag.String("system", "", "replace the system prompt")
	appendSystem := flag.String("append-system", "", "append to the system prompt")
	title := flag.String("title", "", "name the new session")
	sessionID := flag.String("session-id", "", "ID for the new session (Claude needs a UUID)")
	fork := flag.Bool("fork", false, "with -resume, continue in a new session and keep the original")
	forkAt := flag.String("fork-at", "", "with -fork, keep the conversation only through this turn")
	allowTools := flag.String("allow-tools", "", "comma-separated tools that run without asking")
	denyTools := flag.String("deny-tools", "", "comma-separated tools the model may not use")
	strict := flag.Bool("strict", false, "fail to start when the harness cannot honor a setting")
	buffer := flag.Int("buffer", 0, "keep at most this many unread events (0: no limit)")
	schema := flag.String("schema", "", "JSON Schema file every turn's answer must match (Config.OutputSchema)")
	wire := flag.String("wire", "", "write every protocol line to this file (- for stderr); /wire toggles")
	var mcp, dirs listFlag
	flag.Var(&mcp, "mcp", "add an MCP server: name=command args... or name=https://url (repeatable)")
	flag.Var(&dirs, "add-dir", "grant access to a directory outside the working directory (repeatable)")
	flag.Parse()

	a := &app{
		ctx:        context.Background(),
		registry:   agent.NewRegistry(claude.New(claude.Options{}), codex.New(codex.Options{}), pi.New(pi.Options{}), opencode.New(opencode.Options{})),
		provider:   *providerName,
		verbose:    *events,
		modelCache: map[string][]agent.Model{},
		cfg: agent.Config{
			Cwd:            *cwd,
			Model:          *model,
			Effort:         agent.Effort(*effort),
			PermissionMode: agent.PermissionMode(*mode),
			Ephemeral:      *ephemeral,
			Tools:          []agent.Tool{hostInfoTool()},

			SystemPrompt:          *system,
			AppendSystemPrompt:    *appendSystem,
			AllowedTools:          splitList(*allowTools),
			DisallowedTools:       splitList(*denyTools),
			AdditionalDirectories: dirs,
			Strict:                *strict,
			EventBufferLimit:      *buffer,
		},
	}
	servers, err := parseMCP(mcp)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	a.cfg.MCPServers = servers
	if err := a.setWire(*wire); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	a.cfg.WireTap = a.tap
	if *schema != "" {
		data, err := os.ReadFile(*schema)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		a.cfg.OutputSchema = data
	}
	if *debug {
		a.cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if *yes {
		a.cfg.Handlers.Permission = func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
			return agent.PermissionResponse{Decision: agent.DecisionAllow}
		}
	}

	// These describe only the first session; /new and /provider start
	// plain ones.
	start := a.cfg
	start.Resume, start.Fork, start.ForkAtTurn = *resume, *fork, *forkAt
	start.Title, start.SessionID = *title, *sessionID
	if err := a.start(a.provider, start); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	defer func() { _ = a.session().Close() }()

	if *prompt != "" {
		res, err := a.session().Run(a.ctx, agent.Text(*prompt))
		a.waitQuiet()
		if res == nil {
			fmt.Fprintln(os.Stderr, "run:", err)
			os.Exit(1)
		}
		if err != nil {
			os.Exit(1)
		}
		return
	}

	a.loop()
}

// app is the chat state: the active session and what its events reported.
type app struct {
	ctx      context.Context
	registry *agent.Registry
	cfg      agent.Config

	mu         sync.Mutex
	provider   string
	sess       agent.Session
	rendered   chan struct{} // closed when the active session's events end
	transcript *agent.Transcript
	tasks      map[string]agent.Task
	turns      []*sentTurn
	usage      agent.Usage
	verbose    bool
	reasoning  bool
	modelCache map[string][]agent.Model
	eventTypes map[string]bool // event types /events shows; nil means all

	wireOn  atomic.Bool
	wireOut io.Writer // where /wire writes; nil means stderr
}

// sentTurn is input this chat sent and the turn it became.
type sentTurn struct {
	prompt string
	turn   *agent.Turn
}

func (a *app) session() agent.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

// active returns the active provider's name, the provider, and the session.
func (a *app) active() (string, agent.Provider, agent.Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, _ := a.registry.Get(a.provider)
	return a.provider, p, a.sess
}

// busy reports whether a turn sent on the active session still runs.
func (a *app) busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.ContainsFunc(a.turns, func(t *sentTurn) bool {
		select {
		case <-t.turn.Done():
			return false
		default:
			return true
		}
	})
}

// start starts a session on provider and makes it the active one, closing
// the previous one.
func (a *app) start(provider string, cfg agent.Config) error {
	sess, err := a.registry.Start(a.ctx, provider, cfg)
	if err != nil {
		return err
	}
	a.attach(provider, sess)
	return nil
}

// attach makes sess the active session and renders its events.
func (a *app) attach(provider string, sess agent.Session) {
	a.mu.Lock()
	old := a.sess
	a.provider, a.sess = provider, sess
	a.rendered = make(chan struct{})
	a.transcript = &agent.Transcript{}
	a.tasks = map[string]agent.Task{}
	a.turns = nil
	a.usage = agent.Usage{}
	done := a.rendered
	a.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go func() {
		defer close(done)
		a.render(sess)
	}()
}

// setWire opens the -wire destination and turns the tap on.
func (a *app) setWire(path string) error {
	switch path {
	case "":
		return nil
	case "-":
	default:
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		a.wireOut = f
	}
	a.wireOn.Store(true)
	return nil
}

// tap receives every protocol line. It runs on the session's I/O
// goroutine, so it only writes.
func (a *app) tap(dir agent.WireDirection, line []byte) {
	if !a.wireOn.Load() {
		return
	}
	arrow := "<<"
	if dir == agent.WireSend {
		arrow = ">>"
	}
	if a.wireOut != nil {
		fmt.Fprintf(a.wireOut, "%s %s\n", arrow, line)
		return
	}
	a.printf("\n%s %s\n", arrow, oneLine(string(line), 300))
}

// waitTurns waits for the turns still running, so piped input gets its
// replies before the chat ends.
func (a *app) waitTurns() {
	a.mu.Lock()
	turns := slices.Clone(a.turns)
	a.mu.Unlock()
	for _, t := range turns {
		<-t.turn.Done()
	}
}

// waitQuiet waits until the events of a finished one-shot run are printed.
func (a *app) waitQuiet() {
	a.mu.Lock()
	sess, done := a.sess, a.rendered
	a.mu.Unlock()
	_ = sess.Close()
	<-done
}

// loop reads lines until stdin ends or Ctrl+C is pressed while idle.
func (a *app) loop() {
	lines := make(chan string)
	go func() {
		defer close(lines)
		in := bufio.NewScanner(os.Stdin)
		in.Buffer(make([]byte, 1<<20), 1<<20)
		for in.Scan() {
			lines <- in.Text()
		}
	}()
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	a.printf("Type a message, or /help for commands.\n")
	a.prompt()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				a.waitTurns()
				return
			}
			if quit := a.handle(strings.TrimSpace(line)); quit {
				return
			}
		case <-interrupts:
			if !a.busy() {
				a.printf("\n")
				return
			}
			a.printf("\n[interrupting]\n")
			if err := a.session().Interrupt(a.ctx); err != nil {
				a.printf("interrupt: %v\n", err)
			}
		}
	}
}

// handle runs one input line and reports whether the chat should end.
func (a *app) handle(line string) bool {
	switch {
	case line == "":
	case strings.HasPrefix(line, "//"):
		// Sent to the harness as a slash command of its own.
		a.send(agent.Text(line[1:]), line[1:])
	case strings.HasPrefix(line, "/"):
		name, args, _ := strings.Cut(line[1:], " ")
		if name == "quit" || name == "exit" {
			return true
		}
		a.command(name, strings.TrimSpace(args))
	case line == "y" && a.permissionPending():
		a.command("allow", "")
	case line == "n" && a.permissionPending():
		a.command("deny", "")
	default:
		a.send(agent.Text(line), line)
	}
	a.prompt()
	return false
}

// send submits input and tracks its turn.
func (a *app) send(in agent.Input, prompt string) {
	turn, err := a.session().Send(a.ctx, in)
	if err != nil {
		a.printf("send: %v\n", err)
		return
	}
	a.mu.Lock()
	a.turns = append(a.turns, &sentTurn{prompt: prompt, turn: turn})
	a.mu.Unlock()
}

// permissionPending reports whether y and n answer a permission request
// rather than being sent as messages.
func (a *app) permissionPending() bool {
	_, err := requestOf[*agent.PermissionRequest](a, 0)
	return err == nil
}

// printf writes a status line to stderr without interleaving with
// streamed text.
func (a *app) printf(format string, args ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintf(os.Stderr, format, args...)
}

func (a *app) prompt() { a.printf("> ") }

func (a *app) report(err error) {
	if err != nil {
		a.printf("error: %v\n", err)
	}
}

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseMCP reads -mcp values: name=https://url for an HTTP server, or
// name=command args... for a stdio one.
func parseMCP(specs []string) (map[string]agent.MCPServer, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	servers := map[string]agent.MCPServer{}
	for _, spec := range specs {
		name, target, ok := strings.Cut(spec, "=")
		fields := strings.Fields(target)
		if !ok || name == "" || len(fields) == 0 {
			return nil, fmt.Errorf("-mcp %q: want name=command args... or name=https://url", spec)
		}
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			servers[name] = agent.HTTPServer(target)
		} else {
			servers[name] = agent.StdioServer(fields[0], fields[1:]...)
		}
	}
	return servers, nil
}

// outMu orders writes to stdout and stderr.
var outMu sync.Mutex

func hostInfoTool() agent.Tool {
	type args struct{}
	t := agent.NewTool("host_info", "Returns information about the host running this chat, such as its hostname.",
		json.RawMessage(`{"type":"object","properties":{}}`),
		func(context.Context, args) (agent.ToolResult, error) {
			host, _ := os.Hostname()
			return agent.TextResult("hostname: " + host), nil
		})
	t.ReadOnly = true
	return t
}
