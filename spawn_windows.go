//go:build windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// setProcessGroup starts the harness in its own process group, so a Ctrl+C
// in this program's console does not reach it, as Setpgid does on Unix.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateProcess asks the harness and the processes it started to exit.
// Console programs usually ignore the request, so Stop escalates to
// killProcess after its grace period.
func terminateProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return taskkill(cmd.Process.Pid, false)
}

// killProcess ends the harness and every process it started. Killing only
// the direct process would leave children running, such as node behind an
// npm .cmd shim or the MCP servers the harness started.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := taskkill(cmd.Process.Pid, true); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// taskkill runs taskkill /T on pid's process tree, by its System32 path so
// a taskkill earlier on PATH cannot stand in for it.
func taskkill(pid int, force bool) error {
	path := "taskkill.exe"
	if root := os.Getenv("SystemRoot"); root != "" {
		path = filepath.Join(root, "System32", "taskkill.exe")
	}
	args := []string{"/T", "/PID", strconv.Itoa(pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	return exec.Command(path, args...).Run()
}
