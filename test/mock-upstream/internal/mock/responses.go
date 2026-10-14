package mock

import (
	"encoding/json"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

// responses is the OpenAI Responses protocol of POST /v1/responses. Unlike Messages,
// it does not require max_output_tokens, and the mock keeps no state, so it ignores
// previous_response_id.
type responses struct{}

// readRequest reads model, stream, the function tools and the function_call_output
// items of a Responses request. input may be a string, which holds no items.
func (responses) readRequest(body []byte) (*request, *apiError) {
	parsed, err := openai.ParseRequest(body)
	if err != nil {
		return nil, invalidRequest("request body is not valid JSON: " + err.Error())
	}
	var members struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, invalidRequest("input has the wrong shape")
	}
	outputs, ok := functionCallOutputs(members.Input)
	if !ok {
		return nil, invalidRequest("input: must be a string or an array of items")
	}
	return &request{model: parsed.Model, stream: parsed.Stream, bodyBytes: len(body),
		toolResults: outputs, tools: functionTools(parsed.Tools)}, nil
}

// functionCallOutputs counts the function_call_output items of input. It reports
// false when input is neither absent, null, a string nor an array of objects.
func functionCallOutputs(input json.RawMessage) (int, bool) {
	var text *string
	if isAbsent(input) || json.Unmarshal(input, &text) == nil {
		return 0, true
	}
	var items []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(input, &items) != nil {
		return 0, false
	}
	n := 0
	for _, item := range items {
		if item.Type == "function_call_output" {
			n++
		}
	}
	return n, true
}

// functionTools returns the named function tools of the request, with their
// parameters as the input schema. Built-in and custom tools take no JSON arguments,
// so the scenarios never call them.
func functionTools(tools []openai.Tool) []tool {
	var functions []tool
	for _, t := range tools {
		if t.Type != "function" || t.Name == "" {
			continue
		}
		var def struct {
			Parameters schema `json:"parameters"`
		}
		// ParseRequest has checked that the element is an object, and a schema always
		// decodes, so this cannot fail.
		_ = json.Unmarshal(t.Raw, &def)
		functions = append(functions, tool{Name: t.Name, InputSchema: def.Parameters})
	}
	return functions
}

func isAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func (responses) errorBody(failure *apiError) any {
	return openai.NewErrorBody(failure.errorType, failure.message)
}

func (responses) errorEvent(sent int) event {
	data := openai.EncodeErrorEvent(errorTypeOverloaded, overloadedMessage, int64(sent))
	return event{name: openai.EventError, data: data}
}
