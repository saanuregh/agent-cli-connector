package claude

import (
	"slices"
	"testing"
)

func TestTurnLogDropsEverythingAfterTarget(t *testing.T) {
	var l turnLog
	l.add("t1", "t1")
	l.add("t1", "a1")
	l.add("steer", "steer") // a steered message inside t1
	l.add("t1", "a2")       // t1 keeps producing after the steer
	l.add("", "loose")
	l.add("t2", "t2")
	l.add("t2", "b1")
	turns, ids := l.drop("steer")
	if !slices.Equal(turns, []string{"steer", "t2"}) {
		t.Errorf("turns = %v", turns)
	}
	if !slices.Equal(ids, []string{"steer", "a2", "loose", "t2", "b1"}) {
		t.Errorf("ids = %v", ids)
	}
	if l.turnOf("a1") != "t1" || l.turnOf("a2") != "" || l.first() != "t1" {
		t.Errorf("remaining log wrong: turnOf(a1)=%q turnOf(a2)=%q first=%q", l.turnOf("a1"), l.turnOf("a2"), l.first())
	}
}

func TestVersionBefore(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{{"2.1.300", true}, {"2.1.301", false}, {"2.2.0", false}, {"2.1.301-beta", false}, {"1.9.999", true}, {"", false}} {
		if got := versionBefore(c.v, minToolPolicyVersion); got != c.want {
			t.Errorf("versionBefore(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
