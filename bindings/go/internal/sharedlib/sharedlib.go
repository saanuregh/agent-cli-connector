// Package sharedlib finds, loads, and binds the shared native library at run
// time without cgo, through purego. The agent package uses it when the
// static library is not linked in (see native_purego.go).
package sharedlib

import (
	"fmt"
	"os"
	"sync"

	"github.com/ebitengine/purego"
)

// libraryPathVariable names the environment variable that points at the
// shared library and skips the cache and the download.
const libraryPathVariable = "AGENT_CLI_CONNECTOR_LIB"

// The C functions of include/agent.h, bound by Load. A connection pointer is
// C memory, so it is held as a uintptr.
var (
	Open      func() uintptr
	Send      func(conn uintptr, msg *byte, length uintptr) int32
	RecvBatch func(conn uintptr, timeoutMS int32, out **byte, length *uintptr) int32
	Free      func(msg *byte)
	Close     func(conn uintptr)
	Destroy   func(conn uintptr)
	Version   func() string
)

var (
	libraryMu     sync.Mutex
	libraryLoaded bool
)

// Load finds, loads, and binds the shared library of the given version once
// per process. A failure is not remembered, so a later call can retry, for
// example after a network error.
func Load(version string) error {
	libraryMu.Lock()
	defer libraryMu.Unlock()
	if libraryLoaded {
		return nil
	}
	path, err := findLibrary(version)
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
	purego.RegisterLibFunc(&Open, handle, "agent_open")
	purego.RegisterLibFunc(&Send, handle, "agent_send")
	purego.RegisterLibFunc(&RecvBatch, handle, "agent_recv_batch")
	purego.RegisterLibFunc(&Free, handle, "agent_free")
	purego.RegisterLibFunc(&Close, handle, "agent_close")
	purego.RegisterLibFunc(&Destroy, handle, "agent_destroy")
	purego.RegisterLibFunc(&Version, handle, "agent_version")
	return nil
}

// findLibrary returns the path of the shared library: the environment
// variable, then the cache, then a verified download into the cache.
func findLibrary(version string) (string, error) {
	if path := os.Getenv(libraryPathVariable); path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("agent: %s: %w", libraryPathVariable, err)
		}
		return path, nil
	}
	return cachedLibrary(version)
}
