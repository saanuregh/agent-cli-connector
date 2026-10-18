package providerkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// ProcessOptions configures StartProcess.
type ProcessOptions struct {
	// Provider names the harness in errors and logs.
	Provider string
	// Spawner starts the process. Nil means agent.LocalSpawner.
	Spawner agent.Spawner
	Command agent.Command
	Logger  *slog.Logger
	// WireTap receives every protocol line in both directions.
	WireTap func(dir agent.WireDirection, line []byte)
	// StderrLines is how many trailing stderr lines to keep for error
	// reports. Zero means 40.
	StderrLines int
}

// Process is a running harness that exchanges newline-delimited messages
// over stdin and stdout.
type Process struct {
	opts   ProcessOptions
	proc   agent.Process
	logger *slog.Logger

	writeMu sync.Mutex
	stdin   io.WriteCloser
	writer  *bufio.Writer
	closed  atomic.Bool

	stderrMu   sync.Mutex
	stderrTail []string
	stderrDone chan struct{}
	stdoutDone chan struct{}
	readOnce   sync.Once

	exited  chan struct{}
	waitErr error
}

// StartProcess starts the harness and begins draining its stderr.
func StartProcess(ctx context.Context, opts ProcessOptions) (*Process, error) {
	spawner := opts.Spawner
	if spawner == nil {
		spawner = agent.LocalSpawner
	}
	if opts.StderrLines <= 0 {
		opts.StderrLines = 40
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	proc, err := spawner.Spawn(ctx, opts.Command)
	if err != nil {
		if isExecutableNotFound(err) {
			return nil, fmt.Errorf("%s: executable %q not found: %w", opts.Provider, opts.Command.Path, err)
		}
		return nil, fmt.Errorf("%s: start %q: %w", opts.Provider, opts.Command.Path, err)
	}
	p := &Process{
		opts:       opts,
		proc:       proc,
		logger:     logger,
		stdin:      proc.Stdin(),
		writer:     bufio.NewWriterSize(proc.Stdin(), 64<<10),
		stderrDone: make(chan struct{}),
		stdoutDone: make(chan struct{}),
		exited:     make(chan struct{}),
	}
	go p.drainStderr()
	go func() {
		// os/exec requires every pipe read to finish before Wait. A
		// process that reports its own exit is waited for once it exits,
		// after a grace period for reading what it wrote: a descendant
		// that left its process group can hold the pipes open forever.
		if n, ok := proc.(exitNotifier); ok {
			pipesClosed := bothClosed(p.stderrDone, p.stdoutDone)
			select {
			case <-pipesClosed:
			case <-n.Done():
				waitFor(pipesClosed, 2*time.Second)
			}
		} else {
			<-p.stderrDone
			<-p.stdoutDone
		}
		p.waitErr = proc.Wait()
		close(p.exited)
	}()
	return p, nil
}

// exitNotifier is implemented by processes, such as agent.LocalSpawner's,
// that report their exit before their output pipes close. Their Wait closes
// the pipes.
type exitNotifier interface {
	Done() <-chan struct{}
}

func bothClosed(a, b <-chan struct{}) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		<-a
		<-b
		close(ch)
	}()
	return ch
}

func (p *Process) drainStderr() {
	defer close(p.stderrDone)
	r := bufio.NewReaderSize(p.proc.Stderr(), 16<<10)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			p.logger.Debug("harness stderr", "line", line)
			p.stderrMu.Lock()
			p.stderrTail = append(p.stderrTail, line)
			if len(p.stderrTail) > p.opts.StderrLines {
				p.stderrTail = p.stderrTail[len(p.stderrTail)-p.opts.StderrLines:]
			}
			p.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// StderrTail returns the last lines the harness wrote to stderr.
func (p *Process) StderrTail() string {
	p.stderrMu.Lock()
	defer p.stderrMu.Unlock()
	return strings.Join(p.stderrTail, "\n")
}

// WriteLine writes one message followed by a newline. Writes from
// different goroutines do not interleave.
func (p *Process) WriteLine(line []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.closed.Load() {
		return agent.ErrClosed
	}
	if tap := p.opts.WireTap; tap != nil {
		tap(agent.WireSend, line)
	}
	if _, err := p.writer.Write(line); err != nil {
		return p.writeErr(err)
	}
	if err := p.writer.WriteByte('\n'); err != nil {
		return p.writeErr(err)
	}
	if err := p.writer.Flush(); err != nil {
		return p.writeErr(err)
	}
	return nil
}

func (p *Process) writeErr(err error) error {
	if exitErr := p.ExitError(); exitErr != nil {
		return exitErr
	}
	return fmt.Errorf("%s: write to harness: %w", p.opts.Provider, err)
}

// ExitError describes the process exit as an *agent.ExitError, or returns
// nil while the process runs.
func (p *Process) ExitError() error {
	select {
	case <-p.exited:
		return &agent.ExitError{Provider: p.opts.Provider, Err: p.waitErr, Stderr: p.StderrTail()}
	default:
		return nil
	}
}

// WriteJSON encodes v and writes it as one line.
func (p *Process) WriteJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: encode message: %w", p.opts.Provider, err)
	}
	return p.WriteLine(data)
}

// ReadLines calls handle for every stdout line until stdout ends, and then
// returns the reason. Lines are split only on LF, never on Unicode line
// separators, and have any trailing CR removed. Blank lines are skipped.
// Each line slice is owned by the callee. ReadLines must be called exactly
// once; the process is not reaped until it returns.
func (p *Process) ReadLines(handle func(line []byte)) error {
	defer p.readOnce.Do(func() { close(p.stdoutDone) })
	r := bufio.NewReaderSize(p.proc.Stdout(), 256<<10)
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) > 0 {
			if tap := p.opts.WireTap; tap != nil {
				tap(agent.WireReceive, line)
			}
			handle(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
	}
}

// CloseStdin closes the harness input, which asks most harnesses to exit.
// It does not wait for a write in progress: closing the pipe makes a write
// blocked on a harness that stopped reading fail instead of hanging.
func (p *Process) CloseStdin() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	return p.stdin.Close()
}

// Exited is closed after the process exited.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Stop shuts the process down: it closes stdin, waits up to grace for the
// process to exit, then terminates it, and finally kills it. It returns
// after the process exited.
func (p *Process) Stop(grace time.Duration) {
	_ = p.CloseStdin()
	if waitFor(p.exited, grace) {
		return
	}
	_ = p.proc.Terminate()
	if waitFor(p.exited, 2*time.Second) {
		return
	}
	_ = p.proc.Kill()
	<-p.exited
}

func waitFor(ch <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}
