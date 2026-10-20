package agentd

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	agent "github.com/saanuregh/agent-cli-connector"
)

// forwardedTurns is how many recently forwarded TurnCompleted events a
// session remembers. A turn/result waits for its turn's event, which is
// forwarded around the time its Wait returns, so a few suffice.
const forwardedTurns = 64

// hosted is a session agentd runs for the client.
type hosted struct {
	id       string
	spec     ProviderSpec
	sess     agent.Session
	seq      atomic.Int64
	pumpDone chan struct{}

	mu        sync.Mutex
	forwarded []string      // IDs of turns whose TurnCompleted was sent, oldest first
	waiting   int           // turn/result notifications not yet sent
	ended     bool          // session/closed is being sent; no more waiters
	changed   chan struct{} // closed and replaced when forwarded or waiting changes
}

// markForwarded records that turn id's TurnCompleted event was sent.
func (h *hosted) markForwarded(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forwarded = append(h.forwarded, id)
	if len(h.forwarded) > forwardedTurns {
		h.forwarded = slices.Delete(h.forwarded, 0, len(h.forwarded)-forwardedTurns)
	}
	h.broadcastLocked()
}

func (h *hosted) broadcastLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// addWaiter registers a turn/result still to send, or reports false once
// the session has ended.
func (h *hosted) addWaiter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return false
	}
	h.waiting++
	return true
}

func (h *hosted) doneWaiter() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waiting--
	h.broadcastLocked()
}

// endWhenIdle waits until every registered turn/result was sent, then
// refuses new ones, so session/closed is the session's last message.
func (h *hosted) endWhenIdle() {
	for {
		h.mu.Lock()
		if h.waiting == 0 {
			h.ended = true
			h.mu.Unlock()
			return
		}
		changed := h.changed
		h.mu.Unlock()
		<-changed
	}
}

// awaitForwarded waits until turn id's TurnCompleted event was sent or the
// session's events ended, so the client sees a turn's events before its
// turn/result.
func (h *hosted) awaitForwarded(id string) {
	for {
		h.mu.Lock()
		if slices.Contains(h.forwarded, id) {
			h.mu.Unlock()
			return
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-h.pumpDone:
			return
		}
	}
}

func (s *server) start(ctx context.Context, p StartParams) (any, error) {
	return s.startSession(ctx, p, nil)
}

