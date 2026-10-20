// Command protocolgen writes the JSON Schema of agentd's protocol and,
// with -ts and -py, the TypeScript and Python client types rendered from
// it. It reflects over the types internal/agentd describes and fails when
// a type reaches a field the wire cannot carry, such as an error or a
// func, that is neither shadowed by an agentd wire type nor on skipList.
//
//	go run ./internal/protocolgen -out cmd/agentd/protocol.schema.json \
//		-ts clients/typescript/src/protocol.ts -py clients/python/agentd/protocol.py
package main

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/internal/agentd"
	"github.com/saanuregh/agent-cli-connector/internal/jsonfields"
)

const modulePath = "github.com/saanuregh/agent-cli-connector"

// skipList holds the fields left out of the schema on purpose, keyed by
// pkg.Type.Field of the struct that declares them, with the reason. Any
// other func, chan, non-empty interface, *slog.Logger, or *http.Client
// field fails generation until someone decides how the wire carries it.
var skipList = map[string]string{
	"agent.Handlers.Permission":              "callback; clients declare handlers with StartParams.Handlers",
	"agent.Handlers.UserInput":               "callback; clients declare handlers with StartParams.Handlers",
	"agent.Handlers.Elicitation":             "callback; clients declare handlers with StartParams.Handlers",
	"claude.HookMatcher.Hooks":               "callbacks; agentd attaches one claude/hook reverse request per matcher",
	"agent.Config.Logger":                    "process-local logger",
	"agent.Config.WireTap":                   "callback; clients ask for wire/tap with StartParams.WireTap",
	"agent.Tool.Handler":                     "callback; agentd runs tools through tool/call",
	"claude.Options.Spawner":                 "process-local process launcher",
	"claude.SessionOptions.OnUserDialog":     "callback; clients ask for claude/userDialog with StartParams.OnUserDialog",
	"claude.SessionOptions.OnControlRequest": "callback; clients ask for claude/controlRequest with StartParams.OnControlRequest",
	"codex.Options.Spawner":                  "process-local process launcher",
	"codex.Options.OnAuthTokensRefresh":      "callback; clients ask for codex/authTokensRefresh with ProviderInit.OnAuthTokensRefresh",
	"codex.Options.Logger":                   "process-local logger",
	"codex.Options.WireTap":                  "callback; clients ask for wire/tap with ProviderInit.WireTap",
	"opencode.Options.Spawner":               "process-local process launcher",
	"opencode.Options.MapURL":                "callback with no wire form",
	"opencode.Options.HTTPClient":            "process-local HTTP client",
	"opencode.Options.HostToolsURL":          "callback with no wire form",
	"pi.Options.Spawner":                     "process-local process launcher",
}

// customMarshalers are the types with their own JSON encoding that the
// generator knows how to describe. Any other json.Marshaler or
// encoding.TextMarshaler fails generation, since reflection over its
// fields would describe the wrong form.
var customMarshalers = map[reflect.Type]string{
	reflect.TypeFor[time.Time]():       "",
	reflect.TypeFor[json.RawMessage](): "",
	// AgentDefinition adds its Extra map's keys and a numeric effort to
	// its tagged fields; additional properties are allowed by default.
	reflect.TypeFor[claude.AgentDefinition](): "",
}

// eventUnions are the interfaces whose values are events; a field of
// such a type encodes as one of the event types in agentd's table that
// implement it.
var eventUnions = map[reflect.Type]bool{
	reflect.TypeFor[agent.Request](): true,
}

var (
	errorType     = reflect.TypeFor[error]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
	textType      = reflect.TypeFor[encoding.TextMarshaler]()
	loggerType    = reflect.TypeFor[*slog.Logger]()
	httpType      = reflect.TypeFor[*http.Client]()
)

func main() {
	out := flag.String("out", "", "path of the schema file to write")
	tsOut := flag.String("ts", "", "path of the TypeScript types file to write, if any")
	pyOut := flag.String("py", "", "path of the Python types file to write, if any")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "protocolgen: -out is required")
		os.Exit(2)
	}
	err := run(*out, *tsOut, *pyOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, "protocolgen:", err)
		os.Exit(1)
	}
}

