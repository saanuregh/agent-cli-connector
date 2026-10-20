package jsonfields

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/saanuregh/agent-cli-connector/internal/leakcheck"
)

func TestMain(m *testing.M) { leakcheck.Main(m) }

type inner struct {
	Shared string
	Deep   int
	Tie    int
}

type other struct {
	Tie    int
	Tagged int `json:"Tie2"`
}

type outer struct {
	inner
	*other
	Shared  bool   // shallower, shadows inner.Shared
	Renamed string `json:"renamed,omitempty"`
	Skipped string `json:"-"`
	hidden  string
	Named   inner `json:"named"`
}

// TestMatchesEncodingJSON checks the listed names against the keys
// encoding/json actually writes.
func TestMatchesEncodingJSON(t *testing.T) {
	v := outer{other: &other{}, Renamed: "x", hidden: "h"}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &encoded); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range Of(reflect.TypeFor[outer]()) {
		names = append(names, f.Name)
		got := reflect.ValueOf(v).FieldByIndex(f.StructField.Index)
		if got.Type() != f.StructField.Type {
			t.Errorf("%s: index leads to a %s, want %s", f.Name, got.Type(), f.StructField.Type)
		}
	}
	if want := slices.Sorted(maps.Keys(encoded)); !slices.Equal(names, want) {
		t.Errorf("fields %v, encoding/json wrote %v", names, want)
	}
}
