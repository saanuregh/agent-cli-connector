package agent

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	// ErrUnsupported is wrapped by errors for features a harness lacks.
	ErrUnsupported = errors.New("not supported")
	// ErrClosed is returned by calls on an ended session.
	ErrClosed = errors.New("session closed")
	// ErrCancelled is returned by Turn.Wait for queued input that was
	// cancelled before it ran.
	ErrCancelled = errors.New("input cancelled before it ran")
	// ErrRequestDone is returned when answering a request that was already
	// answered or withdrawn.
	ErrRequestDone = errors.New("request already answered or withdrawn")
	// ErrSessionNotFound is wrapped by errors for a Resume target the
	// harness does not have.
	ErrSessionNotFound = errors.New("session not found")
	// ErrSteerRejected is wrapped by Send errors when the harness refused
	// to inject input into the running turn, such as because the turn just
	// ended. The input was not delivered and can be sent again.
	ErrSteerRejected = errors.New("steer rejected")
	// ErrInterrupted is matched by the error of an interrupted turn.
	ErrInterrupted = errors.New("turn interrupted")
	// ErrInvalidAnswer is wrapped by Respond errors for an answer with an
	// empty or unknown decision or action, or a permission decision the
	// request does not offer. The request stays pending.
	ErrInvalidAnswer = errors.New("invalid answer")
)

// UnsupportedError reports a feature a provider does not offer.
type UnsupportedError struct {
	Provider string
	Feature  string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%s: %s is not supported", e.Provider, e.Feature)
}

// Is makes errors.Is(err, ErrUnsupported) true.
func (e *UnsupportedError) Is(target error) bool { return target == ErrUnsupported }

// ProtocolError is an error answer from the harness to a request.
type ProtocolError struct {
	Provider string
	// Method is the request that failed, such as "set_model" or
	// "turn/start".
	Method string
	// Code is the numeric error code: the JSON-RPC code for Codex, the
	// HTTP status for OpenCode, and zero when the harness sends none.
	Code    int
	Message string
	// ErrorCode is the harness's error class, such as Claude's error_code,
	// Codex's codexErrorInfo, or OpenCode's _tag, when present.
	ErrorCode string
	// Data holds extra error detail as JSON, when present.
	Data json.RawMessage
}

func (e *ProtocolError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("%s: %s failed (code %d): %s", e.Provider, e.Method, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s failed: %s", e.Provider, e.Method, e.Message)
}

// ExitError reports that the harness process ended unexpectedly.
type ExitError struct {
	Provider string
	// Err is the process wait error, or nil for a clean exit that was not
	// requested.
	Err error
	// Stderr is the tail of the process's standard error.
	Stderr string
}

func (e *ExitError) Error() string {
	msg := e.Provider + " process exited"
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Stderr != "" {
		msg += "\nstderr: " + e.Stderr
	}
	return msg
}

func (e *ExitError) Unwrap() error { return e.Err }
