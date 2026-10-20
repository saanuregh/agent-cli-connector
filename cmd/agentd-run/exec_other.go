//go:build !unix

package main

import (
	"errors"
	"os"
	"os/exec"
)

// run starts the harness with this process's standard streams, waits, and
// exits with its code, because these systems cannot replace a process.
func run(path string, args []string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		os.Exit(exitErr.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
