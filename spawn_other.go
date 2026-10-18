//go:build !unix

package agent

import "os/exec"

func setProcessGroup(*exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