// startSession starts a session, or forks from when it is not nil.
func (s *server) startSession(ctx context.Context, p StartParams, from *hosted) (any, error) {
	name := p.Provider
	if from != nil {
		name = from.spec.Name
	}
	prov, err := s.provider(name)
	if err != nil {
		return nil, err
	}
	spec := s.specs[name]
	if p.Config.ProviderOptions != nil {
		return nil, newError("invalid_params", "Config.ProviderOptions must be empty; use ProviderOptions")
	}

	s.mu.Lock()
	s.lastSession++
	id := "s" + strconv.Itoa(s.lastSession)
	s.mu.Unlock()

	cfg := p.Config
	if spec.SessionOptions != nil {
		bridge := SessionBridge{Start: p, Session: id, Call: s.call}
		if cfg.ProviderOptions, err = spec.SessionOptions(p.ProviderOptions, bridge); err != nil {
			return nil, err
		}
	} else if !isEmpty(p.ProviderOptions) {
		return nil, newError("invalid_params", name+" has no session options")
	} else if err := refuseSessionCallbacks(name, p); err != nil {
		return nil, err
	}
	s.bindCallbacks(id, name, p, &cfg)
	if s.logger != nil {
		cfg.Logger = s.logger.With("session", id)
	}

	var sess agent.Session
	if from != nil {
		forker, ok := from.sess.(agent.Forker)
		if !ok {
			return nil, &agent.UnsupportedError{Provider: name, Feature: "ForkSession"}
		}
		sess, err = forker.ForkSession(ctx, cfg)
	} else {
		sess, err = prov.Start(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	h := &hosted{id: id, spec: spec, sess: sess, pumpDone: make(chan struct{}), changed: make(chan struct{})}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		_ = sess.Close()
		return nil, agent.ErrClosed
	}
	s.sessions[id] = h
	s.pumps.Add(1)
	s.mu.Unlock()
	go s.pump(h)
	return StartResult{Session: id, Info: sess.Info(), Methods: s.supported(sess, "session/", "request/")}, nil
}

// pump forwards a session's events until it ends, then reports the end.
func (s *server) pump(h *hosted) {
	defer s.pumps.Done()
	for ev := range h.sess.Events() {
		n := EventNotification{Session: h.id, Seq: h.seq.Add(1), Type: agent.EventType(ev)}
		data, err := s.encode(wireEvent(ev))
		if err != nil {
			// Keep the sequence whole: the client learns an event was lost.
			data = mustJSON(map[string]any{"EncodeError": err.Error()})
		}
		n.Event = data
		// Event is already encoded and the envelope has no Raw field, so
		// it skips notify's second pass.
		_ = s.conn.Notify("session/event", n)
		if tc, ok := ev.(*agent.TurnCompleted); ok {
			h.markForwarded(tc.TurnID)
		}
	}
	close(h.pumpDone)
	<-h.sess.Done()
	h.endWhenIdle()
	s.mu.Lock()
	delete(s.sessions, h.id)
	s.mu.Unlock()
	_ = s.notify("session/closed", SessionClosedNotification{Session: h.id, Error: errorInfo(h.sess.Err())})
}

func (s *server) session(id string) (*hosted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.sessions[id]
	if !ok {
		return nil, newError("not_found", "unknown session "+strconv.Quote(id))
	}
	return h, nil
}

func (s *server) send(ctx context.Context, p SendParams) (any, error) {
	h, err := s.session(p.Session)
	if err != nil {
		return nil, err
	}
	in := p.Input
	if in.ProviderOptions != nil {
		return nil, newError("invalid_params", "Input.ProviderOptions must be empty; use ProviderOptions")
	}
	if !isEmpty(p.ProviderOptions) {
		if h.spec.TurnOptions == nil {
			return nil, newError("invalid_params", h.spec.Name+" has no turn options")
		}
		if in.ProviderOptions, err = h.spec.TurnOptions(p.ProviderOptions); err != nil {
			return nil, err
		}
	}
	turn, err := h.sess.Send(ctx, in)
	if err != nil {
		return nil, err
	}
	sent := turn.ID()
	s.mu.Lock()
	closing := s.closing
	if !closing {
		s.pumps.Add(1)
	}
	s.mu.Unlock()
	if closing {
		return SendResult{TurnID: sent}, nil
	}
	if !h.addWaiter() {
		// The session already ended; session/closed ends this turn too.
		s.pumps.Done()
		return SendResult{TurnID: sent}, nil
	}
	go func() {
		defer s.pumps.Done()
		defer h.doneWaiter()
		res, err := turn.Wait(s.ctx)
		n := TurnResultNotification{Session: h.id, TurnID: sent, Result: turnResultWire(res)}
		if res == nil {
			n.Error = errorInfo(err)
		} else {
			// A turn with a result had a TurnCompleted event.
			h.awaitForwarded(res.TurnID)
		}
		if err := s.notify("turn/result", n); err != nil {
			// A result that cannot be encoded still ends the turn for the
			// client.
			_ = s.notify("turn/result", TurnResultNotification{Session: h.id, TurnID: sent,
				Error: errorInfo(fmt.Errorf("agentd: encode turn result: %w", err))})
		}
	}()
	return SendResult{TurnID: sent}, nil
}

func (s *server) info(_ context.Context, p SessionParams) (any, error) {
	h, err := s.session(p.Session)
	if err != nil {
		return nil, err
	}
	return h.sess.Info(), nil
}

func (s *server) interrupt(ctx context.Context, p SessionParams) (any, error) {
	return nil, s.withSession(p.Session, func(sess agent.Session) error { return sess.Interrupt(ctx) })
}

func (s *server) closeSession(_ context.Context, p SessionParams) (any, error) {
	return nil, s.withSession(p.Session, func(sess agent.Session) error { return sess.Close() })
}

// withSession runs f on a hosted session.
func (s *server) withSession(id string, f func(agent.Session) error) error {
	h, err := s.session(id)
	if err != nil {
		return err
	}
	return f(h.sess)
}
