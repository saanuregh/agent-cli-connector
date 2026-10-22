package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector/bindings/go"
)

// commands lists every command with its help text, in /help order.
var commands = []struct{ name, args, help string }{
	{"help", "", "list commands"},
	{"commands", "", "the harness's own slash commands; send them as /name, or //name to force"},
	{"image", "<path|url> [text]", "send an image with optional text"},
	{"file", "<path> [text]", "send a reference to a local file with optional text"},
	{"skill", "[name] [text]", "list the harness's skills, or invoke one"},
	{"json", "<schema> <text>", "ask for one answer matching a JSON Schema, inline or @file (-schema sets one for every turn)"},
	{"with", "[model=id] [effort=level] <text>", "send text with a model or effort from this turn on"},
	{"steer", "<text>", "inject text into the running turn"},
	{"now", "<text>", "interrupt the running turn and send text"},
	{"interrupt", "[turn]", "stop the running turn, or one turn (queued input is dropped)"},
	{"pending", "", "list requests waiting for an answer"},
	{"allow", "[n] [once|session|always]", "allow permission request n (default: oldest)"},
	{"deny", "[n] [reason]", "refuse permission request n"},
	{"answer", "<a1> | <a2> ...", "answer the oldest question; option numbers or text, commas for several"},
	{"accept", "[json]", "accept the oldest MCP elicitation with form content"},
	{"decline", "", "decline the oldest MCP elicitation"},
	{"cancel", "[n]", "cancel request n (a permission's cancel also stops the turn)"},
	{"info", "", "session metadata: model, effort, mode, tools, MCP servers"},
	{"caps", "", "the session's capabilities"},
	{"model", "[n|id]", "list models, or switch to one"},
	{"effort", "[n|level]", "list the current model's efforts, or set one"},
	{"mode", "[n|mode]", "list permission modes, or set one"},
	{"turns", "", "this session's turns with status and tokens"},
	{"transcript", "[n]", "the last n transcript items (default 20)"},
	{"usage", "", "token use and cost so far"},
	{"tasks", "", "subagents and background tasks"},
	{"stoptask", "[n|id]", "list running tasks, or stop one"},
	{"title", "<text>", "rename the session"},
	{"compact", "[instructions]", "compact the context"},
	{"fork", "[n|turn|all]", "list turns, or continue in a fork through turn n (all: the whole conversation)"},
	{"rewind", "[n|turn]", "list turns, or drop turn n and everything after it"},
	{"limits", "", "plan usage windows"},
	{"new", "", "start a fresh session"},
	{"resume", "[n|session id]", "list stored sessions, or resume one"},
	{"provider", "[n|name]", "list providers with their status, or start a session on one"},
	{"check", "", "harness install, version, and sign-in"},
	{"sessions", "[n]", "stored sessions in this folder, newest first"},
	{"history", "[session id] [n]", "the last n items of a stored session (default: this one)"},
	{"subagents", "[session id]", "subagents a stored session ran (default: this one)"},
	{"delete", "[n|session id] confirm", "list stored sessions, or delete one (cannot be undone)"},
	{"events", "[on|off|type,...]", "print every event, or only these types, such as text_delta"},
	{"reasoning", "[on|off]", "stream the model's reasoning"},
	{"wire", "[on|off]", "print every raw protocol line (needs -wire at start)"},
	{"call", "<method> [json]", "send a native harness request (Claude control, Codex method, Pi command)"},
	{"quit", "", "end the chat"},
}

