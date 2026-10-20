// Package envutil edits "KEY=value" environment lists.
package envutil

import (
	"maps"
	"slices"
	"strings"
)

// Unset returns base without the variables named in names.
func Unset(base, names []string) []string {
	if len(names) == 0 {
		return base
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, k) {
			out = append(out, kv)
		}
	}
	return out
}

// Merge returns base with the "KEY=value" entries of overrides applied.
// Later entries win.
func Merge(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	index := make(map[string]int, len(base)+len(overrides))
	out := make([]string, 0, len(base)+len(overrides))
	add := func(kv string) {
		key, _, _ := strings.Cut(kv, "=")
		if i, ok := index[key]; ok {
			out[i] = kv
			return
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		add(kv)
	}
	for _, kv := range overrides {
		add(kv)
	}
	return out
}

// List converts a map to "KEY=value" entries in a stable order.
func List(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Map converts "KEY=value" entries to a map. Later entries win.
func Map(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
