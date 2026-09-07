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

// dropRequired removes names from the schema's required list. We need it
// when a struct field has no `omitempty` (so the inferrer marks it required)
// but the handler wants to issue a domain-shaped validation error instead
// of letting the SDK's terse "missing required property" escape to the wire.
func dropRequired(s *jsonschema.Schema, names ...string) {
	if len(s.Required) == 0 {
		return
	}
	drop := make(map[string]struct{}, len(names))
	for _, n := range names {
		drop[n] = struct{}{}
	}
	out := s.Required[:0]
	for _, r := range s.Required {
		if _, ok := drop[r]; !ok {
			out = append(out, r)
		}
	}
	s.Required = out
}

// setAcceptanceItemShape replaces the inferred items schema with a "string
// OR {text, done} object" union. The handler accepts both shapes (see
// acceptanceIn.UnmarshalJSON); the schema is the place to make the union
// visible to agents and to the JSON-Schema validator.
//
// We use the type-list form (Types: [...]) rather than anyOf so the schema
// stays a single object that the validator walks once — anyOf would force
// the validator to descend each branch and emit two unrelated errors on
// mixed input. Properties apply only when the value is an object, which is
// what JSON Schema's type-keyword contract guarantees.
func setAcceptanceItemShape(s *jsonschema.Schema, textMax *int) {
	// The inferrer set s.Type to "object"; we are widening the union and so
	// must clear Type (the library forbids both being set, see Schema.Type
	// godoc: "Use Type for a single type, or Types for multiple types").
	s.Type = ""
	s.Types = []string{"string", "object"}
	s.Properties = map[string]*jsonschema.Schema{
		"text": {Type: "string", MaxLength: textMax},
		"done": {Type: "boolean"},
	}
	s.Required = []string{"text"}
	s.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
}
