package agent

import "fmt"

// Error is a failure reported by the library. Match its class with
// errors.Is against the Err* sentinels, or read the details with errors.As.
type Error struct {
	ErrorInfo
	// Method is the protocol method that failed, when known.
	Method string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = string(e.Kind)
	}
	if e.Method != "" {
		msg = e.Method + ": " + msg
	}
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	return "agent: " + msg
}

// Is reports whether target is the sentinel of e's kind.
func (e *Error) Is(target error) bool {
	k, ok := target.(errorKindSentinel)
	return ok && ErrorKind(k) == e.Kind
}

type errorKindSentinel ErrorKind

func (k errorKindSentinel) Error() string { return fmt.Sprintf("agent: %s", string(k)) }

// Sentinels for errors.Is, one per ErrorKind.
var (
	// ErrUnsupported: the harness lacks the feature.
	ErrUnsupported error = errorKindSentinel(ErrorKindUnsupported)
	// ErrClosed: the session or client is closed.
	ErrClosed error = errorKindSentinel(ErrorKindClosed)
	// ErrCancelled: queued input was dropped before it ran.
	ErrCancelled error = errorKindSentinel(ErrorKindCancelled)
	// ErrRequestDone: the request was already answered or withdrawn.
	ErrRequestDone error = errorKindSentinel(ErrorKindRequestDone)
	// ErrSessionNotFound: the harness does not have the session.
	ErrSessionNotFound error = errorKindSentinel(ErrorKindSessionNotFound)
	// ErrSteerRejected: the input was not injected and can be sent again.
	ErrSteerRejected error = errorKindSentinel(ErrorKindSteerRejected)
	// ErrInterrupted: the turn was interrupted.
	ErrInterrupted error = errorKindSentinel(ErrorKindInterrupted)
	// ErrInvalidAnswer: an answer to a request could not be used.
	ErrInvalidAnswer error = errorKindSentinel(ErrorKindInvalidAnswer)
	// ErrInvalidParams: a request had invalid parameters.
	ErrInvalidParams error = errorKindSentinel(ErrorKindInvalidParams)
	// ErrNotFound: a named provider, turn, task, or file does not exist.
	ErrNotFound error = errorKindSentinel(ErrorKindNotFound)
	// ErrProtocol: the harness sent something the library cannot read.
	ErrProtocol error = errorKindSentinel(ErrorKindProtocol)
	// ErrExit: the harness process exited.
	ErrExit error = errorKindSentinel(ErrorKindExit)
	// ErrTurnFailed: the turn ran and failed.
	ErrTurnFailed error = errorKindSentinel(ErrorKindTurnFailed)
	// ErrTimeout: the operation timed out.
	ErrTimeout error = errorKindSentinel(ErrorKindTimeout)
	// ErrInternal: a library failure.
	ErrInternal error = errorKindSentinel(ErrorKindInternal)
)

func closedError(method string) *Error {
	return &Error{ErrorInfo: ErrorInfo{Kind: ErrorKindClosed, Message: "client closed"}, Method: method}
}
