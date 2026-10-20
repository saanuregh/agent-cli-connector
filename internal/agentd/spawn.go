package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/agentrun"
	"github.com/saanuregh/agent-cli-connector/internal/envutil"
)

// SpawnerConfig chooses how a provider's harness processes start.
type SpawnerConfig struct {
	// Wrapper runs each harness through a command, such as
	// ["docker", "exec", "-i", "box", "run-harness"] or
	// ["ssh", "host", "run-harness"]: agentd runs the wrapper with the
	// harness's command and arguments appended. The wrapper runs in
	// agentd's directory with the harness's environment, and learns where
	// the harness should run from AGENTD_DIR (its working directory),
	// AGENTD_UNSET_ENV (a JSON array of variables to remove), and AGENTD_ENV
	// (a JSON object of variables to set), to apply on the far side in that
	// order: removals first, as agentd does locally. AGENTD_FILES is a JSON
	// array of {"Path", "Data" (base64)} files the harness reads, such as
	// Pi's bridge extension, to write at those paths before starting it.
	Wrapper []string
	// Runner says the wrapper ends in agentd-run (cmd/agentd-run) on the
	// far side, such as ["ssh", "box", "agentd-run"]. agentd then sends
	// the command, directory, environment, and files over its standard
	// input instead, so nothing needs quoting or forwarding, and reads
	// session files through it, so session lists and history work.
	Runner bool
}

// spawner returns the agent.Spawner c describes, or nil for the default.
func (c *SpawnerConfig) spawner() (agent.Spawner, error) {
	if c == nil {
		return nil, nil
	}
	if len(c.Wrapper) == 0 || c.Wrapper[0] == "" {
		return nil, newError("invalid_params", "Spawner.Wrapper needs a command")
	}
	return wrapperSpawner{wrapper: slices.Clone(c.Wrapper), runner: c.Runner}, nil
}

// wrapperSpawner starts harnesses through a wrapper command.
type wrapperSpawner struct {
	wrapper []string
	runner  bool
}

func (w wrapperSpawner) Spawn(ctx context.Context, c agent.Command) (agent.Process, error) {
	if !w.runner {
		return agent.LocalSpawner.Spawn(ctx, w.wrap(c))
	}
	proc, err := agent.LocalSpawner.Spawn(ctx, w.runnerCommand("--exec"))
	if err != nil {
		return nil, err
	}
	setup, err := json.Marshal(agentrun.Setup{Path: c.Path, Args: c.Args, Dir: c.Dir, Env: c.Env, UnsetEnv: c.UnsetEnv, Files: c.Files})
	if err == nil {
		// The write blocks while the wrapper is not reading yet, such as
		// ssh waiting at a prompt, so ctx bounds it like the rest of the
		// start.
		written := make(chan error, 1)
		go func() {
			_, err := proc.Stdin().Write(append(setup, '\n'))
			written <- err
		}()
		select {
		case err = <-written:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err != nil {
		_ = proc.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("send setup to agentd-run: %w", err)
	}
	return proc, nil
}

// FileSystem returns the far side's file system when the wrapper ends in
// agentd-run, else nil for this machine's.
func (w wrapperSpawner) FileSystem() agent.FileSystem {
	if !w.runner {
		return nil
	}
	return runnerFS{w: w}
}

// runnerCommand runs the wrapper with agentd-run's mode flag appended.
func (w wrapperSpawner) runnerCommand(mode string) agent.Command {
	return agent.Command{Path: w.wrapper[0], Args: append(slices.Clone(w.wrapper[1:]), mode)}
}

// wrap returns the command that runs c through the wrapper.
func (w wrapperSpawner) wrap(c agent.Command) agent.Command {
	env, _ := json.Marshal(envutil.Map(c.Env))
	unset, _ := json.Marshal(append([]string{}, c.UnsetEnv...))
	files, _ := json.Marshal(append([]agent.File{}, c.Files...))
	args := append(slices.Clone(w.wrapper[1:]), c.Path)
	return agent.Command{
		Path: w.wrapper[0],
		Args: append(args, c.Args...),
		Env: append(slices.Clone(c.Env),
			"AGENTD_DIR="+c.Dir, "AGENTD_ENV="+string(env), "AGENTD_UNSET_ENV="+string(unset), "AGENTD_FILES="+string(files)),
		UnsetEnv: c.UnsetEnv,
	}
}
