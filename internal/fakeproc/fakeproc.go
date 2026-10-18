// Package fakeproc runs scripted in-process harnesses behind the
// agent.Spawner interface, so provider tests exercise the real protocol
// code without starting real CLIs.
package fakeproc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Harness is the scripted side of a fake process.
type Harness struct {
	t      testing.TB
	Cmd    agent.Command
	in     *bufio.Reader
	out    io.WriteCloser
	errOut io.WriteCloser
	mu     sync.Mutex
	lines  chan []byte
	// held keeps messages RecvWhere skipped, for later calls.
	held []map[string]any
}

// Spawner returns a spawner whose processes run script on a goroutine. The
// process exits when script returns. Each spawned harness is also sent on
// the returned channel.
func Spawner(t testing.TB, script func(h *Harness)) (agent.Spawner, <-chan *Harness) {
	started := make(chan *Harness, 16)
	return agent.SpawnFunc(func(ctx context.Context, cmd agent.Command) (agent.Process, error) {
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		stderrR, stderrW := io.Pipe()
		h := &Harness{t: t, Cmd: cmd, in: bufio.NewReader(stdinR), out: stdoutW, errOut: stderrW, lines: make(chan []byte, 256)}
		p := &process{stdin: stdinW, stdout: stdoutR, stderr: stderrR, done: make(chan struct{}), stdinR: stdinR}
		go func() {
			for {
				line, err := h.in.ReadBytes('\n')
				if len(line) > 0 {
					h.lines <- line[:len(line)-1]
				}
				if err != nil {
					close(h.lines)
					return
				}
			}
		}()
		go func() {
			defer close(p.done)
			defer stderrW.Close()
			defer stdoutW.Close()
			script(h)
		}()
		select {
		case started <- h:
		default:
		}
		return p, nil
	}), started
}

// Send writes v as one JSON line.
func (h *Harness) Send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		h.t.Errorf("fakeproc: encode: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.out.Write(append(data, '\n'))
}

// SendRaw writes a raw line.
func (h *Harness) SendRaw(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.out.Write([]byte(line + "\n"))
}

// Reply answers the JSON-RPC request req with result.
func (h *Harness) Reply(req map[string]any, result any) {
	h.Send(map[string]any{"id": req["id"], "result": result})
}

// Notify sends a JSON-RPC notification.
func (h *Harness) Notify(method string, params map[string]any) {
	h.Send(map[string]any{"method": method, "params": params})
}

// Method matches a JSON-RPC message with the given method, for RecvWhere.
func Method(name string) func(map[string]any) bool {
	return func(m map[string]any) bool { return Str(m, "method") == name }
}

// Stderr writes a line to stderr.
func (h *Harness) Stderr(line string) {
	_, _ = h.errOut.Write([]byte(line + "\n"))
}

// Recv returns the next message from the client decoded as a map, or nil
// when stdin closed or the wait timed out.
func (h *Harness) Recv() map[string]any {
	select {
	case line, ok := <-h.lines:
		if !ok {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			h.t.Errorf("fakeproc: client sent invalid JSON %q: %v", line, err)
			return nil
		}
		return m
	case <-time.After(10 * time.Second):
		h.t.Errorf("fakeproc: timed out waiting for client message")
		return nil
	}
}

// RecvWhere returns the next client message for which match is true.
// Messages that do not match are kept for later RecvWhere calls.
func (h *Harness) RecvWhere(match func(map[string]any) bool) map[string]any {
	for i, m := range h.held {
		if match(m) {
			h.held = append(h.held[:i], h.held[i+1:]...)
			return m
		}
	}
	for {
		m := h.Recv()
		if m == nil || match(m) {
			return m
		}
		h.held = append(h.held, m)
	}
}

// WaitClosed waits until the client closes stdin.
func (h *Harness) WaitClosed() {
	for range h.lines {
	}
}

type process struct {
	stdin  *io.PipeWriter
	stdinR *io.PipeReader
	stdout *io.PipeReader
	stderr *io.PipeReader
	done   chan struct{}
}

func (p *process) Stdin() io.WriteCloser { return p.stdin }
func (p *process) Stdout() io.ReadCloser { return p.stdout }
func (p *process) Stderr() io.ReadCloser { return p.stderr }
func (p *process) Wait() error {
	<-p.done
	return nil
}
func (p *process) Terminate() error { return p.Kill() }
func (p *process) Kill() error {
	_ = p.stdinR.Close()
	_ = p.stdout.Close()
	_ = p.stderr.Close()
	return nil
}

// Get walks a decoded JSON value by keys and indexes.
func Get(v any, path ...any) any {
	for _, p := range path {
		switch key := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[key]
		case int:
			a, ok := v.([]any)
			if !ok || key >= len(a) {
				return nil
			}
			v = a[key]
		}
	}
	return v
}

// Str returns Get as a string.
func Str(v any, path ...any) string {
	s, _ := Get(v, path...).(string)
	return s
}