// command runs one slash command.
func (a *app) command(name, args string) {
	sess := a.session()
	ctx := a.ctx
	switch name {
	case "help":
		for _, c := range commands {
			a.printf("  /%-10s %-24s %s\n", c.name, c.args, c.help)
		}
		a.printf("A number picks from the list the same command shows without one.\n")
	case "commands":
		cmds := sess.Info().Commands
		if len(cmds) == 0 {
			a.printf("the harness reported no slash commands\n")
		}
		for _, c := range cmds {
			a.printf("  /%-28s %-8s %s %s\n", c.Name, c.Kind, oneLine(c.Description, 70), c.ArgumentHint)
		}
	case "image", "file":
		target, text, _ := strings.Cut(args, " ")
		if target == "" {
			a.printf("usage: /%s <path> [text]\n", name)
			return
		}
		part := agent.FileRef(target)
		if name == "image" {
			part = agent.ImageFile(target)
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				part = agent.ImageURL(target)
			}
		}
		a.send(agent.Prompt(part, agent.TextPart(strings.TrimSpace(text))), args)
	case "skill":
		a.skillCommand(sess, args)
	case "json":
		in, err := schemaInput(args)
		if err != nil {
			a.printf("%v\nusage: /json {\"type\":\"object\",...} <text>, or /json @schema.json <text>\n", err)
			return
		}
		a.send(in, args)
	case "with":
		in := agent.Input{}
		words := strings.Fields(args)
	settings:
		for len(words) > 0 {
			key, value, ok := strings.Cut(words[0], "=")
			if !ok {
				break
			}
			switch key {
			case "model":
				in.Model = value
			case "effort":
				in.Effort = agent.Effort(value)
			default:
				break settings
			}
			words = words[1:]
		}
		if len(words) == 0 || in.Model == "" && in.Effort == "" {
			a.printf("usage: /with [model=id] [effort=level] <text>\n")
			return
		}
		text := strings.Join(words, " ")
		in.Parts = []agent.Part{agent.TextPart(text)}
		a.send(in, text)
	case "wire":
		switch args {
		case "", "on":
			a.wireOn.Store(true)
		case "off":
			a.wireOn.Store(false)
		default:
			a.printf("usage: /wire [on|off]\n")
			return
		}
		a.printf("wire %s\n", onOff(a.wireOn.Load()))
	case "steer", "now":
		if args == "" {
			a.printf("usage: /%s <text>\n", name)
			return
		}
		in := agent.Text(args)
		in.Delivery = map[string]agent.Delivery{"steer": agent.DeliverySteer, "now": agent.DeliveryInterrupt}[name]
		a.send(in, args)
	case "interrupt":
		if args == "" {
			a.report(sess.Interrupt(ctx))
			return
		}
		a.report(sess.InterruptTurn(ctx, args))
	case "pending":
		pending, err := sess.PendingRequests(ctx)
		if err != nil {
			a.report(err)
			return
		}
		if len(pending) == 0 {
			a.printf("no pending requests\n")
		}
		for i, r := range pending {
			a.printf("  %d. %s\n", i+1, describeRequest(r))
		}
	case "allow":
		n, rest := leadingNumber(args)
		req, err := requestOf[agent.PermissionRequest](a, n)
		if err != nil {
			a.report(err)
			return
		}
		decision := map[string]agent.PermissionDecision{"": agent.PermissionDecisionAllow, "once": agent.PermissionDecisionAllow,
			"session": agent.PermissionDecisionAllowSession, "always": agent.PermissionDecisionAllowAlways}[rest]
		if decision == "" {
			a.printf("usage: /allow [n] [once|session|always]\n")
			return
		}
		a.report(sess.Respond(ctx, req.ID, agent.PermissionResponse{Decision: decision}))
	case "deny":
		n, reason := leadingNumber(args)
		req, err := requestOf[agent.PermissionRequest](a, n)
		if err != nil {
			a.report(err)
			return
		}
		a.report(sess.Deny(ctx, req.ID, cmp.Or(reason, "The user refused.")))
	case "answer":
		req, err := requestOf[agent.UserInputRequest](a, 0)
		if err != nil {
			a.report(err)
			return
		}
		a.report(answerQuestions(ctx, sess, req, args))
	case "accept", "decline":
		e, err := requestOf[agent.ElicitationRequest](a, 0)
		if err != nil {
			a.report(err)
			return
		}
		if name == "decline" {
			a.report(sess.Decline(ctx, e.ID))
			return
		}
		var content any
		if args != "" {
			if !json.Valid([]byte(args)) {
				a.printf("the form content must be JSON\n")
				return
			}
			content = json.RawMessage(args)
		}
		a.report(sess.Accept(ctx, e.ID, content))
	case "cancel":
		n, _ := leadingNumber(args)
		req, err := requestOf[agent.EventPayload](a, n)
		if err != nil {
			a.report(err)
			return
		}
		switch req := req.(type) {
		case agent.PermissionRequest:
			a.report(sess.Respond(ctx, req.ID, agent.PermissionResponse{Decision: agent.PermissionDecisionCancel}))
		case agent.UserInputRequest:
			a.report(sess.Respond(ctx, req.ID, agent.UserInputResponse{Cancelled: true}))
		case agent.ElicitationRequest:
			a.report(sess.Respond(ctx, req.ID, agent.ElicitationResponse{Action: agent.ElicitationResponseActionCancel}))
		}
	case "info":
		info, err := sess.FetchInfo(ctx)
		if err != nil {
			info = sess.Info()
		}
		a.showInfo(info)
	case "caps":
		data, _ := json.MarshalIndent(sess.Capabilities(), "  ", "  ")
		a.printf("  %s\n", data)
	case "call":
		method, params, _ := strings.Cut(args, " ")
		var p any
		if params = strings.TrimSpace(params); params != "" {
			if !json.Valid([]byte(params)) {
				a.printf("usage: /call <method> [json]\n")
				return
			}
			p = json.RawMessage(params)
		}
		var result json.RawMessage
		if err := sess.Call(ctx, method, p, &result); err != nil {
			a.report(err)
			return
		}
		a.printf("%s\n", cmp.Or(string(result), "null"))
	case "model":
		a.modelCommand(sess, args)
	case "effort":
		a.effortCommand(sess, args)
	case "mode":
		modes := sess.Capabilities().PermissionModes
		if args == "" {
			current := sess.Info().PermissionMode
			a.printf("mode: %s\n", describeMode(current))
			for i, m := range modes {
				a.printf("  %d. %s%s\n", i+1, mark(m == current), describeMode(m))
			}
			return
		}
		a.report(sess.SetPermissionMode(ctx, agent.PermissionMode(choose(args, modes, func(m agent.PermissionMode) string { return string(m) }))))
	case "turns":
		a.showTurns()
	case "transcript":
		n := positiveOr(args, 20)
		a.mu.Lock()
		items := slices.Clone(a.items)
		a.mu.Unlock()
		a.showItems(items[max(0, len(items)-n):])
	case "usage":
		a.mu.Lock()
		u := a.usage
		a.mu.Unlock()
		a.printf("input %d, output %d (reasoning %d), cache read %d, cache write %d, cost $%.4f, context %d / %d\n",
			u.InputTokens, u.OutputTokens, u.ReasoningTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD, u.ContextTokens, u.ContextWindow)
	case "tasks":
		tasks := a.taskList(false)
		if len(tasks) == 0 {
			a.printf("no tasks\n")
		}
		for _, t := range tasks {
			a.printf("  %s  %-8s %-9s %s\n", t.ID, t.Kind, t.Status, oneLine(cmp.Or(t.Description, t.Summary), 80))
		}
	case "stoptask":
		running := a.taskList(true)
		if args == "" {
			if len(running) == 0 {
				a.printf("no running tasks\n")
			}
			for i, t := range running {
				a.printf("  %d. %s  %-8s %s\n", i+1, t.ID, t.Kind, oneLine(cmp.Or(t.Description, t.Summary), 80))
			}
			return
		}
		a.report(sess.StopTask(ctx, choose(args, running, func(t agent.Task) string { return t.ID })))
	case "title":
		a.report(sess.SetTitle(ctx, args))
	case "compact":
		a.printf("[compacting…]\n")
		a.report(sess.Compact(ctx, args))
	case "fork":
		a.forkCommand(sess, args)
	case "rewind":
		a.rewindCommand(sess, args)
	case "limits":
		limits, err := sess.RateLimits(ctx)
		if err != nil {
			a.report(err)
			return
		}
		for _, l := range limits {
			a.printf("  %s %s: %.0f%% used, %s%s\n", cmp.Or(l.LimitID, "plan"), l.Name, l.UsedPercent, l.Status, resetNote(l.ResetsAtMs))
		}
	case "new":
		provider, _ := a.active()
		a.report(a.start(provider, a.cfg))
	case "resume":
		id, ok := a.chooseSession(sess, args, "")
		if !ok {
			return
		}
		provider, _ := a.active()
		cfg := a.cfg
		cfg.ResumeID = id
		a.report(a.start(provider, cfg))
	case "provider":
		names := a.providerNames()
		if args == "" {
			a.showProviders(names)
			return
		}
		provider := choose(args, names, func(n string) string { return n })
		if !slices.Contains(names, provider) {
			a.printf("unknown provider %q; /provider lists them\n", provider)
			return
		}
		cfg := a.cfg
		// Model and effort names differ between harnesses.
		cfg.Model, cfg.Effort = "", ""
		a.report(a.start(provider, cfg))
	case "check":
		a.checkCommand()
	case "sessions":
		if list, ok := a.storedSessions(sess, positiveOr(args, 10)); ok {
			a.showSessions(sess, list)
		}
	case "history":
		a.historyCommand(args)
	case "subagents":
		a.subagentsCommand(args)
	case "delete":
		a.deleteCommand(args)
	case "events":
		a.setEventFilter(args)
	case "reasoning":
		on := args != "off"
		a.mu.Lock()
		a.reasoning = on
		a.mu.Unlock()
		a.printf("reasoning %s\n", onOff(on))
	default:
		// The harness's own slash commands go to it as text.
		if slices.ContainsFunc(sess.Info().Commands, func(c agent.SlashCommand) bool { return c.Name == name }) {
			text := "/" + name
			if args != "" {
				text += " " + args
			}
			a.send(agent.Text(text), text)
			return
		}
		a.printf("unknown command /%s; /help lists the chat's, /commands the harness's (//%s sends it anyway)\n", name, name)
	}
}

