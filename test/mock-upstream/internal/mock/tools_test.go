package mock

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"testing"
)

func object(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "required": required, "properties": properties}
}

func toolDef(name string, schema map[string]any) map[string]any {
	return map[string]any{"name": name, "description": "Test tool " + name, "input_schema": schema}
}

var (
	str     = map[string]any{"type": "string"}
	num     = map[string]any{"type": "number"}
	integer = map[string]any{"type": "integer"}
	boolean = map[string]any{"type": "boolean"}
)

// openCodeTools are shaped like OpenCode's tools. todoread comes before read and
// contains its name, so the exact name must win.
var openCodeTools = []any{
	toolDef("glob", object([]string{"pattern"}, map[string]any{"pattern": str, "path": str})),
	toolDef("todoread", object(nil, map[string]any{})),
	toolDef("bash", object([]string{"command", "description"},
		map[string]any{"command": str, "timeout": num, "description": str})),
	toolDef("edit", object([]string{"filePath", "oldString", "newString"},
		map[string]any{"filePath": str, "oldString": str, "newString": str, "replaceAll": boolean})),
	toolDef("read", object([]string{"filePath"},
		map[string]any{"filePath": str, "offset": num, "limit": num})),
}

// otherTools have other names and schemas with every kind of member.
var otherTools = []any{
	toolDef("list_dir", object([]string{"path"}, map[string]any{"path": str})),
	toolDef("read_file", object([]string{"path", "encoding"}, map[string]any{
		"path": str, "encoding": map[string]any{"type": "string", "enum": []any{"utf-8", "latin-1"}},
	})),
	toolDef("file_editor", object([]string{"file_path", "old_text", "new_text", "options"},
		map[string]any{"file_path": str, "old_text": str, "new_text": str,
			"options": object([]string{"dry_run"}, map[string]any{"dry_run": boolean})})),
	toolDef("run_bash", object([]string{"cmd", "timeout_ms", "background", "args", "mode"},
		map[string]any{"cmd": str, "timeout_ms": integer, "background": boolean,
			"args": map[string]any{"type": "array", "items": str},
			"mode": map[string]any{"type": []any{"null", "string"}}})),
}

// toolNamed returns the tool called name in tools.
func toolNamed(t *testing.T, tools []any, name string) map[string]any {
	t.Helper()
	for _, item := range tools {
		if def := item.(map[string]any); def["name"] == name {
			return def
		}
	}
	t.Fatalf("tool_use names %q, which is not in the request's tools", name)
	return nil
}

// checkToolUse checks that a tool_use block calls a tool of the request with input
// that parses and fits the tool's input_schema, and returns the input.
func checkToolUse(t *testing.T, block gotBlock, tools []any) map[string]any {
	t.Helper()
	if block.Type != "tool_use" || block.ID == "" {
		t.Fatalf("block %+v is not a tool_use", block)
	}
	def := toolNamed(t, tools, block.Name)
	var input any
	if err := json.Unmarshal(block.Input, &input); err != nil {
		t.Fatalf("input %s of %s does not parse: %v", block.Input, block.Name, err)
	}
	for _, problem := range validate(def["input_schema"].(map[string]any), input, "input") {
		t.Errorf("%s: %s", block.Name, problem)
	}
	return input.(map[string]any)
}

// validate checks value against the type, enum, required and properties keywords
// of schema.
func validate(schema map[string]any, value any, path string) []string {
	if !typeMatches(schema["type"], value) {
		return []string{fmt.Sprintf("%s = %v does not have type %v", path, value, schema["type"])}
	}
	if enum, ok := schema["enum"].([]any); ok && !containsValue(enum, value) {
		return []string{fmt.Sprintf("%s = %v is not in %v", path, value, enum)}
	}
	members, isObject := value.(map[string]any)
	if !isObject {
		return nil
	}
	var problems []string
	required, _ := schema["required"].([]string)
	for _, name := range required {
		if _, ok := members[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s has no required member %s", path, name))
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, member := range members {
		if property, ok := properties[name].(map[string]any); ok {
			problems = append(problems, validate(property, member, path+"."+name)...)
		}
	}
	return problems
}

func typeMatches(want any, value any) bool {
	switch want := want.(type) {
	case nil:
		return true
	case []any:
		for _, one := range want {
			if typeMatches(one, value) {
				return true
			}
		}
		return false
	case string:
		switch v := value.(type) {
		case string:
			return want == "string"
		case float64:
			return want == "number" || want == "integer" && v == math.Trunc(v)
		case bool:
			return want == "boolean"
		case []any:
			return want == "array"
		case map[string]any:
			return want == "object"
		case nil:
			return want == "null"
		}
	}
	return false
}

func containsValue(values []any, value any) bool {
	for _, one := range values {
		if one == value {
			return true
		}
	}
	return false
}

func TestMockToolCallsTheFirstTool(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	for name, tools := range map[string][]any{"opencode": openCodeTools, "other": otherTools} {
		t.Run(name, func(t *testing.T) {
			events, err := readEvents(t, post(t, srv, requestBody(t, "mock-tool", true, tools, userMessages), nil))
			if err != nil {
				t.Fatal(err)
			}
			streamed := assemble(t, events)
			message := readMessage(t, post(t, srv, requestBody(t, "mock-tool", false, tools, userMessages), nil))
			for _, block := range []gotBlock{streamed.blocks[0], message.Content[0]} {
				checkToolUse(t, block, tools)
				first := tools[0].(map[string]any)["name"]
				if block.Name != first || block.ID != "toolu_mock_tool_0" {
					t.Errorf("tool_use %s %s, want %s toolu_mock_tool_0", block.Name, block.ID, first)
				}
			}
		})
	}
}

// codingStepWant is what one step of the coding script calls.
type codingStepWant struct {
	tool  string
	input string // the exact input, or "" to check only the schema
}

func TestMockCodingPlaysTheWholeScript(t *testing.T) {
	cases := map[string]struct {
		tools []any
		steps []codingStepWant
	}{
		"opencode": {openCodeTools, []codingStepWant{
			{"read", `{"filePath":"calc.py"}`},
			{"edit", `{"filePath":"calc.py","newString":"return a + b","oldString":"return a - b"}`},
			{"bash", `{"command":"python -m unittest","description":"Run the tests"}`},
		}},
		"other": {otherTools, []codingStepWant{
			{"read_file", `{"encoding":"utf-8","path":"calc.py"}`},
			{"file_editor", `{"file_path":"calc.py","new_text":"return a + b",` +
				`"old_text":"return a - b","options":{"dry_run":false}}`},
			// mode is ["null","string"], so it gets the default string.
			{"run_bash", `{"args":[],"background":false,"cmd":"python -m unittest",` +
				`"mode":"mock","timeout_ms":1}`},
		}},
	}
	srv := newServer(t, DefaultTiming)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			playScript(t, srv, tc.tools, tc.steps)
		})
	}
}

