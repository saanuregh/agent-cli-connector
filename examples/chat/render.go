package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// render prints the events of sess until it ends.
func (a *app) render(sess agent.Session) {
	state := renderState{streamed: map[string]bool{}}
	for ev := range sess.Events() {
		a.mu.Lock()
		active := a.sess == sess
		if active {
			a.transcript.Apply(ev)
		}
		verbose, reasoning, types := a.verbose, a.reasoning, a.eventTypes
		a.mu.Unlock()
		if !active {
			continue
		}
		if verbose && (types == nil || types[strings.ToLower(agent.EventType(ev))]) {
			a.printEvent(ev)
		}
		a.show(ev, &state, reasoning)
	}
	a.mu.Lock()
	active := a.sess == sess
	a.mu.Unlock()
	if active {
		note := ""
		if err := sess.Err(); err != nil {
			note = ": " + err.Error()
		}
		a.printf("\n[session ended%s]\n", note)
	}
}

// renderState is what one session's render loop remembers between events.
type renderState struct {
	settings string          // the model, effort, and mode last shown
	streamed map[string]bool // assistant messages whose text was streamed
}

// show prints the events a person following the chat cares about.
func (a *app) show(ev agent.Event, state *renderState, reasoning bool) {
	switch ev := ev.(type) {
	case *agent.SessionStarted:
		i := ev.Info
		state.settings = settingsOf(i)
		resumed := ""
		if ev.Resumed {
			resumed = ", resumed"
		}
		a.printf("\n[%s session %s, %s%s]\n", i.Provider, i.ID, settingsOf(i), resumed)
	case *agent.SessionUpdated:
		// The harness can report a setting later, such as Claude's effort
		// when the first turn starts.
		s := settingsOf(ev.Info)
		changed := s != state.settings
		state.settings = s
		if changed {
			a.printf("\n[%s]\n", s)
		}
	case *agent.TextDelta:
		if ev.ParentID == "" {
			state.streamed[ev.ItemID] = true
			outMu.Lock()
			fmt.Print(ev.Delta)
			outMu.Unlock()
		}
	case *agent.ReasoningDelta:
		if reasoning && ev.ParentID == "" {
			a.printf("%s", ev.Delta)
		}
	case *agent.ItemStarted:
		if ev.Item.Kind == agent.ItemToolCall && ev.Item.Tool != nil {
			a.printf("\n[tool %s%s]\n", ev.Item.Tool.Name, subagentNote(ev.Item.SubagentID))
		}
	case *agent.ItemCompleted:
		if it := ev.Item; it.Kind == agent.ItemAssistantMessage && it.ParentID == "" {
			// Text that arrived without deltas, such as a harness slash
			// command's output or a session without streaming.
			streamed := state.streamed[it.ID]
			delete(state.streamed, it.ID)
			if !streamed && it.Text != "" {
				outMu.Lock()
				fmt.Println(it.Text)
				outMu.Unlock()
			}
		}
		if t := ev.Item.Tool; t != nil {
			a.printf("[tool %s %s: %s] %s\n", t.Name, ev.Item.Status, oneLine(toolSummary(t), 80), oneLine(t.Output, 120))
		}
	case *agent.PermissionRequest:
		a.printf("\n[permission] %s %s%s\n  y / n, /allow [session|always], /deny [reason], /pending\n",
			cmp.Or(ev.Title, ev.ToolName), oneLine(cmp.Or(ev.Command, string(ev.Input)), 120), subagentNote(ev.SubagentID))
	case *agent.UserInputRequest:
		a.printf("\n[question]%s %s\n", subagentNote(ev.SubagentID), ev.Title)
		for qi, q := range ev.Questions {
			a.printf("  %d. %s\n", qi+1, q.Text)
			for oi, o := range q.Options {
				a.printf("     %d) %s %s\n", oi+1, o.Label, oneLine(o.Description, 80))
			}
		}
		a.printf("  /answer <answer 1> | <answer 2> ...  (option numbers or text), /pending\n")
	case *agent.ElicitationRequest:
		a.printf("\n[elicitation] %s asks: %s\n", ev.Server, oneLine(ev.Message, 200))
		if ev.URL != "" {
			a.printf("  open %s\n", ev.URL)
		}
		if len(ev.Schema) > 0 {
			a.printf("  schema: %s\n", oneLine(string(ev.Schema), 200))
		}
		a.printf("  /accept [json], /decline\n")
	case *agent.TaskUpdated:
		a.mu.Lock()
		prev, seen := a.tasks[ev.Task.ID]
		a.tasks[ev.Task.ID] = ev.Task
		a.mu.Unlock()
		if !seen || prev.Status != ev.Task.Status {
			t := ev.Task
			a.printf("\n[task %s %s %s: %s] %s\n", t.Kind, t.ID, t.Status, oneLine(t.Description, 80), oneLine(cmp.Or(t.Output, t.Error), 120))
		}
	case *agent.PlanUpdated:
		a.printf("\n[plan]\n")
		for _, s := range ev.Steps {
			a.printf("  [%s] %s\n", s.Status, s.Text)
		}
	case *agent.Notice:
		label := string(ev.Level)
		if ev.Code != "" && ev.Code != label {
			label += " " + ev.Code
		}
		a.printf("\n[%s] %s\n", label, ev.Message)
	case *agent.Retry:
		a.printf("\n[retry %d/%d in %s] %s\n", ev.Attempt, ev.MaxAttempts, ev.Delay, oneLine(ev.Message, 120))
	case *agent.Compaction:
		if ev.Done {
			a.printf("\n[compacted %d → %d tokens]\n", ev.TokensBefore, ev.TokensAfter)
		} else {
			a.printf("\n[compacting]\n")
		}
	case *agent.GoalUpdated:
		if ev.Goal == nil {
			a.printf("\n[goal cleared]\n")
		} else {
			a.printf("\n[goal %s] %s\n", ev.Goal.Status, ev.Goal.Objective)
		}
	case *agent.PlanDelta:
		a.printf("%s", ev.Delta)
	case *agent.ItemsRemoved:
		a.printf("\n[the harness withdrew %d items]\n", len(ev.IDs))
	case *agent.ConversationReset:
		note := ""
		if ev.NewSessionID != "" {
			note = ", new session " + ev.NewSessionID
		}
		a.printf("\n[conversation reset (%s)%s]\n", cmp.Or(ev.Trigger, "unknown reason"), note)
	case *agent.RateLimitUpdated:
		for _, l := range ev.Limits {
			if l.Status != agent.RateLimitOK && l.Status != "" {
				a.printf("\n[rate limit %s %s, %.0f%% used%s]\n", l.Name, l.Status, l.UsedPercent, resetNote(l.ResetsAt))
			}
		}
	case *agent.HookRun:
		// Hooks that passed quietly are left to /events.
		if ev.Done && (ev.Output != "" || ev.Outcome != agent.HookSucceeded) {
			a.printf("\n[hook %s %s %s] %s\n", ev.Event, ev.Name, ev.Status, oneLine(ev.Output, 120))
		}
	case *agent.EventsDropped:
		a.printf("\n[%d events dropped]\n", ev.Count)
	case *agent.TurnCompleted:
		r := ev.Result
		a.mu.Lock()
		a.usage.Add(r.Usage)
		a.mu.Unlock()
		line := fmt.Sprintf("\n[turn %s %s, %d in / %d out tokens", r.TurnID, r.Status, r.Usage.InputTokens, r.Usage.OutputTokens)
		if r.Usage.CostUSD > 0 {
			line += fmt.Sprintf(", $%.4f", r.Usage.CostUSD)
		}
		line += ", " + r.Duration.Round(100*time.Millisecond).String() + "]"
		if r.Err != nil {
			line += " " + r.Err.Error()
		}
		a.printf("%s\n", line)
		if len(r.StructuredOutput) > 0 {
			a.printf("[structured output] %s\n", r.StructuredOutput)
		}
		a.prompt()
	case *agent.StatusChanged, *agent.RequestResolved, *agent.UsageUpdated, *agent.TurnStarted:
		// Shown with /events.
	}
}

