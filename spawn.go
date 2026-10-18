package agent

import (
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/saanuregh/agent-cli-connector/internal/envutil"
)

// Command describes a harness process to start.
type Command struct {
	// Path is the executable name or path.
	Path string
	Args []string
	// Env lists "KEY=value" variables to set on top of the environment the
	// spawner starts from. LocalSpawner starts from this process's
	// environment.
	Env []string
	// UnsetEnv lists variables to remove from the environment the spawner
	// starts from.
	UnsetEnv []string
	// Dir is the working directory. Empty means the spawner's default.
	Dir string
}

// Process is a started harness process.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	// Wait waits for the process to exit and releases its resources.
	Wait() error
	// Terminate asks the process to stop, such as with SIGTERM.
	Terminate() error
	// Kill stops the process and its children immediately.
	Kill() error
}

// Spawner starts harness processes. Replace the default to run harnesses
// in containers, on remote machines, or under a sandbox.
type Spawner interface {
	// Spawn starts cmd. ctx bounds only the start, not the process's life.
	Spawn(ctx context.Context, cmd Command) (Process, error)
}

// SpawnFunc adapts a function to Spawner.
type SpawnFunc func(ctx context.Context, cmd Command) (Process, error)

// Spawn calls f.
func (f SpawnFunc) Spawn(ctx context.Context, cmd Command) (Process, error) { return f(ctx, cmd) }

// LocalSpawner starts processes on this machine with os/exec. On Unix each
// process gets its own process group so Kill also stops its children.
var LocalSpawner Spawner = localSpawner{}

type localSpawner struct{}

func (localSpawner) Spawn(ctx context.Context, c Command) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = envutil.Merge(envutil.Unset(os.Environ(), c.UnsetEnv), c.Env)
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Files instead of StdoutPipe let the exit be noticed while a child the
	// harness started still holds the pipes open; see Done.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		stdout.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	err = cmd.Start()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		stdout.Close()
		stderr.Close()
		return nil, err
	}
	p := &localProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		// Kill what is left of the process group, such as MCP servers the
		// harness started, which would otherwise outlive it and keep its
		// output pipes open.
		_ = killProcess(cmd)
		close(p.exited)
	}()
	return p, nil
}

type localProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *os.File
	stderr  *os.File
	exited  chan struct{}
	waitErr error
}

func (p *localProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *localProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *localProcess) Stderr() io.ReadCloser { return p.stderr }

// Done is closed when the process exited, possibly before its output pipes
// close.
func (p *localProcess) Done() <-chan struct{} { return p.exited }

// Wait waits for the process to exit and closes its output pipes, which
// ends reads blocked on them.
func (p *localProcess) Wait() error {
	<-p.exited
	p.stdout.Close()
	p.stderr.Close()
	return p.waitErr
}

func (p *localProcess) Terminate() error { return terminateProcess(p.cmd) }
func (p *localProcess) Kill() error      { return killProcess(p.cmd) }
