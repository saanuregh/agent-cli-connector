package agent

import (
	"errors"
	"unsafe"
)

// The backends: native_static.go calls the static library linked in with
// cgo; native_purego.go loads the shared library at run time without cgo,
// through internal/sharedlib.
// Each provides connectionPointer, loadLibrary, and the agent* functions of
// include/agent.h used below.

// connection is the in-process JSON-RPC pipe of the native library.
type connection struct {
	ptr connectionPointer
}

func openConnection() (*connection, error) {
	if err := loadLibrary(); err != nil {
		return nil, err
	}
	ptr := agentOpen()
	var null connectionPointer
	if ptr == null {
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

// LoadedLibraryVersion returns the version of the native library in use:
// the static library linked in with cgo, or else the shared library, loaded
// first if no client has. It returns "" if the shared library cannot be
// loaded; [Open] reports why. [LibraryVersion] is the version these bindings
// expect.
func LoadedLibraryVersion() string {
	if loadLibrary() != nil {
		return ""
	}
	return agentVersion()
}