// run writes the schema to out and the client types rendered from it to
// the paths that are set, creating their directories.
func run(out, tsOut, pyOut string) error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	schemaData, err := generate(root)
	if err != nil {
		return err
	}
	files := []struct {
		path string
		data []byte
	}{{out, schemaData}}
	for _, c := range []struct {
		path   string
		render func([]byte) ([]byte, error)
	}{{tsOut, typescriptTypes}, {pyOut, pythonTypes}} {
		if c.path == "" {
			continue
		}
		data, err := c.render(schemaData)
		if err != nil {
			return err
		}
		files = append(files, struct {
			path string
			data []byte
		}{c.path, data})
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// moduleRoot finds the directory of go.mod above the working directory.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// schema is one JSON Schema object. encoding/json sorts its keys.
type schema = map[string]any

// generate returns the protocol schema, indented, with a trailing newline.
func generate(root string) ([]byte, error) {
	g := newGenerator(root)
	proto := agentd.Protocol()

	methods := map[string]any{}
	for name, m := range proto.Methods {
		result := schema{"type": "object", "maxProperties": 0}
		if m.Result != nil {
			result = g.schemaFor(m.Result, name+" result")
		}
		methods[name] = schema{"params": g.schemaFor(m.Params, name+" params"), "result": result}
	}
	notifications := map[string]any{}
	for name, n := range proto.Notifications {
		notifications[name] = schema{"direction": "agentd to client", "params": g.schemaFor(n.Params, name+" params")}
	}
	clientNotifications := map[string]any{}
	for name, n := range proto.ClientNotifications {
		clientNotifications[name] = schema{"direction": "client to agentd", "params": g.schemaFor(n.Params, name+" params")}
	}
	reverse := map[string]any{}
	for name, r := range proto.ReverseRequests {
		reverse[name] = schema{
			"direction": "agentd to client; the client replies with the result",
			"params":    g.schemaFor(r.Params, name+" params"),
			"result":    g.schemaFor(r.Result, name+" result"),
		}
	}
	events := map[string]any{}
	for _, ev := range agentd.EventTypes() {
		var t reflect.Type
		if _, ok := ev.(*agent.TurnCompleted); ok {
			t = reflect.TypeFor[agentd.TurnCompletedWire]()
		} else {
			t = reflect.TypeOf(ev).Elem()
		}
		events[agent.EventType(ev)] = g.schemaFor(t, "event "+agent.EventType(ev))
	}
	providers := map[string]any{}
	for _, p := range agentd.Defaults() {
		entry := schema{}
		for key, v := range map[string]any{"options": p.Options, "sessionOptions": p.SessionOptionsType, "turnOptions": p.TurnOptionsType} {
			if v != nil {
				entry[key] = g.schemaFor(reflect.TypeOf(v), p.Name+" "+key)
			}
		}
		providers[p.Name] = entry
	}
	kinds, err := g.errorKinds()
	if err != nil {
		return nil, err
	}
	errorsSection := schema{
		"description": "Failed requests get a JSON-RPC error {code, message, data}, where data is an ErrorInfo. A line that is not JSON gets a -32700 error with a null id.",
		"data":        g.schemaFor(reflect.TypeFor[agentd.ErrorInfo](), "errors data"),
		"codes": map[string]string{
			"-32700": "parse error (ErrorInfo.Kind is parse_error)",
			"-32800": "cancelled by the client's $/cancelRequest (ErrorInfo.Kind is cancelled)",
			"-32602": "invalid params (ErrorInfo.Kind is invalid_params)",
			"-32601": "unknown method (ErrorInfo.Kind is unknown_method)",
			"-32000": "library error",
		},
		"kinds": kinds,
	}
	for key := range skipList {
		if !g.skipUsed[key] {
			g.fail(key, "is on skipList but no longer reached; remove it")
		}
	}
	if err := g.err(); err != nil {
		return nil, err
	}

	doc := schema{
		"$schema":         "https://json-schema.org/draft/2020-12/schema",
		"title":           "agentd protocol",
		"protocolVersion": agentd.ProtocolVersion,
		"description": "JSON-RPC 2.0, one message per line; every message agentd writes has \"jsonrpc\":\"2.0\". " +
			"Property names are Go field names. All properties are optional on input; on output every property is present " +
			"unless its type marks it omitempty in Go, and nil pointers, slices, and maps are null. " +
			"Raw properties carrying native messages are removed unless initialize set IncludeRaw. " +
			"Methods without a result reply with an empty object. Requests run concurrently, except that requests after initialize wait for it and requests naming the same session run in arrival order, apart from request/respond, request/cancel, session/interrupt, session/interruptTurn, and session/close. String types list their known constants as examples; \"\" means unset, and providers may report other values.",
		"$defs":               g.defs,
		"methods":             methods,
		"notifications":       notifications,
		"clientNotifications": clientNotifications,
		"reverseRequests":     reverse,
		"events":              events,
		"providerOptions":     providers,
		"errors":              errorsSection,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// generator builds schemas for Go types and collects their definitions.
type generator struct {
	root     string
	defs     map[string]any
	skipUsed map[string]bool
	consts   map[string]map[string][]string // package dir → type name → string constants
	errs     []string
}

func newGenerator(root string) *generator {
	return &generator{root: root, defs: map[string]any{}, skipUsed: map[string]bool{}, consts: map[string]map[string][]string{}}
}

func (g *generator) fail(path, msg string) {
	g.errs = append(g.errs, path+": "+msg)
}

// err returns the problems found so far, sorted.
func (g *generator) err() error {
	if len(g.errs) == 0 {
		return nil
	}
	slices.Sort(g.errs)
	return errors.New(strings.Join(g.errs, "\n"))
}

func ref(name string) schema { return schema{"$ref": "#/$defs/" + name} }

func nullable(s schema) schema { return schema{"anyOf": []any{s, schema{"type": "null"}}} }

// defName names t's definition: plain for agentd types, pkg.Type for the
// others.
func defName(t reflect.Type) string {
	pkg := t.PkgPath()
	switch {
	case pkg == modulePath+"/internal/agentd":
		return t.Name()
	case pkg == modulePath:
		return "agent." + t.Name()
	}
	return pkg[strings.LastIndex(pkg, "/")+1:] + "." + t.Name()
}

// schemaFor returns the schema of t, adding definitions for the named
// types it reaches. path locates t in the protocol for error messages.
func (g *generator) schemaFor(t reflect.Type, path string) schema {
	switch t {
	case reflect.TypeFor[time.Time]():
		return schema{"type": "string", "format": "date-time"}
	case reflect.TypeFor[time.Duration]():
		return schema{"type": "integer", "description": "nanoseconds"}
	case reflect.TypeFor[json.RawMessage]():
		return schema{}
	}
	if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface {
		if _, ok := customMarshalers[t]; !ok && (t.Implements(marshalerType) || t.Implements(textType) ||
			reflect.PointerTo(t).Implements(marshalerType) || reflect.PointerTo(t).Implements(textType)) {
			g.fail(path, t.String()+" has its own JSON encoding the generator does not know; describe it in customMarshalers")
			return schema{}
		}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return nullable(g.schemaFor(t.Elem(), path))
	case reflect.Interface:
		if t.NumMethod() == 0 {
			return schema{}
		}
		if eventUnions[t] {
			return g.eventUnion(t, path)
		}
		g.fail(path, "interface "+t.String()+" has no wire form")
		return schema{}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return nullable(schema{"type": "string", "contentEncoding": "base64"})
		}
		return nullable(schema{"type": "array", "items": g.schemaFor(t.Elem(), path+"[]")})
	case reflect.Array:
		return schema{"type": "array", "items": g.schemaFor(t.Elem(), path+"[]"), "minItems": t.Len(), "maxItems": t.Len()}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			g.fail(path, "map key "+t.Key().String()+" is not a string")
			return schema{}
		}
		if t.Name() != "" {
			return g.named(t, func() schema {
				return schema{"type": "object", "additionalProperties": g.schemaFor(t.Elem(), path+"{}")}
			}, true)
		}
		return nullable(schema{"type": "object", "additionalProperties": g.schemaFor(t.Elem(), path+"{}")})
	case reflect.Struct:
		if t.Name() == "" {
			return g.structSchema(t, path)
		}
		return g.named(t, func() schema { return g.structSchema(t, defName(t)) }, false)
	case reflect.Bool:
		return g.scalar(t, "boolean")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return g.scalar(t, "integer")
	case reflect.Float32, reflect.Float64:
		return g.scalar(t, "number")
	case reflect.String:
		if t.Name() == "" || t.PkgPath() == "" {
			return schema{"type": "string"}
		}
		return g.named(t, func() schema {
			s := schema{"type": "string"}
			// The constants are examples, not an enum: Go sends "" for an
			// unset value, and providers pass native values through for
			// open sets such as Effort and PermissionMode.
			if values := g.stringConstants(t); len(values) > 0 {
				s["examples"] = values
			}
			return s
		}, false)
	}
	g.fail(path, t.Kind().String()+" "+t.String()+" has no wire form")
	return schema{}
}

// named returns a reference to t's definition, building it on first use.
// A nullable definition is referenced through anyOf with null.
func (g *generator) named(t reflect.Type, build func() schema, isNullable bool) schema {
	name := defName(t)
	if _, ok := g.defs[name]; !ok {
		g.defs[name] = schema{} // placeholder for recursive types
		g.defs[name] = build()
	}
	if isNullable {
		return nullable(ref(name))
	}
	return ref(name)
}

func (g *generator) scalar(t reflect.Type, typ string) schema {
	if t.Name() == "" || t.PkgPath() == "" {
		return schema{"type": typ}
	}
	return g.named(t, func() schema { return schema{"type": typ} }, false)
}

// structSchema describes a struct the way encoding/json encodes it.
func (g *generator) structSchema(t reflect.Type, path string) schema {
	props := schema{}
	for _, f := range jsonfields.Of(t) {
		key := defName(f.Owner) + "." + f.StructField.Name
		fieldPath := path + "." + f.Name
		ft := f.StructField.Type
		if ft == errorType {
			g.fail(fieldPath, "error field "+key+" is not shadowed by an agentd wire type")
			continue
		}
		if unrepresentable(ft) {
			if _, ok := skipList[key]; ok {
				g.skipUsed[key] = true
				continue
			}
			g.fail(fieldPath, ft.String()+" field "+key+" has no wire form; add it to skipList with a reason or give it one")
			continue
		}
		props[f.Name] = g.schemaFor(ft, fieldPath)
	}
	return schema{"type": "object", "properties": props}
}

// eventUnion describes interface t as any of the event types that
// implement it, or null.
func (g *generator) eventUnion(t reflect.Type, path string) schema {
	var options []any
	for _, ev := range agentd.EventTypes() {
		if reflect.TypeOf(ev).Implements(t) {
			options = append(options, g.schemaFor(reflect.TypeOf(ev).Elem(), path))
		}
	}
	if len(options) == 0 {
		g.fail(path, "no event type implements "+t.String())
	}
	return schema{"anyOf": append(options, schema{"type": "null"})}
}

// unrepresentable reports whether fields of type t, or of the elements
// it holds, are left to skipList.
func unrepresentable(t reflect.Type) bool {
	if t == loggerType || t == httpType {
		return true
	}
	switch t.Kind() {
	case reflect.Func, reflect.Chan:
		return true
	case reflect.Interface:
		return t.NumMethod() > 0 && !eventUnions[t]
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return unrepresentable(t.Elem())
	}
	return false
}

// stringConstants returns the string constants of named type t declared
// in its package's source, in source order, or nil when t's package is
// outside the module.
func (g *generator) stringConstants(t reflect.Type) []string {
	pkg := t.PkgPath()
	if pkg != modulePath && !strings.HasPrefix(pkg, modulePath+"/") {
		return nil
	}
	dir := filepath.Join(g.root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(pkg, modulePath), "/")))
	byType, ok := g.consts[dir]
	if !ok {
		var err error
		byType, err = parseStringConstants(dir)
		if err != nil {
			g.fail(t.String(), err.Error())
		}
		g.consts[dir] = byType
	}
	return byType[t.Name()]
}