// providerNames lists the providers the client offers.
func (a *app) providerNames() []string {
	var names []string
	for _, p := range a.client.Providers() {
		names = append(names, p.Name)
	}
	return names
}

// skillCommand lists the harness's skills, or sends one with text.
func (a *app) skillCommand(sess *agent.Session, args string) {
	var skills []agent.SlashCommand
	for _, c := range sess.Info().Commands {
		if c.Kind == "skill" {
			skills = append(skills, c)
		}
	}
	name, text, _ := strings.Cut(args, " ")
	if name == "" {
		if len(skills) == 0 {
			a.printf("the harness reported no skills\n")
		}
		for i, c := range skills {
			a.printf("  %2d. %-28s %s\n", i+1, c.Name, oneLine(c.Description, 80))
		}
		return
	}
	skill := agent.SlashCommand{Name: name}
	if i, ok := pick(name, len(skills)); ok {
		skill = skills[i]
	} else if i := slices.IndexFunc(skills, func(c agent.SlashCommand) bool { return c.Name == name }); i >= 0 {
		skill = skills[i]
	}
	parts := []agent.Part{agent.SkillPart(skill.Name, skill.Path)}
	if text = strings.TrimSpace(text); text != "" {
		parts = append(parts, agent.TextPart(text))
	}
	a.send(agent.Prompt(parts...), strings.TrimSpace("/"+skill.Name+" "+text))
}

