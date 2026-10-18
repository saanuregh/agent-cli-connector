package providerkit

import (
	"cmp"
	"context"
	"slices"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/providerutil"
)

// The turn queue runs one input at a time. A single running slot holds the
// turn the harness works on, or the turn whose start is in flight. Input
// sent while the slot is taken waits in a FIFO queue and starts, one at a
// time, on a runtime goroutine once the slot frees. All of it is guarded
// by Runtime.mu.

// Starter starts queued input as turn id. It returns once the harness
// accepted or refused the input; an error abandons the turn with that
// error. A start that ends the turn at once, such as a command the harness
// handles without a run, finishes the turn itself.
type Starter func(ctx context.Context, id string, in agent.Input) error

type queuedInput struct {
	id string
	in agent.Input
}

// SetStarter sets the function that starts queued input. Providers set it
// before the first Send.
func (r *Runtime) SetStarter(start Starter) {
	r.mu.Lock()
	r.starter = start
	r.mu.Unlock()
}

// Idle reports whether no turn runs or starts, no steer that may start a
// run is in flight, and no input is queued.
func (r *Runtime) Idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.idleLocked()
}

func (r *Runtime) idleLocked() bool {
	return r.running == "" && len(r.starting) == 0 && r.steering == "" && len(r.inputs) == 0
}

// StartIdle starts turn id when the session is idle (see Idle): it takes
// the running slot, registers the turn, and runs start with ctx on the
// calling goroutine. When start fails, the turn is abandoned and StartIdle
// returns the error and no turn. When the session is busy, StartIdle does
// nothing and reports ok=false.
func (r *Runtime) StartIdle(ctx context.Context, id string, start func(context.Context) error) (turn *agent.Turn, ok bool, err error) {
	r.mu.Lock()
	if !r.finished && !r.idleLocked() {
		r.mu.Unlock()
		return nil, false, nil
	}
	turn, err = r.reserveLocked(id)
	r.mu.Unlock()
	if err != nil {
		return nil, true, err
	}
	if err := r.runStart(ctx, id, start); err != nil {
		return nil, true, err
	}
	return turn, true, nil
}

// Enqueue registers turn id and queues in to run once the running slot is
// free. When the session is idle the input starts at once, on a runtime
// goroutine.
func (r *Runtime) Enqueue(id string, in agent.Input) (*agent.Turn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, err := r.newTurnLocked(id)
	if err != nil {
		return nil, err
	}
	r.inputs = append(r.inputs, queuedInput{id: id, in: in})
	r.kickLocked()
	return turn, nil
}

// ClearQueue drops the queued input, fails its turns with
// agent.ErrCancelled, and returns the input in queue order.
func (r *Runtime) ClearQueue() []agent.Input { return r.AbandonQueue(agent.ErrCancelled) }

// AbandonQueue drops the queued input, fails its turns with err, and
// returns the input in queue order.
func (r *Runtime) AbandonQueue(err error) []agent.Input {
	r.mu.Lock()
	queued := r.inputs
	r.inputs = nil
	r.mu.Unlock()
	out := make([]agent.Input, 0, len(queued))
	for _, q := range queued {
		r.AbandonTurn(q.id, err)
		out = append(out, q.in)
	}
	return out
}

// CancelQueued drops queued input id and fails its turn with
// agent.ErrCancelled. It reports false when id is not queued.
func (r *Runtime) CancelQueued(id string) bool {
	r.mu.Lock()
	i := slices.IndexFunc(r.inputs, func(q queuedInput) bool { return q.id == id })
	if i >= 0 {
		r.inputs = slices.Delete(r.inputs, i, i+1)
	}
	r.mu.Unlock()
	if i < 0 {
		return false
	}
	r.AbandonTurn(id, agent.ErrCancelled)
	return true
}

// Running returns the turn holding the running slot, or "" when it is
// free, and whether a start is in flight. While a start is in flight the
// turn may still carry the ID it was submitted with.
func (r *Runtime) Running() (id string, starting bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, len(r.starting) > 0
}

// Accepted marks the start of turn id done: the harness accepted the
// input. A harness that assigns its own turn IDs passes the turn's ID as
// harnessID, and the turn, including its handle, takes that ID. Accepted
// reports whether an interrupt was asked for while the start was in
// flight; the provider sends it now. A provider that does not call
// Accepted has the start marked done when the start function returns or
// RunStarted reports its run.
func (r *Runtime) Accepted(id, harnessID string) (interrupt bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = r.acceptLocked(id, harnessID)
	if r.interrupt == id {
		interrupt = r.interruptPending
		r.interruptPending = false
	}
	r.kickLocked()
	return interrupt
}

// acceptLocked marks the start of turn id done and gives the turn
// harnessID, when set. It returns the turn's ID.
func (r *Runtime) acceptLocked(id, harnessID string) string {
	r.starting = slices.DeleteFunc(r.starting, func(s string) bool { return s == id })
	if harnessID == "" || harnessID == id {
		return id
	}
	if t := r.turnLocked(id); t != nil {
		setTurnID(t.turn, harnessID)
	}
	if r.running == id {
		r.running = harnessID
	}
	if r.interrupt == id {
		r.interrupt = harnessID
	}
	// Callers that read the slot before the rename still name the turn.
	r.renamedFrom, r.renamedTo = id, harnessID
	return harnessID
}

