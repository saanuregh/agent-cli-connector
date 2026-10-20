package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/jsonfields"
	"github.com/saanuregh/agent-cli-connector/internal/jsonrpc"
)

// JSON-RPC error codes agentd returns.
const (
	codeParseError    = -32700
	codeCancelled     = -32800
	codeUnknownMethod = -32601
	codeInvalidParams = -32602
	codeLibrary       = -32000
)

// errorKinds classifies library errors, first match wins.
var errorKinds = []struct {
	target error
	kind   string
}{
	{agent.ErrUnsupported, "unsupported"},
	{agent.ErrSessionNotFound, "session_not_found"},
	{agent.ErrInterrupted, "interrupted"},
	{agent.ErrCancelled, "cancelled"},
	{agent.ErrClosed, "closed"},
	{agent.ErrRequestDone, "request_done"},
	{agent.ErrInvalidAnswer, "invalid_answer"},
	{agent.ErrSteerRejected, "steer_rejected"},
}

// kindError is an error agentd itself raises, with its kind.
type kindError struct {
	kind string
	msg  string
}

func (e *kindError) Error() string { return e.msg }

func newError(kind, msg string) error { return &kindError{kind: kind, msg: msg} }

// errorInfo describes err for the wire, or returns nil for a nil err.
func errorInfo(err error) *ErrorInfo {
	if err == nil {
		return nil
	}
	info := &ErrorInfo{Message: err.Error(), Kind: errorKind(err)}
	if te, ok := errors.AsType[*agent.TurnError](err); ok {
		info.Turn = &TurnErrorWire{TurnError: te, Err: errorInfo(te.Err)}
	}
	if pe, ok := errors.AsType[*agent.ProtocolError](err); ok {
		info.Protocol = pe
	}
	if ee, ok := errors.AsType[*agent.ExitError](err); ok {
		info.Exit = &ExitErrorWire{ExitError: ee, Err: errorInfo(ee.Err)}
	}
	return info
}

func errorKind(err error) string {
	if ke, ok := errors.AsType[*kindError](err); ok {
		return ke.kind
	}
	for _, k := range errorKinds {
		if errors.Is(err, k.target) {
			return k.kind
		}
	}
	if _, ok := errors.AsType[*agent.ProtocolError](err); ok {
		return "protocol"
	}
	if _, ok := errors.AsType[*agent.ExitError](err); ok {
		return "exit"
	}
	return ""
}

// rpcError converts a method's error into the JSON-RPC error sent back.
func rpcError(err error) *jsonrpc.Error {
	if rpcErr, ok := errors.AsType[*jsonrpc.Error](err); ok {
		return rpcErr
	}
	info := errorInfo(err)
	code := codeLibrary
	switch info.Kind {
	case "invalid_params":
		code = codeInvalidParams
	case "cancelled":
		code = codeCancelled
	}
	data, _ := json.Marshal(info)
	return &jsonrpc.Error{Code: code, Message: info.Message, Data: data}
}

// turnResultWire converts a turn result for the wire.
func turnResultWire(res *agent.TurnResult) *TurnResultWire {
	if res == nil {
		return nil
	}
	return &TurnResultWire{TurnResult: res, Err: errorInfo(res.Err)}
}

// wireEvent returns ev in the form agentd encodes it.
func wireEvent(ev agent.Event) any {
	if tc, ok := ev.(*agent.TurnCompleted); ok {
		return &TurnCompletedWire{TurnCompleted: tc, Result: turnResultWire(tc.Result)}
	}
	return ev
}

// encode marshals v. Without includeRaw, every field named Raw of type
// json.RawMessage is removed, except the payload of an agent.RawEvent,
// which is all such an event carries. Removal follows v's Go value rather
// than key names, so a "Raw" key inside tool arguments or other free-form
// JSON is kept.
func (s *server) encode(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil || s.includeRaw.Load() {
		return data, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if !stripRaw(reflect.ValueOf(v), tree) {
		return data, nil
	}
	return json.Marshal(tree)
}

var (
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	rawEventType   = reflect.TypeFor[agent.RawEvent]()
)

// stripRaw removes Raw fields from tree, the decoded JSON of v, and
// reports whether it removed any.
func stripRaw(v reflect.Value, tree any) bool {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	changed := false
	switch v.Kind() {
	case reflect.Struct:
		obj, ok := tree.(map[string]any)
		if !ok {
			return false
		}
		for _, f := range jsonfields.Of(v.Type()) {
			val, present := obj[f.Name]
			if !present {
				continue
			}
			if f.Name == "Raw" && f.StructField.Type == rawMessageType && v.Type() != rawEventType {
				delete(obj, f.Name)
				changed = true
				continue
			}
			fv, err := v.FieldByIndexErr(f.StructField.Index)
			if err == nil && stripRaw(fv, val) {
				changed = true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		list, ok := tree.([]any)
		if !ok {
			return false
		}
		for i := 0; i < v.Len() && i < len(list); i++ {
			if stripRaw(v.Index(i), list[i]) {
				changed = true
			}
		}
	case reflect.Map:
		obj, ok := tree.(map[string]any)
		if !ok || v.Type().Key().Kind() != reflect.String {
			return false
		}
		iter := v.MapRange()
		for iter.Next() {
			if val, present := obj[iter.Key().String()]; present && stripRaw(iter.Value(), val) {
				changed = true
			}
		}
	}
	return changed
}
