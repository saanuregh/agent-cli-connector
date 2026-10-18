package providerkit

import (
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// eventQueue delivers events through a channel without ever blocking the
// producer. Producers are protocol readers; blocking them while a consumer
// is slow would stall answers to in-flight requests and can deadlock a
// consumer that calls the session from its event loop.
type eventQueue struct {
	mu      sync.Mutex
	items   []agent.Event
	head    int
	limit   int
	dropped int
	closed  bool
	notify  chan struct{}
	out     chan agent.Event
	started sync.Once
	meta    func() agent.EventMeta
}

func newEventQueue(limit int, meta func() agent.EventMeta) *eventQueue {
	return &eventQueue{
		limit:  limit,
		notify: make(chan struct{}, 1),
		out:    make(chan agent.Event),
		meta:   meta,
	}
}

// channel starts delivery on first use and returns the output channel.
// Delivery starts lazily so a session whose events are never read holds no
// goroutine.
func (q *eventQueue) channel() <-chan agent.Event {
	q.started.Do(func() { go q.pump() })
	return q.out
}

func (q *eventQueue) push(ev agent.Event) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	if q.limit > 0 && len(q.items)-q.head >= q.limit {
		q.dropOldestLocked()
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.wake()
}

// close stops accepting events. Queued events are still delivered, then
// the output channel is closed.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

// dropOldestLocked removes the oldest event that can be lost without
// harm. Requests and turn outcomes are never dropped, so the queue may
// exceed its limit when it holds nothing else.
func (q *eventQueue) dropOldestLocked() {
	for i := q.head; i < len(q.items); i++ {
		switch q.items[i].(type) {
		case agent.Request, *agent.TurnCompleted, *agent.SessionStarted:
			continue
		}
		if i == q.head {
			q.items[i] = nil
			q.head++
			q.dropped++
			return
		}
		copy(q.items[i:], q.items[i+1:])
		q.items[len(q.items)-1] = nil
		q.items = q.items[:len(q.items)-1]
		q.dropped++
		return
	}
}

func (q *eventQueue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *eventQueue) next() (agent.Event, bool) {
	for {
		q.mu.Lock()
		if dropped := q.dropped; dropped > 0 {
			q.dropped = 0
			q.mu.Unlock()
			// q.meta takes the runtime lock, which pushers hold while they
			// take q.mu. Only this goroutine dequeues, so the report still
			// precedes every item queued after the drops.
			meta := q.meta()
			meta.Time = time.Now()
			return &agent.EventsDropped{EventMeta: meta, Count: dropped}, true
		}
		if q.head < len(q.items) {
			ev := q.items[q.head]
			q.items[q.head] = nil
			q.head++
			if q.head == len(q.items) {
				q.items = q.items[:0]
				q.head = 0
			} else if q.head > 1024 && q.head*2 > len(q.items) {
				n := copy(q.items, q.items[q.head:])
				clear(q.items[n:])
				q.items = q.items[:n]
				q.head = 0
			}
			q.mu.Unlock()
			return ev, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, false
		}
		<-q.notify
	}
}

func (q *eventQueue) pump() {
	for {
		ev, ok := q.next()
		if !ok {
			close(q.out)
			return
		}
		q.out <- ev
	}
}