// schemaInput reads "/json <schema> <text>": the schema is a JSON value or
// @file, and the rest is the prompt.
func schemaInput(args string) (agent.Input, error) {
	var schema json.RawMessage
	rest := args
	if path, ok := strings.CutPrefix(args, "@"); ok {
		path, rest, _ = strings.Cut(path, " ")
		data, err := os.ReadFile(path)
		if err != nil {
			return agent.Input{}, err
		}
		if !json.Valid(data) {
			return agent.Input{}, errors.New("schema is not valid JSON")
		}
		schema = data
	} else {
		dec := json.NewDecoder(strings.NewReader(args))
		if err := dec.Decode(&schema); err != nil {
			return agent.Input{}, fmt.Errorf("schema: %w", err)
		}
		rest = args[dec.InputOffset():]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return agent.Input{}, errors.New("missing text")
	}
	in := agent.Text(rest)
	in.OutputSchema = schema
	return in, nil
}

func (a *app) modelCommand(sess *agent.Session, args string) {
	models, listErr := a.models()
	if args != "" {
		a.report(sess.SetModel(a.ctx, choose(args, models, func(m agent.Model) string { return m.ID })))
		return
	}
	current := sess.Info().Model
	a.printf("model: %s\n", cmp.Or(current, unset))
	if listErr != nil {
		a.report(listErr)
		return
	}
	for i, m := range models {
		a.printf("  %d. %s%s%s\n", i+1, mark(sameModel(m, current)), m.ID, describeModel(m))
	}
}

