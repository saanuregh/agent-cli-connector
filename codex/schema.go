package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Keywords whose value maps names to subschemas.
var schemaMapKeywords = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}

// Keywords whose value is a subschema or an array of subschemas.
var subschemaKeywords = []string{
	"items", "prefixItems", "additionalItems", "contains", "additionalProperties", "unevaluatedProperties",
	"unevaluatedItems", "propertyNames", "allOf", "anyOf", "oneOf", "not", "if", "then", "else",
}

// outputSchema returns schema as sent on turn/start: normalized with
// StrictOutputSchema unless DisableStrictOutputSchema is set. An empty
// schema stays empty.
func (o SessionOptions) outputSchema(schema json.RawMessage) (json.RawMessage, error) {
	if len(schema) == 0 || o.DisableStrictOutputSchema {
		return schema, nil
	}
	return StrictOutputSchema(schema)
}

// StrictOutputSchema rewrites a JSON Schema into the strict form OpenAI
// structured outputs require: the root is an object schema, and every
// object schema, at any depth, sets additionalProperties to false and lists
// all its properties as required. It fails when an object schema allows
// additional properties. Property order is kept, because the model writes
// keys in schema order.
func StrictOutputSchema(schema json.RawMessage) (json.RawMessage, error) {
	root, err := decodeOrdered(schema)
	if err != nil {
		return nil, fmt.Errorf("codex: output schema: %w", err)
	}
	obj, ok := root.(*orderedObject)
	if !ok {
		return nil, errors.New("codex: output schema must be a JSON object")
	}
	if err := strictNode(obj, "$"); err != nil {
		return nil, err
	}
	if !isObjectSchema(obj) {
		return nil, errors.New("codex: output schema root must be an object schema")
	}
	return json.Marshal(obj)
}

func strictSubschema(v any, path string) error {
	switch v := v.(type) {
	case *orderedObject:
		return strictNode(v, path)
	case []any:
		for i, entry := range v {
			if obj, ok := entry.(*orderedObject); ok {
				if err := strictNode(obj, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func strictNode(node *orderedObject, path string) error {
	for _, kw := range schemaMapKeywords {
		if m, ok := node.get(kw).(*orderedObject); ok {
			for _, name := range m.keys {
				if err := strictSubschema(m.vals[name], path+"."+kw+"."+name); err != nil {
					return err
				}
			}
		}
	}
	for _, kw := range subschemaKeywords {
		if v, ok := node.vals[kw]; ok {
			if err := strictSubschema(v, path+"."+kw); err != nil {
				return err
			}
		}
	}
	if !isObjectSchema(node) {
		return nil
	}
	if ap, ok := node.vals["additionalProperties"]; !ok {
		node.set("additionalProperties", false)
	} else if ap != false {
		return fmt.Errorf("codex: output schema: %s must set additionalProperties to false; strict structured outputs reject other values (set SessionOptions.DisableStrictOutputSchema to send the schema unchanged)", path)
	}
	props, ok := node.get("properties").(*orderedObject)
	if !ok {
		return nil
	}
	var required []any
	if list, ok := node.get("required").([]any); ok {
		for _, r := range list {
			if name, ok := r.(string); ok && !slices.Contains(required, any(name)) {
				required = append(required, name)
			}
		}
	}
	for _, name := range props.keys {
		if !slices.Contains(required, any(name)) {
			required = append(required, name)
		}
	}
	if required == nil {
		required = []any{}
	}
	node.set("required", required)
	return nil
}

func isObjectSchema(node *orderedObject) bool {
	if _, ok := node.get("properties").(*orderedObject); ok {
		return true
	}
	switch t := node.get("type").(type) {
	case string:
		return t == "object"
	case []any:
		return slices.Contains(t, any("object"))
	}
	return false
}

// orderedObject is a JSON object that keeps its key order.
type orderedObject struct {
	keys []string
	vals map[string]any
}

func (o *orderedObject) get(key string) any { return o.vals[key] }

func (o *orderedObject) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// decodeOrdered decodes JSON, representing objects as *orderedObject and
// numbers as json.Number so they round-trip unchanged.
func decodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("unexpected data after the schema")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		obj := &orderedObject{vals: map[string]any{}}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := keyTok.(string)
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			obj.set(key, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil
	case json.Delim('['):
		list := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return list, nil
	}
	return tok, nil
}
