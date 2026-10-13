package mock

import (
	"encoding/json"
	"slices"
	"strings"
)

// tool is a member of the request's tools array.
type tool struct {
	Name        string `json:"name"`
	InputSchema schema `json:"input_schema"`
}

// schema is the part of a JSON Schema that the mock reads to build a valid input.
type schema struct {
	Type       json.RawMessage   `json:"type"` // a type name or an array of them
	Properties map[string]schema `json:"properties"`
	Required   []string          `json:"required"`
	Enum       []json.RawMessage `json:"enum"`
	Const      json.RawMessage   `json:"const"`
}

// UnmarshalJSON reads a schema object. Any other JSON, such as the boolean schema
// true, reads as a schema without constraints, so an odd tool never fails a request.
func (s *schema) UnmarshalJSON(data []byte) error {
	type members schema
	var read members
	if json.Unmarshal(data, &read) != nil {
		read = members{}
	}
	*s = schema(read)
	return nil
}

// openCodeInputs holds the input of each step for OpenCode's own tools, keyed by
// tool name.
var openCodeInputs = map[string]map[string]string{
	"read": {"filePath": "calc.py"},
	"edit": {"filePath": "calc.py", "oldString": "return a - b", "newString": "return a + b"},
	"bash": {"command": "python -m unittest", "description": "Run the tests"},
}

// stringHints picks the value of a generated string member by the first key that
// its lowercased name contains; other strings get defaultString.
var stringHints = []struct{ key, value string }{
	{"old", "return a - b"},
	{"new", "return a + b"},
	{"command", "python -m unittest"},
	{"cmd", "python -m unittest"},
	{"path", "calc.py"},
	{"file", "calc.py"},
}

const defaultString = "mock"

// pickTool returns the tool named kind, else the first whose name contains kind,
// else the first tool. tools must not be empty.
func pickTool(tools []tool, kind string) tool {
	for _, t := range tools {
		if t.Name == kind {
			return t
		}
	}
	for _, t := range tools {
		if strings.Contains(strings.ToLower(t.Name), kind) {
			return t
		}
	}
	return tools[0]
}

// toolInput returns the JSON input of a call to t. An OpenCode tool gets its fixed
// members where its schema declares them as strings; every required member still
// missing gets a value of its schema's type. Keys are sorted, so the bytes are fixed.
func toolInput(t tool) []byte {
	input := map[string]any{}
	props := t.InputSchema.Properties
	for name, value := range openCodeInputs[t.Name] {
		prop, declared := props[name]
		if len(props) == 0 || declared && prop.allows("string") {
			input[name] = value
		}
	}
	for _, name := range t.InputSchema.Required {
		if _, ok := input[name]; !ok {
			input[name] = props[name].value(name)
		}
	}
	return mustMarshal(input)
}

// value returns a value valid for s: its const, its first enum value, or a fixed
// value of its first type. A string's value depends on the member's name.
func (s schema) value(name string) any {
	if len(s.Const) > 0 {
		return s.Const
	}
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	switch s.firstType() {
	case "number", "integer":
		return 1
	case "boolean":
		return false
	case "array":
		return []any{}
	case "null":
		return nil
	case "object":
		object := map[string]any{}
		for _, member := range s.Required {
			object[member] = s.Properties[member].value(member)
		}
		return object
	}
	return stringValue(name)
}

func stringValue(name string) string {
	lower := strings.ToLower(name)
	for _, hint := range stringHints {
		if strings.Contains(lower, hint.key) {
			return hint.value
		}
	}
	return defaultString
}

// types returns the type names of s, or nil when it names none.
func (s schema) types() []string {
	var one string
	if json.Unmarshal(s.Type, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(s.Type, &many) == nil {
		return many
	}
	return nil
}

// firstType returns the first type of s other than null, or null when that is its
// only type, or "" when it names none.
func (s schema) firstType() string {
	types := s.types()
	for _, name := range types {
		if name != "null" {
			return name
		}
	}
	if len(types) > 0 {
		return "null"
	}
	return ""
}

// allows reports whether s accepts values of the named type; a schema without a
// type accepts any.
func (s schema) allows(name string) bool {
	types := s.types()
	return len(types) == 0 || slices.Contains(types, name)
}
