package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/agentd"
)

func generateForTest(t *testing.T) (raw []byte, doc map[string]any) {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return raw, doc
}

func TestSchemaCoversProtocol(t *testing.T) {
	_, doc := generateForTest(t)
	proto := agentd.Protocol()
	sections := map[string][]string{}
	for name := range proto.Methods {
		sections["methods"] = append(sections["methods"], name)
	}
	for name := range proto.Notifications {
		sections["notifications"] = append(sections["notifications"], name)
	}
	for name := range proto.ReverseRequests {
		sections["reverseRequests"] = append(sections["reverseRequests"], name)
	}
	for name := range proto.ClientNotifications {
		sections["clientNotifications"] = append(sections["clientNotifications"], name)
	}
	for _, ev := range agentd.EventTypes() {
		sections["events"] = append(sections["events"], agent.EventType(ev))
	}
	for _, p := range agentd.Defaults() {
		sections["providerOptions"] = append(sections["providerOptions"], p.Name)
	}
	for section, names := range sections {
		entries, _ := doc[section].(map[string]any)
		for _, name := range names {
			if _, ok := entries[name]; !ok {
				t.Errorf("%s has no entry for %q", section, name)
			}
		}
		if len(entries) != len(names) {
			t.Errorf("%s has %d entries, want %d", section, len(entries), len(names))
		}
	}
	defs := doc["$defs"].(map[string]any)
	var refs int
	walkJSON(doc, func(v map[string]any) {
		if r, ok := v["$ref"].(string); ok {
			refs++
			if _, ok := defs[strings.TrimPrefix(r, "#/$defs/")]; !ok || !strings.HasPrefix(r, "#/$defs/") {
				t.Errorf("$ref %q does not resolve", r)
			}
		}
	})
	if refs == 0 {
		t.Error("schema has no $ref")
	}
}

func walkJSON(v any, visit func(map[string]any)) {
	switch v := v.(type) {
	case map[string]any:
		visit(v)
		for _, child := range v {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range v {
			walkJSON(child, visit)
		}
	}
}

type withErrorField struct {
	Name string
	Err  error
}

type withFuncField struct {
	Name     string
	Callback func()
}

type withIntKeyMap struct {
	Counts map[int]string
}

type shadowsError struct {
	*withErrorField
	Err string
}

func TestGeneratorRejectsErrorField(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{withErrorField{}, "error field protocolgen.withErrorField.Err"},
		{withFuncField{}, "protocolgen.withFuncField.Callback has no wire form"},
		{withIntKeyMap{}, "map key int is not a string"},
	} {
		g := newGenerator(t.TempDir())
		g.schemaFor(reflect.TypeOf(tc.value), "test")
		if err := g.err(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%T: error = %v, want it to contain %q", tc.value, err, tc.want)
		}
	}
	g := newGenerator(t.TempDir())
	g.schemaFor(reflect.TypeFor[shadowsError](), "test")
	if err := g.err(); err != nil {
		t.Errorf("shadowed error field: %v", err)
	}
}

