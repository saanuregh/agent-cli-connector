// Command orchestrator splits a question across several agents on
// different harnesses and merges their findings.
//
//	go run ./examples/orchestrator "How are errors handled across this repository, and where is it inconsistent?"
//	go run ./examples/orchestrator -planner codex -workers claude,pi,opencode -max 3 "Map the public API"
//
// It runs in three steps:
//
//  1. A planner breaks the task into independent subtasks, answering with
//     structured output.
//  2. Workers, spread round-robin over the -workers providers, research
//     the subtasks in parallel. They may read but not change anything.
//  3. The planner's harness writes the final answer from their reports.
//
// Every session is ephemeral, so nothing is left in the harnesses' session
// lists.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/saanuregh/agent-cli-connector/opencode"
	"github.com/saanuregh/agent-cli-connector/pi"
)

func main() {
	planner := flag.String("planner", "claude", "provider that plans and writes the final answer")
	workers := flag.String("workers", "claude,codex", "comma-separated providers the subtasks are spread over")
	maxTasks := flag.Int("max", 4, "most subtasks to plan")
	cwd := flag.String("cwd", "", "working directory for every agent (default: this one)")
	timeout := flag.Duration("timeout", 10*time.Minute, "time limit for each worker")
	flag.Parse()
	task := strings.Join(flag.Args(), " ")
	if task == "" {
		fmt.Fprintln(os.Stderr, "usage: orchestrator [flags] <task>")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	o := &orchestrator{
		registry: agent.NewRegistry(claude.New(claude.Options{}), codex.New(codex.Options{}), pi.New(pi.Options{}), opencode.New(opencode.Options{})),
		cwd:      *cwd,
	}
	names := splitList(*workers)
	if len(names) == 0 || *maxTasks < 1 {
		fmt.Fprintln(os.Stderr, "-workers needs a provider and -max at least 1")
		os.Exit(2)
	}
	if err := o.run(ctx, task, *planner, names, *maxTasks, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type orchestrator struct {
	registry *agent.Registry
	cwd      string
	outMu    sync.Mutex
}

type subtask struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

// report is what one worker found.
type report struct {
	subtask
	provider string
	result   *agent.TurnResult
	err      error
}

var planSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"subtasks": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"title": {"type": "string"},
					"prompt": {"type": "string"}
				},
				"required": ["title", "prompt"],
				"additionalProperties": false
			}
		}
	},
	"required": ["subtasks"],
	"additionalProperties": false
}`)

func (o *orchestrator) run(ctx context.Context, task, planner string, workers []string, maxTasks int, timeout time.Duration) error {
	o.logf("planning on %s", planner)
	plan, err := o.plan(ctx, planner, task, maxTasks)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	for i, s := range plan {
		o.logf("  %d. %s → %s", i+1, s.Title, workers[i%len(workers)])
	}

	reports := make([]report, len(plan))
	var wg sync.WaitGroup
	for i, s := range plan {
		reports[i] = report{subtask: s, provider: workers[i%len(workers)]}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			reports[i].result, reports[i].err = o.work(ctx, i+1, reports[i].provider, task, s)
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	o.logf("writing the answer on %s", planner)
	final, err := o.synthesize(ctx, planner, task, reports)
	o.summary(reports, final)
	return err
}

// plan asks for subtasks as structured output. A harness without
// structured output ignores the schema, so its plain-text reply is parsed
// as JSON instead.
func (o *orchestrator) plan(ctx context.Context, provider, task string, maxTasks int) ([]subtask, error) {
	cfg := o.readOnly(provider)
	in := agent.Text(fmt.Sprintf("Split this task into at most %d independent research subtasks that separate agents can do in parallel "+
		"by reading the files in the working directory. Each prompt must stand alone, because its agent sees nothing else. "+
		`Answer with JSON only: {"subtasks": [{"title": "...", "prompt": "..."}]}.`+"\n\nTask: %s", maxTasks, task))
	cfg.OutputSchema = planSchema
	sess, err := o.start(ctx, provider, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.Run(ctx, in)
	if err != nil {
		return nil, err
	}
	answer := res.StructuredOutput
	if len(answer) == 0 {
		answer = json.RawMessage(res.Text)
	}
	var plan struct{ Subtasks []subtask }
	if err := json.Unmarshal(answer, &plan); err != nil {
		return nil, fmt.Errorf("the plan is not the requested JSON: %w", err)
	}
	if len(plan.Subtasks) == 0 {
		return nil, errors.New("the planner returned no subtasks")
	}
	return plan.Subtasks[:min(len(plan.Subtasks), maxTasks)], nil
}

// work runs one subtask and prints the worker's tool calls as they start.
func (o *orchestrator) work(ctx context.Context, n int, provider, task string, s subtask) (*agent.TurnResult, error) {
	sess, err := o.start(ctx, provider, o.readOnly(provider))
	if err != nil {
		o.logf("[%d %s] could not start: %v", n, provider, err)
		return nil, err
	}
	defer func() { _ = sess.Close() }()

	turn, err := sess.Send(ctx, agent.Text(fmt.Sprintf("You are one of several agents working on: %s\n\nYour part: %s\n\n"+
		"Do not change any file. Report concrete findings with file paths, briefly.", task, s.Prompt)))
	if err != nil {
		return nil, err
	}
	for ev := range turn.Events() {
		if ev, ok := ev.(*agent.ItemStarted); ok && ev.Item.Tool != nil && ev.Item.ParentID == "" {
			t := ev.Item.Tool
			o.logf("[%d %s] %s %s", n, provider, t.Kind, oneLine(cmp.Or(t.Command, t.Path, t.Query, t.URL, t.Name), 70))
		}
	}
	res, err := turn.Wait(ctx)
	if res != nil {
		o.logf("[%d %s] %s in %s", n, provider, res.Status, res.Duration.Round(time.Second))
	}
	return res, err
}

// synthesize merges the reports and streams the answer to stdout.
func (o *orchestrator) synthesize(ctx context.Context, provider, task string, reports []report) (*agent.TurnResult, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Several agents researched parts of this task: %s\n\nTheir reports follow. Write the final answer: merge what they found, "+
		"resolve contradictions, and say what is still unknown. Do not redo their research.\n", task)
	for i, r := range reports {
		fmt.Fprintf(&b, "\n## Report %d: %s (%s)\n", i+1, r.Title, r.provider)
		switch {
		case r.result != nil && r.result.Text != "":
			b.WriteString(r.result.Text)
		case r.err != nil:
			fmt.Fprintf(&b, "The agent failed: %v", r.err)
		default:
			b.WriteString("The agent returned nothing.")
		}
		b.WriteString("\n")
	}

	sess, err := o.start(ctx, provider, o.readOnly(provider))
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	turn, err := sess.Send(ctx, agent.Text(b.String()))
	if err != nil {
		return nil, err
	}
	fmt.Println()
	for ev := range turn.Events() {
		if d, ok := ev.(*agent.TextDelta); ok && d.ParentID == "" {
			fmt.Print(d.Delta)
		}
	}
	fmt.Println()
	return turn.Wait(ctx)
}

// readOnly returns a config that keeps an agent from changing anything:
// the harness's read-only mode where it has one, and for the rest a
// handler that refuses every tool call the harness asks about.
func (o *orchestrator) readOnly(provider string) agent.Config {
	cfg := agent.Config{
		Cwd:       o.cwd,
		Ephemeral: true,
		Handlers: agent.Handlers{
			Permission: func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
				return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "You may only read. Report what you would change instead."}
			},
		},
	}
	if p, ok := o.registry.Get(provider); ok && slices.Contains(p.Capabilities().PermissionModes, agent.PermissionReadOnly) {
		cfg.PermissionMode = agent.PermissionReadOnly
	}
	return cfg
}

func (o *orchestrator) summary(reports []report, final *agent.TurnResult) {
	var total agent.Usage
	o.logf("")
	for i, r := range reports {
		status := "failed: " + fmt.Sprint(r.err)
		if r.result != nil {
			status = fmt.Sprintf("%s, %d in / %d out tokens, %s", r.result.Status, r.result.Usage.InputTokens, r.result.Usage.OutputTokens, r.result.Duration.Round(time.Second))
			total.Add(r.result.Usage)
		}
		o.logf("worker %d (%s) %s: %s", i+1, r.provider, r.Title, status)
	}
	if final != nil {
		total.Add(final.Usage)
	}
	line := fmt.Sprintf("total %d in / %d out tokens", total.InputTokens, total.OutputTokens)
	if total.CostUSD > 0 {
		line += fmt.Sprintf(", $%.4f where reported", total.CostUSD)
	}
	o.logf("%s", line)
}

// start starts a session that is followed only through its turns' events.
// It still reads Session.Events, so they do not pile up unread.
func (o *orchestrator) start(ctx context.Context, provider string, cfg agent.Config) (agent.Session, error) {
	sess, err := o.registry.Start(ctx, provider, cfg)
	if err != nil {
		return nil, err
	}
	go func() {
		for range sess.Events() {
		}
	}()
	return sess, nil
}

func (o *orchestrator) logf(format string, args ...any) {
	o.outMu.Lock()
	defer o.outMu.Unlock()
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func oneLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