// printEvent prints any event as its type and JSON. The native message is
// left out except for a RawEvent, whose content is only there.
func (a *app) printEvent(ev agent.Event) {
	var fields map[string]json.RawMessage
	data, err := json.Marshal(ev)
	if err == nil {
		err = json.Unmarshal(data, &fields)
	}
	if err != nil {
		a.printf("\n· %T (%v)\n", ev, err)
		return
	}
	if _, native := ev.(*agent.RawEvent); !native {
		delete(fields, "Raw")
	}
	delete(fields, "Provider")
	delete(fields, "SessionID")
	delete(fields, "Time")
	data, _ = json.Marshal(fields)
	a.printf("\n· %T %s\n", ev, oneLine(string(data), 300))
}

func resetNote(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return ", resets " + t.Local().Format(time.DateTime)
}

func toolSummary(t *agent.ToolCall) string {
	for _, s := range []string{t.Command, t.Path, t.Query, t.URL} {
		if s != "" {
			return s
		}
	}
	return string(t.Input)
}

func subagentNote(id string) string {
	if id == "" {
		return ""
	}
	return " (subagent " + id + ")"
}

// oneLine returns the first line of s, cut to n runes.
func oneLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}

func settingsOf(i agent.SessionInfo) string {
	return fmt.Sprintf("model %s, effort %s, mode %s", cmp.Or(i.Model, unset), cmp.Or(string(i.Effort), unset), describeMode(i.PermissionMode))
}

// describeMode names a permission mode and says what it does.
func describeMode(m agent.PermissionMode) string {
	meaning := modeMeanings[m]
	if m == "" {
		return unset
	}
	if meaning == "" {
		return string(m) + " (native)"
	}
	return string(m) + " (" + meaning + ")"
}

var modeMeanings = map[agent.PermissionMode]string{
	agent.PermissionDefault:     "asks before risky actions",
	agent.PermissionAcceptEdits: "edits without asking",
	agent.PermissionPlan:        "plans without changing anything",
	agent.PermissionBypass:      "runs everything without asking",
	agent.PermissionReadOnly:    "reads only",
	agent.PermissionDontAsk:     "never asks; refuses what would ask",
	agent.PermissionAuto:        "a classifier approves each call",
}

// unset names a setting the harness chose without reporting it.
const unset = "harness default"