// parseStringConstants maps type names to their string constants in the
// non-test Go files of dir. It recognizes `X T = "v"` and `X = T("v")`.
func parseStringConstants(dir string) (map[string][]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, value := range vs.Values {
					typeName, lit := "", value
					if id, ok := vs.Type.(*ast.Ident); ok {
						typeName = id.Name
					} else if call, ok := value.(*ast.CallExpr); ok && len(call.Args) == 1 {
						if id, ok := call.Fun.(*ast.Ident); ok {
							typeName, lit = id.Name, call.Args[0]
						}
					}
					basic, ok := lit.(*ast.BasicLit)
					if typeName == "" || !ok || basic.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(basic.Value)
					if err != nil {
						return nil, err
					}
					if !slices.Contains(out[typeName], s) {
						out[typeName] = append(out[typeName], s)
					}
				}
			}
		}
	}
	return out, nil
}

// errorKinds reads the ErrorInfo.Kind values from its doc comment in
// internal/agentd/protocol.go, "empty" standing for "".
func (g *generator) errorKinds() ([]string, error) {
	file := filepath.Join(g.root, "internal", "agentd", "protocol.go")
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var doc string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "ErrorInfo" {
			return doc == ""
		}
		for _, field := range ts.Type.(*ast.StructType).Fields.List {
			if len(field.Names) == 1 && field.Names[0].Name == "Kind" {
				doc = field.Doc.Text()
			}
		}
		return false
	})
	_, list, ok := strings.Cut(doc, ":")
	list, _, _ = strings.Cut(list, ".")
	if !ok || list == "" {
		return nil, errors.New(file + ": ErrorInfo.Kind doc comment no longer lists the kinds as \"...: a, b, or empty.\"")
	}
	var kinds []string
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimPrefix(strings.Join(strings.Fields(item), " "), "or ")
		if item == "empty" {
			item = ""
		}
		kinds = append(kinds, item)
	}
	return kinds, nil
}
