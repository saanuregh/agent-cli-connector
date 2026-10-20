package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/jsonfields"
)

const wireSchemaPath = "testdata/app-server.schema.json"

// wireCase is one Codex message shape the package decodes. direction is
// "notification", "serverRequest", "response" (the result of the client
// request method), or "definition" (method names a schema definition, for
// shapes nested in other messages). notApplicable lists json paths of a
// shared struct that the message does not carry; knownDrift lists json paths
// we read that the schema no longer has.
type wireCase struct {
	method        string
	direction     string
	v             any
	notApplicable []string
	knownDrift    []string
}

// responseDefinitions names the response definition of client requests
// whose name does not follow <Params name minus Params>Response.
var responseDefinitions = map[string]string{
	"account/rateLimits/read": "GetAccountRateLimitsResponse",
}

var wireCases = []wireCase{
	// Notifications.
	{method: "serverRequest/resolved", direction: "notification", v: serverRequestResolvedParams{}},
	{method: "item/mcpToolCall/progress", direction: "notification", v: mcpToolCallProgressParams{}},
	{method: "item/commandExecution/terminalInteraction", direction: "notification", v: terminalInteractionParams{}},
	{method: "turn/plan/updated", direction: "notification", v: turnPlanUpdatedParams{}},
	{method: "turn/diff/updated", direction: "notification", v: turnDiffUpdatedParams{}},
	{method: "thread/status/changed", direction: "notification", v: threadStatusChangedParams{}},
	{method: "error", direction: "notification", v: errorNotificationParams{}},
	{method: "thread/name/updated", direction: "notification", v: threadNameUpdatedParams{}},
	{method: "thread/settings/updated", direction: "notification", v: threadSettingsUpdatedParams{}},
	{method: "model/rerouted", direction: "notification", v: modelReroutedParams{}},
	{method: "model/rerouted", direction: "notification", v: childModelReroutedParams{}},
	{method: "warning", direction: "notification", v: warningParams{}, notApplicable: []string{"summary", "details"}},
	{method: "configWarning", direction: "notification", v: warningParams{}, notApplicable: []string{"message"}},
	{method: "deprecationNotice", direction: "notification", v: warningParams{}, notApplicable: []string{"message"}},
	{method: "guardianWarning", direction: "notification", v: warningParams{}, notApplicable: []string{"summary", "details"}},
	{method: "mcpServer/startupStatus/updated", direction: "notification", v: mcpServerStartupStatusParams{}},
	{method: "hook/started", direction: "notification", v: hookRunParams{}},
	{method: "hook/completed", direction: "notification", v: hookRunParams{}},
	{method: "thread/goal/updated", direction: "notification", v: threadGoalUpdatedParams{}},
	{method: "account/rateLimits/updated", direction: "notification", v: rateLimitsUpdatedParams{}},
	{method: "thread/tokenUsage/updated", direction: "notification", v: tokenUsageUpdatedParams{}},
	{method: "thread/tokenUsage/updated", direction: "notification", v: childTokenUsageUpdatedParams{}},

	// turnParams is decoded for every notification; each method carries
	// only some of its fields.
	{method: "turn/started", direction: "notification", v: turnParams{}, notApplicable: []string{"turnId", "itemId", "delta", "item"}},
	{method: "turn/completed", direction: "notification", v: turnParams{}, notApplicable: []string{"turnId", "itemId", "delta", "item"}},
	{method: "item/started", direction: "notification", v: turnParams{}, notApplicable: []string{"itemId", "delta", "turn"}},
	{method: "item/completed", direction: "notification", v: turnParams{}, notApplicable: []string{"itemId", "delta", "turn"}},
	{method: "item/agentMessage/delta", direction: "notification", v: turnParams{}, notApplicable: []string{"turn", "item"}},
	{method: "item/plan/delta", direction: "notification", v: turnParams{}, notApplicable: []string{"turn", "item"}},
	{method: "item/reasoning/summaryTextDelta", direction: "notification", v: turnParams{}, notApplicable: []string{"turn", "item"}},
	{method: "item/reasoning/summaryPartAdded", direction: "notification", v: turnParams{}, notApplicable: []string{"delta", "turn", "item"}},
	{method: "item/reasoning/textDelta", direction: "notification", v: turnParams{}, notApplicable: []string{"turn", "item"}},
	{method: "item/commandExecution/outputDelta", direction: "notification", v: turnParams{}, notApplicable: []string{"turn", "item"}},
	{method: "item/fileChange/outputDelta", direction: "notification", v: turnParams{}, notApplicable: []string{"turn", "item"}},
	{method: "item/mcpToolCall/progress", direction: "notification", v: turnParams{}, notApplicable: []string{"delta", "turn", "item"}},
	{method: "item/commandExecution/terminalInteraction", direction: "notification", v: turnParams{}, notApplicable: []string{"delta", "turn", "item"}},
	{method: "thread/tokenUsage/updated", direction: "notification", v: turnParams{}, notApplicable: []string{"itemId", "delta", "turn", "item"}},

	// Server requests.
	{method: "item/tool/call", direction: "serverRequest", v: dynamicToolCallParams{}},
	{method: "item/commandExecution/requestApproval", direction: "serverRequest", v: commandExecutionApprovalParams{}},
	{method: "item/fileChange/requestApproval", direction: "serverRequest", v: fileChangeApprovalParams{}},
	{method: "item/permissions/requestApproval", direction: "serverRequest", v: permissionsApprovalParams{}},
	{method: "item/tool/requestUserInput", direction: "serverRequest", v: toolRequestUserInputParams{}},
	{method: "mcpServer/elicitation/request", direction: "serverRequest", v: mcpElicitationParams{}},
	{method: "applyPatchApproval", direction: "serverRequest", v: legacyApprovalParams{}, notApplicable: []string{"command", "cwd"}},
	{method: "execCommandApproval", direction: "serverRequest", v: legacyApprovalParams{}, notApplicable: []string{"fileChanges", "grantRoot"}},

	// Responses.
	{method: "thread/start", direction: "response", v: threadResponse{}},
	{method: "thread/resume", direction: "response", v: threadResponse{}},
	{method: "thread/fork", direction: "response", v: threadResponse{}},
	{method: "skills/list", direction: "response", v: skillsListResponse{}},
	{method: "config/read", direction: "response", v: configReadResponse{}},
	{method: "turn/steer", direction: "response", v: turnSteerResponse{}},
	{method: "turn/start", direction: "response", v: turnStartResponse{}},
	{method: "review/start", direction: "response", v: reviewStartResponse{}},
	{method: "thread/read", direction: "response", v: threadReadHistoryResponse{}},
	{method: "thread/turns/list", direction: "response", v: threadTurnsListResponse{}},
	{method: "thread/goal/get", direction: "response", v: threadGoalGetResponse{}},
	{method: "account/rateLimits/read", direction: "response", v: rateLimitsReadResponse{}},
	{method: "thread/backgroundTerminals/terminate", direction: "response", v: terminateBackgroundTerminalResponse{}},
	{method: "account/rateLimitResetCredit/consume", direction: "response", v: rateLimitResetCreditResponse{}},
	{method: "account/read", direction: "response", v: accountReadResponse{}},
	{method: "thread/list", direction: "response", v: threadListResponse{}},
	{method: "thread/read", direction: "response", v: threadReadTurnsResponse{}},
	{method: "thread/read", direction: "response", v: threadReadParentResponse{}},
	{method: "thread/turns/list", direction: "response", v: threadTurnsListHistoryResponse{}},

	// Shapes nested in other messages.
	{method: "Turn", direction: "definition", v: turnWire{}},
	{method: "Model", direction: "definition", v: modelWire{}},
	{method: "ThreadItem", direction: "definition", v: threadItem{}},
	{method: "ThreadItem", direction: "definition", v: collabAgentStates{}},
	{method: "McpToolCallResult", direction: "definition", v: mcpToolCallResult{}},
	{method: "SandboxPolicy", direction: "definition", v: sandboxPolicyType{}},
	{method: "RateLimitSnapshot", direction: "definition", v: rateLimitSnapshot{}},
	{method: "ThreadGoal", direction: "definition", v: goalWire{}},
	{method: "TokenUsageBreakdown", direction: "definition", v: tokenUsage{}},
	{method: "Thread", direction: "definition", v: threadSummaryWire{}},
	{method: "Turn", direction: "definition", v: historyTurn{}},
	{method: "UserInput", direction: "definition", v: userInputPart{}},
	{method: "ThreadItem", direction: "definition", v: itemTimestamps{}, knownDrift: []string{"timestamp", "createdAt"}},
}

