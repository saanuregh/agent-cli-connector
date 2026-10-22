package agent

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libraryPathVariable names the environment variable that points at the
// shared library and skips the cache and the download.
const libraryPathVariable = "AGENT_CLI_CONNECTOR_LIB"

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

// connection is the in-process JSON-RPC pipe of the native library.
type connection struct {
	ptr uintptr
}

func openConnection() (*connection, error) {
	if err := loadLibrary(); err != nil {
		return nil, err
	}
	ptr := agentOpen()
	if ptr == 0 {
		return nil, errors.New("agent: the native library could not open a connection")
	}
	return &connection{ptr: ptr}, nil
}

// send passes one JSON-RPC message to the library. The library copies it
// before returning.
func (c *connection) send(msg []byte) error {
	if len(msg) == 0 {
		return errors.New("agent: empty message")
	}
	if agentSend(c.ptr, &msg[0], uintptr(len(msg))) != 0 {
		return errors.New("agent: the native library refused a message")
	}
	return nil
}

// recv waits up to timeoutMS for messages and returns every queued one,
// joined by newlines. It returns nil and true on timeout, and nil and false
// once the connection is closed and drained.
func (c *connection) recv(timeoutMS int) ([]byte, bool) {
	var out *byte
	var n uintptr
	switch agentRecvBatch(c.ptr, int32(timeoutMS), &out, &n) {
	case 1:
		msgs := make([]byte, n)
		copy(msgs, unsafe.Slice(out, n))
		agentFree(out)
		return msgs, true
	case 0:
		return nil, true
	default:
		return nil, false
	}
}

// shutdown closes every session and makes recv drain and report the end.
func (c *connection) shutdown() { agentClose(c.ptr) }

// destroy frees the connection. No goroutine may use it afterwards.
func (c *connection) destroy() { agentDestroy(c.ptr) }

// LoadedLibraryVersion returns the version of the loaded native library,
// loading it first if no client has. It returns "" if the library cannot be
// loaded; [Open] reports why. [LibraryVersion] is the version these bindings
// expect.
func LoadedLibraryVersion() string {
	if loadLibrary() != nil {
		return ""
	}
	return agentVersion()
}