func (a *app) effortCommand(sess *agent.Session, args string) {
	info := sess.Info()
	var efforts []agent.Effort
	modelID := info.Model
	if models, err := a.models(); err == nil {
		if i := slices.IndexFunc(models, func(m agent.Model) bool { return sameModel(m, info.Model) }); i >= 0 {
			efforts, modelID = models[i].Efforts, models[i].ID
		}
	}
	if args != "" {
		a.report(sess.SetEffort(a.ctx, agent.Effort(choose(args, efforts, func(e agent.Effort) string { return string(e) }))))
		return
	}
	a.printf("effort: %s\n", cmp.Or(info.Effort, unset))
	if len(efforts) == 0 {
		a.printf("no effort list for %s; /model lists each model's efforts\n", cmp.Or(modelID, "the current model"))
		return
	}
	for i, e := range efforts {
		a.printf("  %d. %s%s\n", i+1, mark(e == info.Effort), e)
	}
}

// turnChoice is a turn /fork and /rewind can name.
type turnChoice struct {
	id, prompt string
}

// turnChoices lists the session's turns: from its stored history when the
// provider can read it, so a resumed session shows its earlier turns too,
// then any turns of this chat the history does not have yet.
func (a *app) turnChoices(sess *agent.Session) []turnChoice {
	var out []turnChoice
	seen := map[string]bool{}
	provider, _ := a.active()
	a.mu.Lock()
	sent := slices.Clone(a.turns)
	a.mu.Unlock()
	items, err := a.client.ReadHistory(a.ctx, provider, sess.ID(), agent.ReadHistoryOptions{Cwd: sess.Info().Cwd})
	if err == nil {
		for _, it := range items {
			if it.Kind == agent.ItemKindUserMessage && it.TurnID != "" && !seen[it.TurnID] && !it.Synthetic {
				seen[it.TurnID] = true
				out = append(out, turnChoice{id: it.TurnID, prompt: it.Text})
			}
		}
	}
	for _, t := range sent {
		if id := t.turn.ID(); !seen[id] {
			seen[id] = true
			out = append(out, turnChoice{id: id, prompt: t.prompt})
		}
	}
	return out
}

func (a *app) rewindCommand(sess *agent.Session, args string) {
	if turn, ok := a.chooseTurn(sess, args, "/rewind n drops turn n and everything after it"); ok {
		a.report(sess.Rewind(a.ctx, turn))
	}
}

func (a *app) forkCommand(sess *agent.Session, args string) {
	turn, ok := a.chooseTurn(sess, args, "/fork n keeps the conversation through turn n; /fork all copies all of it")
	if !ok {
		return
	}
	if turn == "all" {
		turn = ""
	}
	forked, err := sess.Fork(a.ctx, agent.Config{ForkAtTurn: turn})
	if err != nil {
		a.report(err)
		return
	}
	a.printf("[forked %s into %s; continuing in the fork]\n", sess.ID(), forked.ID())
	provider, _ := a.active()
	a.attach(provider, forked)
}

// chooseTurn returns the turn args names, by ID or by number in
// turnChoices. Without args it lists the turns, prints hint, and reports
// false.
// chooseSession resolves args to a stored session ID: an ID as given, or a
// number from the stored session list. Empty args lists the sessions, then
// hint when set.
func (a *app) chooseSession(sess *agent.Session, args, hint string) (string, bool) {
	if args != "" && !isNumber(args) {
		return args, true
	}
	list, ok := a.storedSessions(sess, 10)
	if !ok {
		return "", false
	}
	if args != "" {
		return choose(args, list, sessionID), true
	}
	a.showSessions(sess, list)
	if hint != "" {
		a.printf("%s\n", hint)
	}
	return "", false
}