func TestWireDrift(t *testing.T) {
	data, err := os.ReadFile(wireSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	s := &wireSchema{root: root}
	for _, c := range wireCases {
		name := fmt.Sprintf("%s %s %T", c.direction, c.method, c.v)
		t.Run(name, func(t *testing.T) {
			def, err := s.definition(c.method, c.direction)
			if err != nil {
				t.Fatal(err)
			}
			if def == nil {
				t.Fatalf("%s %s resolves to no schema", c.direction, c.method)
			}
			check := &driftCheck{
				t:             t,
				label:         fmt.Sprintf("%s %s", c.direction, c.method),
				schema:        s,
				notApplicable: c.notApplicable,
				knownDrift:    c.knownDrift,
			}
			check.value(reflect.TypeOf(c.v), []any{def}, fmt.Sprintf("%T", c.v), "")
		})
	}
	if len(s.unresolved) > 0 {
		t.Errorf("schema references that point nowhere: %v", s.unresolved)
	}
}

type wireSchema struct {
	root map[string]any
	// unresolved collects $refs that point nowhere. Such a node would
	// accept anything, so the test fails instead of passing vacuously.
	unresolved []string
}

// definition returns the params, response, or named definition of method.
func (s *wireSchema) definition(method, direction string) (any, error) {
	defs, _ := s.root["definitions"].(map[string]any)
	if direction == "definition" {
		if d, ok := defs[method]; ok {
			return d, nil
		}
		if v2, ok := defs["v2"].(map[string]any); ok && v2[method] != nil {
			return v2[method], nil
		}
		return nil, fmt.Errorf("schema has no definition %s", method)
	}
	union := map[string]string{"notification": "ServerNotification", "serverRequest": "ServerRequest", "response": "ClientRequest"}[direction]
	if union == "" {
		return nil, fmt.Errorf("unknown direction %q", direction)
	}
	u, ok := defs[union].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema has no %s definition", union)
	}
	for _, entry := range asList(u["oneOf"]) {
		props, _ := asMap(entry)["properties"].(map[string]any)
		m := asMap(props["method"])
		if !slices.Contains(append(asList(m["enum"]), m["const"]), any(method)) {
			continue
		}
		if direction != "response" {
			return s.resolve(props["params"]), nil
		}
		name := responseDefinitions[method]
		if name == "" {
			ref, _ := asMap(props["params"])["$ref"].(string)
			base, ok := strings.CutSuffix(ref[strings.LastIndex(ref, "/")+1:], "Params")
			if !ok {
				return nil, fmt.Errorf("client request %s has params %q; add its response to responseDefinitions", method, ref)
			}
			name = base + "Response"
		}
		return s.definition(name, "definition")
	}
	return nil, fmt.Errorf("schema %s has no method %s", union, method)
}