// RunStarted reports a run the harness started; id is its turn ID, or ""
// when the harness has none. When the slot is free and no start is in
// flight, the run takes the slot as a tracked turn: as the turn of the
// steer in flight, if any, otherwise as id or a new ID. When the slot's
// start is the only one in flight and id is set, the run is that start's
// turn, which harnesses that run one turn at a time guarantee: the start is
// accepted as turn id (see Accepted), though an interrupt asked for during
// the start is still reported by Accepted. It returns the turn holding the
// slot and whether a start is in flight.
func (r *Runtime) RunStarted(id string) (running string, starting bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" && len(r.starting) == 1 && r.starting[0] == r.running {
		r.acceptLocked(r.running, id)
	}
	if r.running != "" || len(r.starting) > 0 || r.finished {
		return r.running, len(r.starting) > 0
	}
	r.running = cmp.Or(r.steering, id, providerutil.NewUUID())
	r.steering = ""
	_, _ = r.newTurnLocked(r.running)
	return r.running, false
}

// BeginSteer marks steer id into turn current in flight, and reports
// false, marking nothing, unless current holds the slot with its start
// done and no other steer is in flight: input steered into a turn that is
// still starting, or that another turn replaced, could start a run of its
// own ahead of that turn, so it should be queued instead. Until EndSteer
// or a run adopts the steer (see RunStarted), queued input waits, because
// the harness may start a run for the steer when the running turn ended
// meanwhile.
func (r *Runtime) BeginSteer(id, current string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current == "" || r.running != current || len(r.starting) > 0 || r.steering != "" {
		return false
	}
	r.steering = id
	return true
}

// EndSteer ends steer id that did not start a run.
func (r *Runtime) EndSteer(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steering == id {
		r.steering = ""
		r.kickLocked()
	}
}

// Interrupt records an interrupt of the turn holding the slot when it is
// want, or of any turn when want is "", so an interrupt meant for one turn
// never stops the input that replaced it. It returns the turn, or "" when
// there is none to interrupt, and whether its start is in flight; such an
// interrupt is reported by Accepted. The record lasts until the turn ends
// or ClearInterrupt.
func (r *Runtime) Interrupt(want string) (id string, starting bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if want != "" && want == r.renamedFrom {
		want = r.renamedTo
	}
	if r.running == "" || want != "" && want != r.running {
		return "", false
	}
	r.interrupt = r.running
	r.interruptPending = slices.Contains(r.starting, r.running)
	return r.interrupt, r.interruptPending
}

// Interrupting returns the turn an interrupt was asked for, or "", and
// whether the interrupt is still pending on the turn's start.
func (r *Runtime) Interrupting() (id string, pending bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupt, r.interruptPending
}

// ClearInterrupt forgets the interrupt of turn id, for an interrupt the
// harness refused.
func (r *Runtime) ClearInterrupt(id string) {
	r.mu.Lock()
	if r.interrupt == id {
		r.interrupt, r.interruptPending = "", false
	}
	r.mu.Unlock()
}

// reserveLocked registers turn id and gives it the running slot with its
// start in flight.
func (r *Runtime) reserveLocked(id string) (*agent.Turn, error) {
	turn, err := r.newTurnLocked(id)
	if err != nil {
		return nil, err
	}
	r.running = id
	r.starting = append(r.starting, id)
	return turn, nil
}

// runStart runs the start of turn id and then marks it done. A failed
// start abandons the turn.
func (r *Runtime) runStart(ctx context.Context, id string, start func(context.Context) error) error {
	err := start(ctx)
	r.mu.Lock()
	if i := slices.Index(r.starting, id); i >= 0 {
		r.starting = slices.Delete(r.starting, i, i+1)
		if err == nil && r.interrupt == id {
			r.interruptPending = false
		}
	}
	r.kickLocked()
	r.mu.Unlock()
	if err != nil {
		r.AbandonTurn(id, err)
	}
	return err
}

// releaseLocked frees what turn id holds because it ended.
func (r *Runtime) releaseLocked(id string) {
	if id == "" {
		return
	}
	if r.interrupt == id {
		r.interrupt, r.interruptPending = "", false
	}
	if r.running == id {
		r.running = ""
		r.kickLocked()
	}
}

func (r *Runtime) canStartLocked() bool {
	return !r.finished && r.starter != nil && len(r.inputs) > 0 &&
		r.running == "" && len(r.starting) == 0 && r.steering == ""
}

// kickLocked starts the queue goroutine when queued input can start and
// none runs. Starting never happens on the caller's goroutine, which may
// be the harness reader.
func (r *Runtime) kickLocked() {
	if r.pumping || !r.canStartLocked() {
		return
	}
	r.pumping = true
	go r.pump()
}

// pump starts queued input one at a time while the slot is free, moving
// past input whose start failed or ended the turn at once.
func (r *Runtime) pump() {
	for {
		r.mu.Lock()
		if !r.canStartLocked() {
			r.pumping = false
			r.mu.Unlock()
			return
		}
		next := r.inputs[0]
		r.inputs[0] = queuedInput{}
		r.inputs = r.inputs[1:]
		r.running = next.id
		r.starting = append(r.starting, next.id)
		start := r.starter
		r.mu.Unlock()
		_ = r.runStart(r.ctx, next.id, func(ctx context.Context) error { return start(ctx, next.id, next.in) })
	}
}
