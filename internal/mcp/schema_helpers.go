package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaFor infers a JSON Schema from a Go struct type. Every tool's input
// and output schema starts here: struct inference gives us
// additionalProperties:false and a required list derived from `omitempty`
// for free (PLAN §6 rule 1), and the few properties that need an enum,
// default or a limit pulled from internal/domain/limits.go are patched
// afterwards with the prop/setX helpers below.
func schemaFor[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		var zero T
		panic(fmt.Sprintf("mcp: schema for %T: %v", zero, err))
	}
	return s
}

// prop looks up a property schema by name, panicking if it is absent. A
// missing property here is a programming error caught at server
// construction time (every AddTool call runs at startup), not a runtime
// condition to handle gracefully.
func prop(s *jsonschema.Schema, name string) *jsonschema.Schema {
	p, ok := s.Properties[name]
	if !ok {
		panic(fmt.Sprintf("mcp: schema has no property %q", name))
	}
	return p
}

func enumOf(vals []string) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mcp: marshaling schema default: %v", err))
	}
	return b
}

func setDefault(s *jsonschema.Schema, v any)       { s.Default = rawJSON(v) }
func setEnum(s *jsonschema.Schema, vals ...string) { s.Enum = enumOf(vals) }
func setMax(s *jsonschema.Schema, n float64)       { s.Maximum = jsonschema.Ptr(n) }
func setMin(s *jsonschema.Schema, n float64)       { s.Minimum = jsonschema.Ptr(n) }
func setMaxLen(s *jsonschema.Schema, n int)        { s.MaxLength = jsonschema.Ptr(n) }
func setMinLen(s *jsonschema.Schema, n int)        { s.MinLength = jsonschema.Ptr(n) }
func setMaxItems(s *jsonschema.Schema, n int)      { s.MaxItems = jsonschema.Ptr(n) }
func setMinItems(s *jsonschema.Schema, n int)      { s.MinItems = jsonschema.Ptr(n) }
