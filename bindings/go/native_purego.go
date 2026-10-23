//go:build !(cgo && !agent_use_lib && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64)))

package agent

import (
	"fmt"
	"os"
	"sync"

	"github.com/ebitengine/purego"
)

// libraryPathVariable names the environment variable that points at the
// shared library and skips the cache and the download.
const libraryPathVariable = "AGENT_CLI_CONNECTOR_LIB"

// connectionPointer is how this backend holds a connection's C pointer.
type connectionPointer = uintptr

// The C functions of include/agent.h, bound by loadLibrary. A connection
// pointer is C memory, so it is held as a uintptr.
var (
	agentOpen      func() uintptr
	agentSend      func(conn uintptr, msg *byte, length uintptr) int32
	agentRecvBatch func(conn uintptr, timeoutMS int32, out **byte, length *uintptr) int32
	agentFree      func(msg *byte)
	agentClose     func(conn uintptr)
	agentDestroy   func(conn uintptr)
	agentVersion   func() string
)

var (
	libraryMu     sync.Mutex
	libraryLoaded bool
)

// loadLibrary finds, loads, and binds the shared library once per process.
// A failure is not remembered, so a later call can retry, for example after
// a network error.
func loadLibrary() error {
	libraryMu.Lock()
	defer libraryMu.Unlock()
	if libraryLoaded {
		return nil
	}
	path, err := findLibrary()
	if err != nil {
		return err
	}
	handle, err := openLibrary(path)
	if err != nil {
		return fmt.Errorf("agent: load the native library %s: %w (set %s to a working copy)", path, err, libraryPathVariable)
	}
	if err := bindFunctions(handle); err != nil {
		return fmt.Errorf("agent: native library %s: %w", path, err)
	}
	libraryLoaded = true
	return nil
}

// bindFunctions turns the panic of purego.RegisterLibFunc on a missing
// symbol into an error.
func bindFunctions(handle uintptr) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("missing symbol: %v", r)
		}
	}()
	purego.RegisterLibFunc(&agentOpen, handle, "agent_open")
	purego.RegisterLibFunc(&agentSend, handle, "agent_send")
	purego.RegisterLibFunc(&agentRecvBatch, handle, "agent_recv_batch")
	purego.RegisterLibFunc(&agentFree, handle, "agent_free")
	purego.RegisterLibFunc(&agentClose, handle, "agent_close")
	purego.RegisterLibFunc(&agentDestroy, handle, "agent_destroy")
	purego.RegisterLibFunc(&agentVersion, handle, "agent_version")
	return nil
}

// findLibrary returns the path of the shared library: the environment
// variable, then the cache, then a verified download into the cache.
func findLibrary() (string, error) {
	if path := os.Getenv(libraryPathVariable); path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("agent: %s: %w", libraryPathVariable, err)
		}
		return path, nil
	}
	return cachedLibrary()
}
