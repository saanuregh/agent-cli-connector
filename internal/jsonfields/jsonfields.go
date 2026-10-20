// Package jsonfields lists a struct type's fields the way encoding/json
// encodes them.
package jsonfields

import (
	"cmp"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// Field is a struct field as encoding/json encodes it.
type Field struct {
	// Name is the JSON name.
	Name string
	// StructField is the field. Its Index is the path from the listed
	// type, for reflect.Value.FieldByIndex.
	StructField reflect.StructField
	// Owner is the struct type that declares the field.
	Owner reflect.Type

	depth  int
	tagged bool
}

var cache sync.Map // reflect.Type → []Field

// Of lists struct type t's encoded fields, sorted by name: embedded
// structs without a name tag are flattened, and for each name the
// shallowest field wins, a tagged one breaking ties; an unbroken tie
// drops the name, as encoding/json does.
func Of(t reflect.Type) []Field {
	if fields, ok := cache.Load(t); ok {
		return fields.([]Field)
	}
	fields := list(t)
	cache.Store(t, fields)
	return fields
}

func list(t reflect.Type) []Field {
	type level struct {
		t     reflect.Type
		index []int
	}
	var all []Field
	visited := map[reflect.Type]bool{}
	next := []level{{t, nil}}
	for depth := 0; len(next) > 0; depth++ {
		current := next
		next = nil
		for _, l := range current {
			if visited[l.t] {
				continue
			}
			visited[l.t] = true
			for i := range l.t.NumField() {
				sf := l.t.Field(i)
				ft := sf.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if sf.Anonymous {
					if !sf.IsExported() && ft.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				sf.Index = append(slices.Clone(l.index), i)
				name, _, _ := strings.Cut(tag, ",")
				if name == "" && sf.Anonymous && ft.Kind() == reflect.Struct {
					next = append(next, level{ft, sf.Index})
					continue
				}
				tagged := name != ""
				if !tagged {
					name = sf.Name
				}
				all = append(all, Field{Name: name, StructField: sf, Owner: l.t, depth: depth, tagged: tagged})
			}
		}
	}
	byName := map[string][]Field{}
	for _, f := range all {
		byName[f.Name] = append(byName[f.Name], f)
	}
	var out []Field
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		fields := byName[name]
		minDepth := slices.MinFunc(fields, func(a, b Field) int { return cmp.Compare(a.depth, b.depth) }).depth
		var shallow, tagged []Field
		for _, f := range fields {
			if f.depth == minDepth {
				shallow = append(shallow, f)
				if f.tagged {
					tagged = append(tagged, f)
				}
			}
		}
		switch {
		case len(shallow) == 1:
			out = append(out, shallow[0])
		case len(tagged) == 1:
			out = append(out, tagged[0])
		}
	}
	return out
}