func (a *app) chooseTurn(sess *agent.Session, args, hint string) (string, bool) {
	if args != "" && !isNumber(args) {
		return args, true
	}
	turns := a.turnChoices(sess)
	if args != "" {
		return choose(args, turns, func(t turnChoice) string { return t.id }), true
	}
	if len(turns) == 0 {
		a.printf("no turns yet\n")
	}
	for i, t := range turns {
		a.printf("  %d. %s  %s\n", i+1, t.id, oneLine(t.prompt, 80))
	}
	a.printf("%s\n", hint)
	return "", false
}

func (a *app) showTurns() {
	a.mu.Lock()
	turns := slices.Clone(a.turns)
	a.mu.Unlock()
	if len(turns) == 0 {
		a.printf("no turns yet\n")
	}
	for i, t := range turns {
		state := "running"
		select {
		case <-t.turn.Done():
			// Wait returns at once for a finished turn.
			if res, _ := t.turn.Wait(a.ctx); res != nil {
				state = fmt.Sprintf("%s, %d in / %d out", res.Status, res.Usage.InputTokens, res.Usage.OutputTokens)
				if res.Usage.CostUSD > 0 {
					state += fmt.Sprintf(", $%.4f", res.Usage.CostUSD)
				}
			} else {
				state = "did not run"
			}
		default:
		}
		a.printf("  %d. %s  %-34s %s\n", i+1, t.turn.ID(), state, oneLine(t.prompt, 60))
	}
}

// taskList returns the tasks seen, oldest ID first, only running ones when
// running is set.
func (a *app) taskList(running bool) []agent.Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	tasks := make([]agent.Task, 0, len(a.tasks))
	for _, t := range a.tasks {
		if !running || t.Status == agent.TaskStatusRunning {
			tasks = append(tasks, t)
		}
	}
	slices.SortFunc(tasks, func(x, y agent.Task) int { return strings.Compare(x.ID, y.ID) })
	return tasks
}

// storedSessions lists up to limit stored sessions in sess's folder.
func (a *app) storedSessions(sess *agent.Session, limit int) ([]agent.SessionSummary, bool) {
	provider, _ := a.active()
	list, err := a.client.ListSessions(a.ctx, provider, agent.ListSessionsOptions{Cwd: sess.Info().Cwd, Limit: int64(limit)})
	if err != nil {
		a.report(err)
		return nil, false
	}
	return list, true
}

func (a *app) showSessions(sess *agent.Session, list []agent.SessionSummary) {
	if len(list) == 0 {
		a.printf("no stored sessions in %s\n", sess.Info().Cwd)
	}
	current := sess.ID()
	for i, s := range list {
		a.printf("  %d. %s%s  %s  %s\n", i+1, mark(s.ID == current), s.ID, time.UnixMilli(s.UpdatedAtMs).Local().Format(time.DateTime),
			oneLine(cmp.Or(s.Title, s.LastPrompt, s.FirstPrompt), 60))
	}
}

// showProviders lists the providers with their Check result, checked at
// the same time because each check can start a harness process.
func (a *app) showProviders(names []string) {
	statuses := make([]string, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
			defer cancel()
			st, err := a.client.Check(ctx, name)
			switch {
			case err != nil:
				statuses[i] = err.Error()
			case st.Error != nil:
				statuses[i] = oneLine(st.Error.Message, 80)
			default:
				statuses[i] = "version " + cmp.Or(st.Version, "unknown")
				if st.Auth != nil && st.Auth.Authenticated {
					statuses[i] += ", signed in"
				}
			}
		})
	}
	wg.Wait()
	current, _ := a.active()
	for i, name := range names {
		a.printf("  %d. %s%-9s %s\n", i+1, mark(name == current), name, statuses[i])
	}
}

func (a *app) checkCommand() {
	provider, _ := a.active()
	st, err := a.client.Check(a.ctx, provider)
	if err != nil {
		a.report(err)
		return
	}
	a.printf("installed %v, version %s", st.Installed, cmp.Or(st.Version, "unknown"))
	if st.Auth != nil {
		a.printf(", signed in %v (%s %s %s)", st.Auth.Authenticated, st.Auth.Method, st.Auth.Email, st.Auth.Plan)
	}
	a.printf("\n")
	if st.Error != nil {
		a.printf("problem: %s\n", st.Error.Message)
	}
}

