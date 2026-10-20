// Command agentd-run runs on the machine where agentd's harnesses run, such
// as inside a container or on an ssh host. agentd starts it through a
// wrapper and sends it everything over standard input, so the wrapper
// needs no quoting and no environment forwarding:
//
//	"Spawner": {"Wrapper": ["ssh", "build-box", "agentd-run"], "Runner": true}
//	"Spawner": {"Wrapper": ["docker", "exec", "-i", "box", "agentd-run"], "Runner": true}
//
// agentd-run --exec starts a harness: it writes the files the harness
// reads, applies its environment and working directory, and replaces
// itself with the harness (on Windows, runs it and passes its exit code
// on). agentd-run --fs reads one file or directory for agentd, which is
// how session lists and history work for harnesses on that machine.
//
// Build it for the far side with GOOS and GOARCH, for example
// GOOS=linux GOARCH=amd64 go build ./cmd/agentd-run.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/saanuregh/agent-cli-connector/internal/agentrun"
)

func main() {
	if len(os.Args) != 2 {
		usage()
	}
	switch os.Args[1] {
	case "--exec":
		if err := execHarness(); err != nil {
			fmt.Fprintln(os.Stderr, "agentd-run:", err)
			os.Exit(1)
		}
	case "--fs":
		if err := agentrun.ServeFS(os.Stdin, os.Stdout); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				os.Exit(agentrun.ExitNotExist)
			}
			fmt.Fprintln(os.Stderr, "agentd-run:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentd-run --exec|--fs (agentd sends the rest on standard input)")
	os.Exit(2)
}

// execHarness reads the setup line and starts the harness it names.
func execHarness() error {
	line, err := agentrun.ReadLine(os.Stdin)
	if err != nil {
		return fmt.Errorf("read setup: %w", err)
	}
	var s agentrun.Setup
	if err := json.Unmarshal(line, &s); err != nil {
		return fmt.Errorf("decode setup: %w", err)
	}
	if s.Path == "" {
		return errors.New("setup names no command")
	}
	if err := agentrun.WriteFiles(s.Files); err != nil {
		return fmt.Errorf("write files: %w", err)
	}
	if err := agentrun.ApplyEnv(s); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	if s.Dir != "" {
		if err := os.Chdir(s.Dir); err != nil {
			return err
		}
	}
	return run(s.Path, s.Args)
}