// resolve follows a $ref.
func (s *wireSchema) resolve(node any) any {
	for range 32 {
		ref, ok := asMap(node)["$ref"].(string)
		if !ok {
			return node
		}
		var cur any = s.root
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			cur = asMap(cur)[part]
		}
		if cur == nil {
			s.unresolved = append(s.unresolved, ref)
			return nil
		}
		node = cur
	}
	return node
}

// leaves expands $ref, allOf, oneOf, and anyOf into the schemas a value may
// match.
func (s *wireSchema) leaves(nodes []any) []map[string]any {
	var out []map[string]any
	var walk func(node any, depth int)
	walk = func(node any, depth int) {
		if depth > 32 {
			return
		}
		if b, ok := node.(bool); ok {
			if b {
				out = append(out, map[string]any{})
			}
			return
		}
		m := asMap(s.resolve(node))
		combined := false
		for _, key := range []string{"allOf", "oneOf", "anyOf"} {
			for _, sub := range asList(m[key]) {
				combined = true
				walk(sub, depth+1)
			}
		}
		if !combined || m["properties"] != nil || m["type"] != nil {
			out = append(out, m)
		}
	}
	for _, n := range nodes {
		walk(n, 0)
	}
	return out
}

// jsonTypes returns the JSON types a leaf schema allows, or nil when it
// allows anything.
func jsonTypes(leaf map[string]any) []string {
	switch t := leaf["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, v := range t {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if leaf["properties"] != nil || leaf["additionalProperties"] != nil {
		return []string{"object"}
	}
	values := asList(leaf["enum"])
	if c, ok := leaf["const"]; ok {
		values = append(values, c)
	}
	var out []string
	for _, v := range values {
		switch v.(type) {
		case string:
			out = append(out, "string")
		case float64:
			out = append(out, "number")
		case bool:
			out = append(out, "boolean")
		case nil:
			out = append(out, "null")
		}
	}
	return out
}

