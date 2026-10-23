//go:build cgo && !agent_use_lib && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64))

package agent

// #include "agent.h"
import "C"

import "unsafe"

// connectionPointer is how this backend holds a connection's C pointer.
type connectionPointer = *C.agent_connection

// loadLibrary has nothing to load: the static library of this platform's
// bindings/go/lib module is linked in.
func loadLibrary() error { return nil }

// The C functions of include/agent.h, with the types native.go uses.

func agentOpen() connectionPointer { return C.agent_open() }

func agentSend(conn connectionPointer, msg *byte, length uintptr) int32 {
	return int32(C.agent_send(conn, (*C.char)(unsafe.Pointer(msg)), C.size_t(length)))
}

func agentRecvBatch(conn connectionPointer, timeoutMS int32, out **byte, length *uintptr) int32 {
	return int32(C.agent_recv_batch(conn, C.int(timeoutMS), (**C.char)(unsafe.Pointer(out)), (*C.size_t)(unsafe.Pointer(length))))
}

func agentFree(msg *byte) { C.agent_free((*C.char)(unsafe.Pointer(msg))) }

func agentClose(conn connectionPointer) { C.agent_close(conn) }

func agentDestroy(conn connectionPointer) { C.agent_destroy(conn) }

func agentVersion() string { return C.GoString(C.agent_version()) }
