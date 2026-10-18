package providerutil

import (
	"slices"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	if v := ParseVersion("codex-cli 0.163.0"); v != "0.163.0" {
		t.Errorf("ParseVersion = %q", v)
	}
	if v := ParseVersion("2.1.301 (Claude Code)"); v != "2.1.301" {
		t.Errorf("ParseVersion = %q", v)
	}
}

func TestLastPage(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	for _, c := range []struct {
		limit, offset int
		want          []int
	}{
		{0, 0, items},
		{2, 0, []int{4, 5}},
		{2, 2, []int{2, 3}},
		{2, 4, []int{1}},
		{2, 9, []int{}},
		{0, 3, []int{1, 2}},
		{9, -1, items},
	} {
		if got := LastPage(items, c.limit, c.offset); !slices.Equal(got, c.want) {
			t.Errorf("LastPage(%d, %d) = %v, want %v", c.limit, c.offset, got, c.want)
		}
	}
}

func TestParseTime(t *testing.T) {
	if got := ParseTime("2026-01-02T03:04:05.5Z"); !got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 5e8, time.UTC)) {
		t.Errorf("ParseTime = %v", got)
	}
	if !ParseTime("bad").IsZero() || !FromUnixMilli(0).IsZero() {
		t.Error("want zero time for unknown values")
	}
	if got := FromUnixMilli(1500); !got.Equal(time.UnixMilli(1500)) {
		t.Errorf("FromUnixMilli = %v", got)
	}
}
