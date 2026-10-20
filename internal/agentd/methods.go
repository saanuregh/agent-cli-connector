package agentd

import (
	"context"
	"slices"
	"strconv"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
)

// methodTable maps each method the client can call to its handler.
func (s *server) methodTable() map[string]method {
	return map[string]method{
		"initialize":                handler(s.initialize),
		pingMethod:                  handler(func(context.Context, struct{}) (any, error) { return nil, nil }),
		"session/start":             handler(s.start),
		"session/send":              handler(s.send),
		"session/info":              handler(s.info),
		"session/interrupt":         handler(s.interrupt),
		"session/close":             handler(s.closeSession),
		"provider/check":            handler(s.check),
		"provider/listModels":       handler(s.listModels),
		"provider/listSessions":     handler(s.listSessions),
		"provider/readHistory":      handler(s.readHistory),
		"provider/deleteSession":    handler(s.deleteSession),
		"provider/listSubagents":    handler(s.listSubagents),
		"session/fork":              handler(s.fork),
		"session/setModel":          handler(s.setModel),
		"session/setEffort":         handler(s.setEffort),
		"session/setPermissionMode": handler(s.setPermissionMode),
		"session/setTitle":          handler(s.setTitle),
		"session/compact":           handler(s.compact),
		"session/rewind":            handler(s.rewind),
		"session/stopTask":          handler(s.stopTask),
		"session/interruptTurn":     handler(s.interruptTurn),
		"session/rateLimits":        handler(s.rateLimits),
		"session/pendingRequests":   handler(s.pendingRequests),
		"request/respond":           handler(s.respond),
		"request/cancel":            handler(s.cancelRequest),
	}
}