// playScript sends the four requests of the script, each with the conversation so
// far, as a client that runs every tool would.
func playScript(t *testing.T, srv *httptest.Server, tools []any, steps []codingStepWant) {
	t.Helper()
	messages := append([]any(nil), userMessages...)
	for step := 0; step <= len(steps); step++ {
		events, err := readEvents(t, post(t, srv, requestBody(t, "mock-coding", true, tools, messages), nil))
		if err != nil {
			t.Fatal(err)
		}
		got := assemble(t, events)
		if want := fmt.Sprintf("msg_mock_coding_%d", step); got.messageID != want {
			t.Errorf("step %d: id %s, want %s", step, got.messageID, want)
		}
		if step == len(steps) {
			if got.stopReason != "end_turn" || len(got.blocks) != 1 || got.blocks[0].Text != codingAnswer {
				t.Errorf("last step: stop %s, blocks %+v", got.stopReason, got.blocks)
			}
			return
		}
		if got.stopReason != "tool_use" || len(got.blocks) != 2 || got.blocks[0].Type != "text" {
			t.Fatalf("step %d: stop %s, blocks %+v", step, got.stopReason, got.blocks)
		}
		call := got.blocks[1]
		checkToolUse(t, call, tools)
		want := steps[step]
		if call.Name != want.tool || want.input != "" && string(call.Input) != want.input {
			t.Errorf("step %d: %s %s, want %s %s", step, call.Name, call.Input, want.tool, want.input)
		}
		if wantID := fmt.Sprintf("toolu_mock_coding_%d", step); call.ID != wantID {
			t.Errorf("step %d: tool id %s, want %s", step, call.ID, wantID)
		}
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": got.blocks[0].Text},
				map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Input},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": "done"},
			}})
	}
}

func TestMockCodingFallsBackToTheFirstTool(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	tools := []any{toolDef("get_weather", object([]string{"location"}, map[string]any{"location": str}))}
	message := readMessage(t, post(t, srv, requestBody(t, "mock-coding", false, tools, userMessages), nil))
	if len(message.Content) != 2 {
		t.Fatalf("content %+v", message.Content)
	}
	checkToolUse(t, message.Content[1], tools)
	if got := string(message.Content[1].Input); got != `{"location":"mock"}` {
		t.Errorf("input %s", got)
	}
}

func TestToolInputKeepsOnlyDeclaredStringMembers(t *testing.T) {
	// A tool named read whose schema differs from OpenCode's: filePath is a number
	// and path is required.
	def := tool{Name: "read"}
	raw := `{"type":"object","required":["path","filePath"],` +
		`"properties":{"path":{"type":"string"},"filePath":{"type":"integer"}}}`
	if err := json.Unmarshal([]byte(raw), &def.InputSchema); err != nil {
		t.Fatal(err)
	}
	if got, want := string(toolInput(def, "")), `{"filePath":1,"path":"calc.py"}`; got != want {
		t.Errorf("input %s, want %s", got, want)
	}
	// A tool whose schema is not an object reads as having no constraints.
	var loose tool
	if err := json.Unmarshal([]byte(`{"name":"bash","input_schema":true}`), &loose); err != nil {
		t.Fatal(err)
	}
	if got, want := string(toolInput(loose, "")),
		`{"command":"python -m unittest","description":"Run the tests"}`; got != want {
		t.Errorf("input %s, want %s", got, want)
	}
}
