//go:build !(cgo && !agent_use_lib && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64)))

package agent

import "syscall"

func openLibrary(path string) (uintptr, error) {
	handle, err := syscall.LoadLibrary(path)
	return uintptr(handle), err
}