// historyCommand reads "/history [session id] [n]".
func (a *app) historyCommand(args string) {
	provider, sess := a.active()
	id, n := sess.ID(), 20
	fields := strings.Fields(args)
	if len(fields) == 1 && isNumber(fields[0]) {
		n, _ = strconv.Atoi(fields[0])
		fields = nil
	}
	if len(fields) > 0 {
		id = fields[0]
	}
	if len(fields) > 1 {
		if v, err := strconv.Atoi(fields[1]); err == nil {
			n = v
		}
	}
	items, err := a.client.ReadHistory(a.ctx, provider, id, agent.ReadHistoryOptions{Cwd: sess.Info().Cwd, Limit: int64(n)})
	if err != nil {
		a.report(err)
		return
	}
	a.showItems(items)
}

func (a *app) subagentsCommand(args string) {
	provider, sess := a.active()
	tasks, err := a.client.ListSubagents(a.ctx, provider, cmp.Or(args, sess.ID()), sess.Info().Cwd)
	if err != nil {
		a.report(err)
		return
	}
	if len(tasks) == 0 {
		a.printf("no subagents\n")
	}
	for _, t := range tasks {
		a.printf("  %s  %-9s %s\n", t.ID, t.Status, oneLine(t.Description, 80))
	}
}

func (a *app) deleteCommand(args string) {
	provider, sess := a.active()
	target, confirm, _ := strings.Cut(args, " ")
	target, ok := a.chooseSession(sess, target, "/delete n confirm deletes session n; it cannot be undone")
	if !ok {
		return
	}
	if strings.TrimSpace(confirm) != "confirm" {
		a.printf("add \"confirm\" to delete %s; it cannot be undone\n", target)
		return
	}
	if target == sess.ID() {
		a.printf("refusing to delete the active session\n")
		return
	}
	a.report(a.client.DeleteSession(a.ctx, provider, target, sess.Info().Cwd))
}

func (a *app) showInfo(i agent.SessionInfo) {
	a.printf("id %s, provider %s, version %s\ncwd %s\n%s, title %q\n",
		i.ID, i.Provider, cmp.Or(i.Version, "unknown"), i.Cwd, settingsOf(i), i.Title)
	if len(i.Tools) > 0 {
		a.printf("tools (%d): %s\n", len(i.Tools), oneLine(strings.Join(i.Tools, ", "), 300))
	}
	if len(i.Commands) > 0 {
		a.printf("slash commands: %d (/commands lists them)\n", len(i.Commands))
	}
	for _, m := range i.McpServers {
		a.printf("mcp %s: %s %s\n", m.Name, m.Status, m.Error)
	}
}

func (a *app) showItems(items []agent.Item) {
	if len(items) == 0 {
		a.printf("no items\n")
	}
	for _, it := range items {
		text := it.Text
		if it.Tool != nil {
			text = it.Tool.Name + " " + toolSummary(it.Tool)
		}
		a.printf("  %-17s %-10s %s%s\n", it.Kind, it.Status, oneLine(text, 100), subagentNote(it.SubagentID))
	}
}

// setEventFilter turns the event view on, off, or on for some types only.
func (a *app) setEventFilter(args string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch args {
	case "", "on":
		a.verbose, a.eventTypes = true, nil
	case "off":
		a.verbose, a.eventTypes = false, nil
	default:
		a.verbose, a.eventTypes = true, map[string]bool{}
		for _, t := range strings.Split(args, ",") {
			a.eventTypes[strings.TrimSpace(t)] = true
		}
	}
	a.printf("events %s\n", onOff(a.verbose))
}

// models returns the provider's models, listed once per provider because
// listing can start a harness process.
func (a *app) models() ([]agent.Model, error) {
	a.mu.Lock()
	name := a.provider
	cached, ok := a.modelCache[name]
	a.mu.Unlock()
	if ok {
		return cached, nil
	}
	models, err := a.client.ListModels(a.ctx, name)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.modelCache[name] = models
	a.mu.Unlock()
	return models, nil
}

// sameModel reports whether m is the model named id, also by the full
// model name an alias resolves to (Claude lists "sonnet" for
// "claude-sonnet-…").
func sameModel(m agent.Model, id string) bool {
	if id == "" {
		return false
	}
	if m.ID == id {
		return true
	}
	return m.ResolvedID == id && m.ID != "default"
}

