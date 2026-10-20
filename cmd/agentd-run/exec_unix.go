//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// run replaces this process with the harness, which inherits standard
// input at the point the setup line ended.
func run(path string, args []string) error {
	resolved, err := exec.LookPath(path)
	if err != nil {
		return err
	}
	return syscall.Exec(resolved, append([]string{path}, args...), os.Environ())
}