// optionalMethods maps each method that needs an optional interface to a
// check of the provider or session it runs on.
var optionalMethods = map[string]func(any) bool{
	"provider/check":         implements[agent.Checker],
	"provider/listModels":    implements[agent.ModelLister],
	"provider/listSessions":  implements[agent.SessionLister],
	"provider/readHistory":   implements[agent.HistoryReader],
	"provider/deleteSession": implements[agent.SessionDeleter],
	"provider/listSubagents": implements[agent.SubagentLister],
	"session/fork":           implements[agent.Forker],
	"session/setTitle":       implements[agent.Titler],
	"session/compact":        implements[agent.Compacter],
	"session/rewind":         implements[agent.Rewinder],
	"session/stopTask":       implements[agent.TaskStopper],
	"session/interruptTurn":  implements[agent.TurnInterrupter],
	"session/rateLimits":     implements[agent.RateLimitReader],
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

// supported lists, sorted, the methods with one of prefixes that v
// supports.
func (s *server) supported(v any, prefixes ...string) []string {
	out := []string{}
	for _, name := range s.methodNames {
		if name == "session/start" || !slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			continue
		}
		if check, ok := optionalMethods[name]; ok && !check(v) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// providerAs looks up a provider and asserts that it implements T, the
// optional interface behind feature.
func providerAs[T any](s *server, name, feature string) (T, error) {
	var zero T
	prov, err := s.provider(name)
	if err != nil {
		return zero, err
	}
	impl, ok := prov.(T)
	if !ok {
		return zero, &agent.UnsupportedError{Provider: name, Feature: feature}
	}
	return impl, nil
}

// sessionAs looks up a hosted session and asserts that it implements T,
// the optional interface behind feature.
func sessionAs[T any](s *server, id, feature string) (T, error) {
	var zero T
	h, err := s.session(id)
	if err != nil {
		return zero, err
	}
	impl, ok := h.sess.(T)
	if !ok {
		return zero, &agent.UnsupportedError{Provider: h.spec.Name, Feature: feature}
	}
	return impl, nil
}

func (s *server) check(ctx context.Context, p ProviderParams) (any, error) {
	c, err := providerAs[agent.Checker](s, p.Provider, "Check")
	if err != nil {
		return nil, err
	}
	st, err := c.Check(ctx)
	if err != nil {
		return nil, err
	}
	return ProviderStatusWire{ProviderStatus: st, Err: errorInfo(st.Err)}, nil
}

func (s *server) listModels(ctx context.Context, p ProviderParams) (any, error) {
	l, err := providerAs[agent.ModelLister](s, p.Provider, "ListModels")
	if err != nil {
		return nil, err
	}
	return l.ListModels(ctx)
}

func (s *server) listSessions(ctx context.Context, p ListSessionsParams) (any, error) {
	l, err := providerAs[agent.SessionLister](s, p.Provider, "ListSessions")
	if err != nil {
		return nil, err
	}
	return l.ListSessions(ctx, p.Options)
}

func (s *server) readHistory(ctx context.Context, p ReadHistoryParams) (any, error) {
	r, err := providerAs[agent.HistoryReader](s, p.Provider, "ReadHistory")
	if err != nil {
		return nil, err
	}
	return r.ReadHistory(ctx, p.SessionID, p.Options)
}

func (s *server) deleteSession(ctx context.Context, p DeleteSessionParams) (any, error) {
	d, err := providerAs[agent.SessionDeleter](s, p.Provider, "DeleteSession")
	if err != nil {
		return nil, err
	}
	return nil, d.DeleteSession(ctx, p.SessionID, p.Cwd)
}

func (s *server) listSubagents(ctx context.Context, p ListSubagentsParams) (any, error) {
	l, err := providerAs[agent.SubagentLister](s, p.Provider, "ListSubagents")
	if err != nil {
		return nil, err
	}
	return l.ListSubagents(ctx, p.SessionID, p.Options)
}

func (s *server) fork(ctx context.Context, p ForkParams) (any, error) {
	h, err := s.session(p.Session)
	if err != nil {
		return nil, err
	}
	return s.startSession(ctx, p.Start, h)
}

func (s *server) setModel(ctx context.Context, p ValueParams) (any, error) {
	return nil, s.withSession(p.Session, func(sess agent.Session) error { return sess.SetModel(ctx, p.Value) })
}

func (s *server) setEffort(ctx context.Context, p ValueParams) (any, error) {
	return nil, s.withSession(p.Session, func(sess agent.Session) error { return sess.SetEffort(ctx, agent.Effort(p.Value)) })
}

func (s *server) setPermissionMode(ctx context.Context, p ValueParams) (any, error) {
	return nil, s.withSession(p.Session, func(sess agent.Session) error { return sess.SetPermissionMode(ctx, agent.PermissionMode(p.Value)) })
}

func (s *server) setTitle(ctx context.Context, p ValueParams) (any, error) {
	t, err := sessionAs[agent.Titler](s, p.Session, "SetTitle")
	if err != nil {
		return nil, err
	}
	return nil, t.SetTitle(ctx, p.Value)
}

func (s *server) compact(ctx context.Context, p CompactParams) (any, error) {
	c, err := sessionAs[agent.Compacter](s, p.Session, "Compact")
	if err != nil {
		return nil, err
	}
	return nil, c.Compact(ctx, p.Instructions)
}

func (s *server) rewind(ctx context.Context, p TurnParams) (any, error) {
	r, err := sessionAs[agent.Rewinder](s, p.Session, "Rewind")
	if err != nil {
		return nil, err
	}
	return nil, r.Rewind(ctx, p.TurnID)
}

func (s *server) stopTask(ctx context.Context, p StopTaskParams) (any, error) {
	t, err := sessionAs[agent.TaskStopper](s, p.Session, "StopTask")
	if err != nil {
		return nil, err
	}
	return nil, t.StopTask(ctx, p.TaskID)
}

func (s *server) interruptTurn(ctx context.Context, p TurnParams) (any, error) {
	t, err := sessionAs[agent.TurnInterrupter](s, p.Session, "InterruptTurn")
	if err != nil {
		return nil, err
	}
	return nil, t.InterruptTurn(ctx, p.TurnID)
}

func (s *server) rateLimits(ctx context.Context, p SessionParams) (any, error) {
	r, err := sessionAs[agent.RateLimitReader](s, p.Session, "RateLimits")
	if err != nil {
		return nil, err
	}
	return r.RateLimits(ctx)
}

func (s *server) pendingRequests(_ context.Context, p SessionParams) (any, error) {
	h, err := s.session(p.Session)
	if err != nil {
		return nil, err
	}
	out := []PendingRequest{}
	for _, req := range h.sess.PendingRequests() {
		data, err := s.encode(req)
		if err != nil {
			return nil, err
		}
		out = append(out, PendingRequest{Type: agent.EventType(req), Request: data})
	}
	return out, nil
}

// pendingRequest finds a request of a session that still waits for an
// answer.
func (s *server) pendingRequest(sessionID, requestID string) (agent.Request, error) {
	h, err := s.session(sessionID)
	if err != nil {
		return nil, err
	}
	for _, req := range h.sess.PendingRequests() {
		if req.RequestID() == requestID {
			return req, nil
		}
	}
	return nil, newError("not_found", "no pending request "+strconv.Quote(requestID))
}

func (s *server) respond(ctx context.Context, p RespondParams) (any, error) {
	set := 0
	for _, present := range []bool{p.Permission != nil, p.UserInput != nil, p.Elicitation != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return nil, newError("invalid_params", "set exactly one of Permission, UserInput, and Elicitation")
	}
	req, err := s.pendingRequest(p.Session, p.RequestID)
	if err != nil {
		return nil, err
	}
	switch r := req.(type) {
	case *agent.PermissionRequest:
		if p.Permission != nil {
			return nil, r.Respond(ctx, *p.Permission)
		}
	case *agent.UserInputRequest:
		if p.UserInput != nil {
			return nil, r.Respond(ctx, *p.UserInput)
		}
	case *agent.ElicitationRequest:
		if p.Elicitation != nil {
			return nil, r.Respond(ctx, *p.Elicitation)
		}
	}
	return nil, newError("invalid_params", "the response does not match the request type "+agent.EventType(req))
}

func (s *server) cancelRequest(ctx context.Context, p RequestParams) (any, error) {
	req, err := s.pendingRequest(p.Session, p.RequestID)
	if err != nil {
		return nil, err
	}
	return nil, req.Cancel(ctx)
}