func describeModel(m agent.Model) string {
	var parts []string
	if m.IsDefault {
		parts = append(parts, "default")
	}
	if m.DisplayName != "" && m.DisplayName != m.ID {
		parts = append(parts, m.DisplayName)
	}
	if len(m.Efforts) > 0 {
		efforts := fmt.Sprint(m.Efforts)
		if m.DefaultEffort != "" {
			efforts += " default " + string(m.DefaultEffort)
		}
		parts = append(parts, "efforts "+efforts)
	}
	if len(parts) == 0 {
		return ""
	}
	return "  (" + strings.Join(parts, "; ") + ")"
}

// requestOf returns the payload of pending request n (1-based, in
// /pending order) or, for n 0, the oldest one of type T.
func requestOf[T agent.EventPayload](a *app, n int) (T, error) {
	var zero T
	pending, err := a.session().PendingRequests(a.ctx)
	if err != nil {
		return zero, err
	}
	if n > 0 {
		r, ok := zero, false
		if n <= len(pending) {
			r, ok = pending[n-1].Payload.(T)
		}
		if !ok {
			return zero, fmt.Errorf("no matching request %d; see /pending", n)
		}
		return r, nil
	}
	for _, ev := range pending {
		if r, ok := ev.Payload.(T); ok {
			return r, nil
		}
	}
	return zero, errors.New("no matching pending request")
}

// answerQuestions answers req's questions in order from "|"-separated
// answers. A number picks an option; commas separate several picks.
func answerQuestions(ctx context.Context, sess *agent.Session, req agent.UserInputRequest, args string) error {
	parts := strings.Split(args, "|")
	if args == "" || len(parts) != len(req.Questions) {
		return fmt.Errorf("give %d answers separated by |", len(req.Questions))
	}
	answers := map[string][]string{}
	for i, q := range req.Questions {
		var picked []string
		for _, a := range strings.Split(parts[i], ",") {
			a = strings.TrimSpace(a)
			if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(q.Options) {
				a = q.Options[n-1].Label
			}
			if a != "" {
				picked = append(picked, a)
			}
		}
		answers[q.ID] = picked
	}
	return sess.Answer(ctx, req.ID, answers)
}

func describeRequest(ev agent.Event) string {
	switch r := ev.Payload.(type) {
	case agent.PermissionRequest:
		return fmt.Sprintf("permission: %s %s%s", cmp.Or(r.Title, r.ToolName), oneLine(cmp.Or(r.Command, string(r.Input)), 100), subagentNote(r.SubagentID))
	case agent.UserInputRequest:
		texts := make([]string, 0, len(r.Questions))
		for _, q := range r.Questions {
			texts = append(texts, q.Text)
		}
		return fmt.Sprintf("question: %s%s", oneLine(strings.Join(texts, " | "), 120), subagentNote(r.SubagentID))
	case agent.ElicitationRequest:
		return fmt.Sprintf("elicitation from %s: %s", r.Server, oneLine(r.Message, 100))
	}
	return string(ev.Type)
}

// choose returns the name of the list entry a 1-based number arg picks,
// or arg itself when it is not such a number.
func choose[T any](arg string, list []T, name func(T) string) string {
	if i, ok := pick(arg, len(list)); ok {
		return name(list[i])
	}
	return arg
}

// pick returns the 0-based index for a 1-based number arg within n
// choices.
func pick(arg string, n int) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || v < 1 || v > n {
		return 0, false
	}
	return v - 1, true
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

// positiveOr returns s as a number when it is a positive one, otherwise
// fallback.
func positiveOr(s string, fallback int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return fallback
}

func sessionID(s agent.SessionSummary) string { return s.ID }

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// mark flags the current choice in a list.
func mark(current bool) string {
	if current {
		return "* "
	}
	return "  "
}

// leadingNumber splits "3 rest" into 3 and "rest"; without a number it
// returns 0 and s.
func leadingNumber(s string) (int, string) {
	first, rest, _ := strings.Cut(s, " ")
	if n, err := strconv.Atoi(first); err == nil {
		return n, strings.TrimSpace(rest)
	}
	return 0, s
}