type driftCheck struct {
	t             *testing.T
	label         string
	schema        *wireSchema
	notApplicable []string
	knownDrift    []string
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// value checks Go type typ, reached by goPath and jsonPath, against the
// schemas nodes.
func (c *driftCheck) value(typ reflect.Type, nodes []any, goPath, jsonPath string) {
	if typ == rawMessageType || typ.Kind() == reflect.Interface {
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	leaves := c.schema.leaves(nodes)
	var allowed []string
	for _, l := range leaves {
		types := jsonTypes(l)
		if types == nil {
			return
		}
		allowed = append(allowed, types...)
	}
	want := map[reflect.Kind][]string{
		reflect.String: {"string"}, reflect.Bool: {"boolean"},
		reflect.Struct: {"object"}, reflect.Map: {"object"}, reflect.Slice: {"array"},
	}[typ.Kind()]
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		want = []string{"integer", "number"}
	}
	if !slices.ContainsFunc(want, func(w string) bool { return slices.Contains(allowed, w) }) {
		slices.Sort(allowed)
		c.t.Errorf("%s: %s (json %q) is Go %s, schema allows %v", c.label, goPath, jsonPath, typ, slices.Compact(allowed))
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		var objects []map[string]any
		for _, l := range leaves {
			if slices.Contains(jsonTypes(l), "object") {
				objects = append(objects, l)
			}
		}
		c.fields(typ, objects, goPath, jsonPath)
	case reflect.Slice:
		var items []any
		for _, l := range leaves {
			if l["items"] != nil {
				items = append(items, l["items"])
			}
		}
		if len(items) > 0 {
			c.value(typ.Elem(), items, goPath+"[]", jsonPath+"[]")
		}
	case reflect.Map:
		var values []any
		for _, l := range leaves {
			if ap, ok := l["additionalProperties"].(map[string]any); ok {
				values = append(values, ap)
			}
		}
		if len(values) > 0 {
			c.value(typ.Elem(), values, goPath+"[key]", jsonPath+"[key]")
		}
	}
}

func (c *driftCheck) fields(typ reflect.Type, objects []map[string]any, goPath, jsonPath string) {
	for _, jf := range jsonfields.Of(typ) {
		f, name := jf.StructField, jf.Name
		path := name
		if jsonPath != "" {
			path = jsonPath + "." + name
		}
		var props []any
		known := map[string]bool{}
		for _, o := range objects {
			p, _ := o["properties"].(map[string]any)
			for k := range p {
				known[k] = true
			}
			if v, ok := p[name]; ok {
				props = append(props, v)
			}
		}
		listed := slices.Contains(c.notApplicable, path) || slices.Contains(c.knownDrift, path)
		if listed {
			if len(props) > 0 {
				c.t.Errorf("%s: %s.%s (json %q) is listed as absent but the schema has it; remove it from the list", c.label, goPath, f.Name, path)
			}
			continue
		}
		if len(props) == 0 {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			c.t.Errorf("%s: %s.%s (json %q) is missing from the schema, which has %v", c.label, goPath, f.Name, path, names)
			continue
		}
		c.value(f.Type, props, goPath+"."+f.Name, path)
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
