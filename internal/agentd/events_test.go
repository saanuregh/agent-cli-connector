package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

// eventPackageDirs are the packages whose event types agentd forwards,
// relative to this package's directory, with the package name they use.
var eventPackageDirs = map[string]string{
	"../..":          "agent",
	"../../claude":   "claude",
	"../../codex":    "codex",
	"../../opencode": "opencode",
	"../../pi":       "pi",
}

// TestEventTypesComplete fails when a package defines an exported struct
// embedding agent.EventMeta that the event table lacks.
func TestEventTypesComplete(t *testing.T) {
	inTable := map[string]bool{}
	for _, ev := range EventTypes() {
		inTable[qualifiedEventName(ev)] = true
	}
	var missing []string
	for dir, pkg := range eventPackageDirs {
		for _, name := range embeddersOfEventMeta(t, dir, pkg == "agent") {
			if !inTable[pkg+"."+name] {
				missing = append(missing, pkg+"."+name)
			}
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("event types missing from eventTypes in events.go: %s", strings.Join(missing, ", "))
	}
}

// TestEventTypeNamesUnique fails when two event types share an
// agent.EventType name, since clients tell events apart by that name.
func TestEventTypeNamesUnique(t *testing.T) {
	seen := map[string]string{}
	for _, ev := range EventTypes() {
		name, qualified := agent.EventType(ev), qualifiedEventName(ev)
		if other, ok := seen[name]; ok {
			t.Errorf("event type name %q is shared by %s and %s", name, other, qualified)
		}
		seen[name] = qualified
	}
}

func qualifiedEventName(ev agent.Event) string {
	path := strings.TrimPrefix(typeOfEvent(ev).PkgPath(), "github.com/saanuregh/agent-cli-connector")
	pkg := strings.TrimPrefix(path, "/")
	if pkg == "" {
		pkg = "agent"
	}
	return pkg + "." + agent.EventType(ev)
}

// embeddersOfEventMeta parses the non-test Go files in dir and returns the
// exported struct types that embed EventMeta (root package) or
// agent.EventMeta (provider packages).
func embeddersOfEventMeta(t *testing.T, dir string, root bool) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	var names []string
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 && embedsEventMeta(field.Type, root) {
						names = append(names, ts.Name.Name)
					}
				}
			}
		}
	}
	return names
}

func embedsEventMeta(expr ast.Expr, root bool) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if root {
		id, ok := expr.(*ast.Ident)
		return ok && id.Name == "EventMeta"
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "EventMeta" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "agent"
}

func typeOfEvent(ev agent.Event) reflect.Type {
	t := reflect.TypeOf(ev)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}
