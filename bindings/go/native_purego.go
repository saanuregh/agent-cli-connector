//go:build !(cgo && !agent_use_lib && !android && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64)))

package agent

import "github.com/saanuregh/agent-cli-connector/bindings/go/internal/sharedlib"

// connectionPointer is how this backend holds a connection's C pointer.
type connectionPointer = uintptr

// loadLibrary finds, loads, and binds the shared library once per process.
func loadLibrary() error { return sharedlib.Load(LibraryVersion) }

func agentOpen() uintptr { return sharedlib.Open() }

func agentSend(conn uintptr, msg *byte, length uintptr) int32 {
	return sharedlib.Send(conn, msg, length)
}

func agentRecvBatch(conn uintptr, timeoutMS int32, out **byte, length *uintptr) int32 {
	return sharedlib.RecvBatch(conn, timeoutMS, out, length)
}

func agentFree(msg *byte) { sharedlib.Free(msg) }

func agentClose(conn uintptr) { sharedlib.Close(conn) }

func agentDestroy(conn uintptr) { sharedlib.Destroy(conn) }

func agentVersion() string { return sharedlib.Version() }
