package agent

/*
#cgo CFLAGS: -I${SRCDIR}/../../include
#cgo LDFLAGS: -L${SRCDIR}/../../zig-out/lib -l:libagent-cli-connector.a -lpthread -lm
// The Zig objects carry no .note.GNU-stack section, which GNU ld would read
// as a request for an executable stack.
#cgo linux LDFLAGS: -Wl,-z,noexecstack
#include <stdlib.h>
#include "agent.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// connection is the in-process JSON-RPC pipe of the native library.
type connection struct {
	ptr *C.agent_connection
}

func openConnection() (*connection, error) {
	ptr := C.agent_open()
	if ptr == nil {
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
	if C.agent_send(c.ptr, (*C.char)(unsafe.Pointer(&msg[0])), C.size_t(len(msg))) != 0 {
		return errors.New("agent: the native library refused a message")
	}
	return nil
}

// recv waits up to timeoutMS for messages and returns every queued one,
// joined by newlines. It returns nil and true on timeout, and nil and false
// once the connection is closed and drained.
func (c *connection) recv(timeoutMS int) ([]byte, bool) {
	var out *C.char
	var n C.size_t
	switch C.agent_recv_batch(c.ptr, C.int(timeoutMS), &out, &n) {
	case 1:
		msgs := C.GoBytes(unsafe.Pointer(out), C.int(n))
		C.agent_free(out)
		return msgs, true
	case 0:
		return nil, true
	default:
		return nil, false
	}
}

// shutdown closes every session and makes recv drain and report the end.
func (c *connection) shutdown() { C.agent_close(c.ptr) }

// destroy frees the connection. No goroutine may use it afterwards.
func (c *connection) destroy() { C.agent_destroy(c.ptr) }

// LibraryVersion returns the version of the linked native library.
func LibraryVersion() string { return C.GoString(C.agent_version()) }