func TestSchemaUpToDate(t *testing.T) {
	raw, _ := generateForTest(t)
	root, _ := moduleRoot()
	checkedIn, err := os.ReadFile(filepath.Join(root, "cmd", "agentd", "protocol.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may turn the file's line endings into CRLF.
	if !bytes.Equal(raw, bytes.ReplaceAll(checkedIn, []byte("\r\n"), []byte("\n"))) {
		t.Error("cmd/agentd/protocol.schema.json is stale; run go generate ./cmd/agentd")
	}
}

func TestSamplesMatchSchema(t *testing.T) {
	_, doc := generateForTest(t)
	defs := doc["$defs"].(map[string]any)
	event := func(name string) any { return doc["events"].(map[string]any)[name] }

	var config agent.Config
	configJSON := `{"Cwd":"/tmp","Model":"m","PermissionMode":"plan","Tools":[{"Name":"t","Description":"d","InputSchema":{"type":"object"}}],"Env":{"A":"b"}}`
	dec := json.NewDecoder(strings.NewReader(configJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil {
		t.Fatalf("sample config does not decode: %v", err)
	}
	now := time.Unix(0, 0).UTC()
	meta := agent.EventMeta{Provider: "claude", SessionID: "s", TurnID: "t", Time: now}
	result := &agent.TurnResult{TurnID: "t", Status: agent.TurnFailed, Err: errors.New("boom")}
	samples := []struct {
		name   string
		value  any
		schema any
	}{
		{"StartParams", map[string]any{"Provider": "claude", "Config": json.RawMessage(configJSON), "Handlers": agentd.HandlerFlags{Permission: true}}, ref("StartParams")},
		{"TurnResultWire", agentd.TurnResultWire{TurnResult: result, Err: &agentd.ErrorInfo{Message: "boom"}}, ref("TurnResultWire")},
		{"EventNotification", agentd.EventNotification{Session: "s1", Seq: 1, Type: "TextDelta", Event: json.RawMessage(`{}`)}, ref("EventNotification")},
		{"TextDelta", &agent.TextDelta{EventMeta: meta, Delta: "hi"}, event("TextDelta")},
		{"TurnCompleted", agentd.TurnCompletedWire{TurnCompleted: &agent.TurnCompleted{EventMeta: meta, Result: result}, Result: &agentd.TurnResultWire{TurnResult: result}}, event("TurnCompleted")},
		{"ProviderStatusWire", agentd.ProviderStatusWire{ProviderStatus: &agent.ProviderStatus{}, Err: &agentd.ErrorInfo{Message: "x", Kind: "exit"}}, ref("ProviderStatusWire")},
	}
	if problems := unknownProperties(defs, toAny(ref("SendResult")), map[string]any{"TurnID": "t", "Bogus": 1}, "SendResult"); len(problems) != 1 {
		t.Errorf("checker found %v for an unknown property, want one problem", problems)
	}
	for _, s := range samples {
		data, err := json.Marshal(s.value)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		var tree any
		if err := json.Unmarshal(data, &tree); err != nil {
			t.Fatal(err)
		}
		if s.schema == nil {
			t.Fatalf("%s: no schema", s.name)
		}
		for _, problem := range unknownProperties(defs, toAny(s.schema), tree, s.name) {
			t.Errorf("%s", problem)
		}
	}
}

func toAny(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

// unknownProperties lists the object keys in value that sch does not
// declare, following $ref, anyOf, items, and additionalProperties.
func unknownProperties(defs map[string]any, sch, value any, path string) []string {
	s, _ := sch.(map[string]any)
	if r, ok := s["$ref"].(string); ok {
		return unknownProperties(defs, defs[strings.TrimPrefix(r, "#/$defs/")], value, path)
	}
	if options, ok := s["anyOf"].([]any); ok {
		if value == nil {
			return nil
		}
		var best []string
		for i, option := range options {
			problems := unknownProperties(defs, option, value, path)
			if len(problems) == 0 {
				return nil
			}
			if i == 0 || len(problems) < len(best) {
				best = problems
			}
		}
		return best
	}
	var problems []string
	switch v := value.(type) {
	case map[string]any:
		props, hasProps := s["properties"].(map[string]any)
		additional, hasAdditional := s["additionalProperties"]
		if !hasProps && !hasAdditional {
			if s["type"] != nil && s["type"] != "object" {
				return []string{path + ": object where schema has type " + s["type"].(string)}
			}
			return nil
		}
		for key, child := range v {
			if prop, ok := props[key]; ok {
				problems = append(problems, unknownProperties(defs, prop, child, path+"."+key)...)
			} else if hasAdditional {
				problems = append(problems, unknownProperties(defs, additional, child, path+"."+key)...)
			} else {
				problems = append(problems, path+"."+key+": not in the schema")
			}
		}
	case []any:
		for _, child := range v {
			problems = append(problems, unknownProperties(defs, s["items"], child, path+"[]")...)
		}
	}
	return problems
}

func TestClientTypesUpToDate(t *testing.T) {
	raw, _ := generateForTest(t)
	root, _ := moduleRoot()
	for _, c := range []struct {
		path   string
		render func([]byte) ([]byte, error)
	}{
		{filepath.Join("clients", "typescript", "src", "protocol.ts"), typescriptTypes},
		{filepath.Join("clients", "python", "agentd", "protocol.py"), pythonTypes},
	} {
		want, err := c.render(raw)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		checkedIn, err := os.ReadFile(filepath.Join(root, c.path))
		if err != nil {
			t.Fatal(err)
		}
		// A Windows checkout may turn the file's line endings into CRLF.
		if !bytes.Equal(want, bytes.ReplaceAll(checkedIn, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("%s is stale; run go generate ./cmd/agentd", filepath.ToSlash(c.path))
		}
	}
}

func TestClientTypesCoverProtocol(t *testing.T) {
	raw, doc := generateForTest(t)
	ts, err := typescriptTypes(raw)
	if err != nil {
		t.Fatal(err)
	}
	py, err := pythonTypes(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name := range doc["$defs"].(map[string]any) {
		id := typeIdentifier(name)
		if !bytes.Contains(ts, []byte("export interface "+id+" ")) && !bytes.Contains(ts, []byte("export type "+id+" ")) {
			t.Errorf("protocol.ts has no type %s for $defs %q", id, name)
		}
		if !bytes.Contains(py, []byte("class "+id+"(")) && !bytes.Contains(py, []byte("\n"+id+" = ")) {
			t.Errorf("protocol.py has no type %s for $defs %q", id, name)
		}
	}
	for _, section := range []string{"methods", "notifications", "clientNotifications", "reverseRequests", "events"} {
		for name := range doc[section].(map[string]any) {
			quoted := []byte(`"` + name + `"`)
			if section == "events" {
				quoted = []byte(`Type: "` + name + `"`)
			}
			if !bytes.Contains(ts, quoted) {
				t.Errorf("protocol.ts has no %s entry %q", section, name)
			}
			if !bytes.Contains(py, []byte(`"`+name+`"`)) {
				t.Errorf("protocol.py has no %s entry %q", section, name)
			}
		}
	}
}

// TestClientTypesParse checks the generated files' syntax with the
// interpreters on PATH. Node runs the TypeScript, which has no runtime
// code, with type stripping, which rejects syntax that cannot be erased.
func TestClientTypesParse(t *testing.T) {
	raw, _ := generateForTest(t)
	dir := t.TempDir()
	for _, c := range []struct {
		tool   string
		file   string
		render func([]byte) ([]byte, error)
		args   func(path string) []string
	}{
		{"node", "protocol.mts", typescriptTypes, func(path string) []string { return []string{path} }},
		{"python3", "protocol.py", pythonTypes, func(path string) []string {
			return []string{"-I", "-c", "import importlib.util,sys,typing\n" +
				"spec = importlib.util.spec_from_file_location('protocol', sys.argv[1])\n" +
				"m = importlib.util.module_from_spec(spec); sys.modules['protocol'] = m; spec.loader.exec_module(m)\n" +
				"for name, v in vars(m).items():\n" +
				"    if isinstance(v, type) and v.__module__ == 'protocol': typing.get_type_hints(v)\n", path}
		}},
	} {
		t.Run(c.tool, func(t *testing.T) {
			bin, err := exec.LookPath(c.tool)
			if err != nil {
				t.Skip(c.tool + " is not on PATH")
			}
			data, err := c.render(raw)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, c.file)
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin, c.args(path)...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s %s: %v\n%s", c.tool, c.file, err, out)
			}
		})
	}
}
