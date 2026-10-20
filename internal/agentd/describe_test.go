package agentd

import (
	"maps"
	"slices"
	"testing"
)

func TestProtocolMethodsMatchDispatch(t *testing.T) {
	s := &server{}
	dispatch := s.methodTable()
	want := slices.Sorted(maps.Keys(dispatch))
	got := slices.Sorted(maps.Keys(Protocol().Methods))
	if !slices.Equal(got, want) {
		t.Errorf("Protocol().Methods = %v\ndispatch table = %v", got, want)
	}
}
