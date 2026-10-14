package mock

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

var runTestsTool = functionDef("run_tests", object([]string{"path"}, map[string]any{"path": str}))

// webSearch is a built-in tool, which the scenarios never call.
var webSearch = map[string]any{"type": "web_search"}

// codexToolDefs are shaped like Codex's tools. read_mcp_resource contains "read", so
// the Codex shell must win over the partial match.
var codexToolDefs = []any{
	functionDef("read_mcp_resource", object([]string{"server", "uri"},
		map[string]any{"server": str, "uri": str})),
	functionDef("shell", object([]string{"command"}, map[string]any{
		"command": map[string]any{"type": "array", "items": str}, "workdir": str, "timeout_ms": num})),
	functionDef("update_plan", object([]string{"plan"}, map[string]any{
		"plan": map[string]any{"type": "array"}, "explanation": str})),
	functionDef("apply_patch", object([]string{"input"}, map[string]any{"input": str})),
	webSearch,
}

func TestResponsesScenarioStreams(t *testing.T) {
	codingCall := `{"command":["bash","-lc","cat calc.py"]}`
	cases := []struct {
		model string
		tools []any
		id    string
		want  []string // event types
		items []gotItem
	}{
		{"mock-text", nil, "resp_mock_text_0",
			responseEvents(textItemEvents(words(textAnswer))),
			[]gotItem{messageItem("msg_mock_text_0", textAnswer)}},
		{"mock-tool", []any{webSearch, runTestsTool}, "resp_mock_tool_0",
			responseEvents(callItemEvents(inputParts)),
			[]gotItem{callItem("tool_0", "run_tests", `{"path":"calc.py"}`)}},
		{"mock-coding", codexToolDefs, "resp_mock_coding_0",
			responseEvents(textItemEvents(words(codingSteps[0].text)), callItemEvents(inputParts)),
			[]gotItem{messageItem("msg_mock_coding_0", codingSteps[0].text),
				callItem("coding_0", "shell", codingCall)}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			srv := newResponsesServer(t, DefaultTiming)
			body := responsesBody(t, tc.model, true, tc.tools, userInput)
			events, err := readEvents(t, postResponses(t, srv, body, nil))
			if err != nil {
				t.Fatalf("stream ended with %v", err)
			}
			if got := eventTypes(events); !slices.Equal(got, tc.want) {
				t.Fatalf("events\n%v\nwant\n%v", got, tc.want)
			}
			got := assembleResponse(t, events)
			checkResponse(t, "created", got.created, tc.id, tc.model, statusInProgress, nil)
			checkResponse(t, "completed", got.completed, tc.id, tc.model, statusCompleted, tc.items)
			if !reflect.DeepEqual(got.items, tc.items) {
				t.Errorf("done items %+v, want %+v", got.items, tc.items)
			}
			checkUsage(t, "stream", got.usage, body, got.deltas)

			plain := responsesBody(t, tc.model, false, tc.tools, userInput)
			response, usage := readResponseBody(t, postResponses(t, srv, plain, nil))
			checkResponse(t, "body", response, tc.id, tc.model, statusCompleted, tc.items)
			checkUsage(t, "body", usage, plain, got.deltas)

			for _, request := range [][]byte{body, plain} {
				first, second := rawResponses(t, srv, request), rawResponses(t, srv, request)
				if !bytes.Equal(first, second) {
					t.Errorf("two runs differ:\n%s\n%s", first, second)
				}
			}
		})
	}
}

func messageItem(id, text string) gotItem {
	item := gotItem{ID: id, Type: "message", Status: statusCompleted, Role: "assistant"}
	item.Content = append(item.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"output_text", text})
	return item
}

func callItem(key, name, arguments string) gotItem {
	return gotItem{ID: "fc_mock_" + key, Type: "function_call", Status: statusCompleted,
		Name: name, CallID: "call_mock_" + key, Arguments: arguments}
}

func checkResponse(t *testing.T, what string, got gotResponse, id, model, status string,
	items []gotItem) {
	t.Helper()
	if got.ID != id || got.Object != "response" || got.Model != model || got.Status != status {
		t.Errorf("%s: id %q object %q model %q status %q, want %q response %q %q", what, got.ID,
			got.Object, got.Model, got.Status, id, model, status)
	}
	if len(got.Output) != len(items) || len(items) > 0 && !reflect.DeepEqual(got.Output, items) {
		t.Errorf("%s: output %+v, want %+v", what, got.Output, items)
	}
}

func rawResponses(t *testing.T, srv *httptest.Server, body []byte) []byte {
	t.Helper()
	data, err := io.ReadAll(postResponses(t, srv, body, nil).Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// codingInput is the input after the first done steps of mock-coding: each step adds
// its function_call and the function_call_output that answers it.
func codingInput(done int) []any {
	input := slices.Clone(userInput)
	for step := range done {
		callID := "call_mock_coding_" + strconv.Itoa(step)
		input = append(input,
			map[string]any{"type": "function_call", "name": "shell", "arguments": "{}", "call_id": callID},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": "done"})
	}
	return input
}

func TestResponsesCodingCountsFunctionCallOutputs(t *testing.T) {
	steps := []struct {
		tool, arguments string
	}{
		{"shell", `{"command":["bash","-lc","cat calc.py"]}`},
		{"apply_patch", string(mustMarshal(map[string]string{"input": codexPatch}))},
		{"shell", `{"command":["bash","-lc","python -m unittest"]}`},
	}
	srv := newResponsesServer(t, DefaultTiming)
	for step, want := range steps {
		body := responsesBody(t, "mock-coding", false, codexToolDefs, codingInput(step))
		response, _ := readResponseBody(t, postResponses(t, srv, body, nil))
		if wantID := "resp_mock_coding_" + strconv.Itoa(step); response.ID != wantID {
			t.Errorf("step %d: id %s, want %s", step, response.ID, wantID)
		}
		if len(response.Output) != 2 || response.Output[0].text() != codingSteps[step].text {
			t.Fatalf("step %d: output %+v", step, response.Output)
		}
		if call := response.Output[1]; call.Name != want.tool || call.Arguments != want.arguments {
			t.Errorf("step %d: call %s %s, want %s %s", step, call.Name, call.Arguments,
				want.tool, want.arguments)
		}
	}
	for _, done := range []int{3, 4} {
		body := responsesBody(t, "mock-coding", false, codexToolDefs, codingInput(done))
		response, _ := readResponseBody(t, postResponses(t, srv, body, nil))
		if response.ID != "resp_mock_coding_3" || len(response.Output) != 1 ||
			response.Output[0].text() != codingAnswer {
			t.Errorf("%d results: id %s output %+v, want the answer", done, response.ID, response.Output)
		}
	}
}

func TestResponsesCodingEditsThroughTheShell(t *testing.T) {
	commandLine := map[string]any{"type": "string"}
	cases := []struct {
		tool      map[string]any
		arguments string
	}{
		{functionDef("shell", object([]string{"command"}, map[string]any{
			"command": map[string]any{"type": "array", "items": str}})),
			string(mustMarshal(map[string]any{"command": []string{"apply_patch", codexPatch}}))},
		{functionDef("shell_command", object([]string{"command"}, map[string]any{"command": commandLine})),
			string(mustMarshal(map[string]any{"command": codexShellPatch}))},
		{functionDef("exec_command", object([]string{"cmd"}, map[string]any{"cmd": commandLine})),
			string(mustMarshal(map[string]any{"cmd": codexShellPatch}))},
	}
	srv := newResponsesServer(t, DefaultTiming)
	for _, tc := range cases {
		tools := []any{codexToolDefs[0], tc.tool}
		body := responsesBody(t, "mock-coding", false, tools, codingInput(1))
		response, _ := readResponseBody(t, postResponses(t, srv, body, nil))
		if len(response.Output) != 2 || response.Output[1].Name != tc.tool["name"] ||
			response.Output[1].Arguments != tc.arguments {
			t.Errorf("%s: output %+v, want arguments %s", tc.tool["name"], response.Output, tc.arguments)
		}
	}
}

func TestResponsesCodingWithoutFunctionTools(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	for _, tools := range [][]any{nil, {webSearch}} {
		body := responsesBody(t, "mock-coding", false, tools, "Name this session.")
		response, _ := readResponseBody(t, postResponses(t, srv, body, nil))
		if len(response.Output) != 1 || response.Output[0].text() != codingNoToolsAnswer {
			t.Errorf("tools %v: output %+v", tools, response.Output)
		}
	}
}

func TestResponsesErrorStatusScenarios(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	cases := []struct {
		model      string
		status     int
		errorType  string
		retryAfter string
	}{
		{"mock-429", http.StatusTooManyRequests, "rate_limit_error", "1"},
		{"mock-500", http.StatusInternalServerError, "api_error", ""},
	}
	for _, tc := range cases {
		for _, stream := range []bool{true, false} {
			resp := postResponses(t, srv, responsesBody(t, tc.model, stream, nil, userInput), nil)
			if resp.StatusCode != tc.status {
				t.Errorf("%s stream=%v: status %d, want %d", tc.model, stream, resp.StatusCode, tc.status)
			}
			if got := resp.Header.Get("retry-after"); got != tc.retryAfter {
				t.Errorf("%s: retry-after %q, want %q", tc.model, got, tc.retryAfter)
			}
			if body := readResponsesError(t, resp); body.Error.Type != tc.errorType {
				t.Errorf("%s: error type %q, want %q", tc.model, body.Error.Type, tc.errorType)
			}
		}
	}
}

func TestResponsesBadRequests(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	cases := []struct {
		name      string
		body      []byte
		status    int
		errorType string
	}{
		{"invalid JSON", []byte(`{"model":"mock-text",`), 400, "invalid_request_error"},
		{"not an object", []byte(`[1,2]`), 400, "invalid_request_error"},
		{"input of the wrong type", responsesBody(t, "mock-text", false, nil, 7), 400,
			"invalid_request_error"},
		{"mock-tool without tools", responsesBody(t, "mock-tool", true, nil, userInput), 400,
			"invalid_request_error"},
		{"mock-tool with only built-in tools", responsesBody(t, "mock-tool", false,
			[]any{webSearch}, userInput), 400, "invalid_request_error"},
		{"unknown model", responsesBody(t, "gpt-5", true, codexToolDefs, userInput), 404,
			"not_found_error"},
	}
	for _, tc := range cases {
		resp := postResponses(t, srv, tc.body, nil)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
		if body := readResponsesError(t, resp); body.Error.Type != tc.errorType {
			t.Errorf("%s: error type %q, want %q", tc.name, body.Error.Type, tc.errorType)
		}
	}
}

func TestResponsesTextInputString(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	response, _ := readResponseBody(t, postResponses(t, srv,
		responsesBody(t, "mock-text", false, nil, "Hello"), nil))
	if len(response.Output) != 1 || response.Output[0].text() != textAnswer {
		t.Errorf("output %+v", response.Output)
	}
}

func TestResponsesDropClosesTheConnectionAfterKEvents(t *testing.T) {
	cases := []struct {
		timing Timing
		header map[string]string
		want   int
	}{
		{DefaultTiming, nil, 3},
		{Timing{FailAfterEvents: 5}, nil, 5},
		{DefaultTiming, map[string]string{"X-Mock-Fail-After-Events": "1"}, 1},
		{Timing{FailAfterEvents: 0}, nil, 0},
		{Timing{FailAfterEvents: 10000}, nil, len(responseEvents(textItemEvents(words(longAnswer)))) - 1},
	}
	for _, tc := range cases {
		srv := newResponsesServer(t, tc.timing)
		resp := postResponses(t, srv, responsesBody(t, "mock-drop", true, nil, userInput), tc.header)
		events, err := readEvents(t, resp)
		if err == nil || errors.Is(err, io.EOF) {
			t.Errorf("K=%d: stream ended with %v, want a broken connection", tc.want, err)
		}
		types := eventTypes(events)
		want := responseEvents(textItemEvents(words(longAnswer)))[:tc.want]
		if !slices.Equal(types, want) {
			t.Errorf("events %v, want %v", types, want)
		}
	}
}

func TestResponsesDropWithoutStreamClosesTheConnection(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	body := responsesBody(t, "mock-drop", false, nil, userInput)
	resp, err := srv.Client().Post(srv.URL+"/v1/responses", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got status %d, want no response", resp.StatusCode)
	}
}

func TestResponsesErrorEventAfterKEvents(t *testing.T) {
	for _, k := range []int{0, 3, 7} {
		srv := newResponsesServer(t, Timing{FailAfterEvents: k})
		resp := postResponses(t, srv, responsesBody(t, "mock-error-event", true, nil, userInput), nil)
		events, err := readEvents(t, resp)
		if err != nil {
			t.Fatalf("K=%d: stream ended with %v, want a clean end", k, err)
		}
		want := append(responseEvents(textItemEvents(words(longAnswer)))[:k], openai.EventError)
		if types := eventTypes(events); !slices.Equal(types, want) {
			t.Fatalf("K=%d: events %v, want %v", k, types, want)
		}
		var data struct {
			Type           string  `json:"type"`
			Code           string  `json:"code"`
			Message        string  `json:"message"`
			Param          *string `json:"param"`
			SequenceNumber *int64  `json:"sequence_number"`
		}
		if err := json.Unmarshal(events[k].Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Code != "overloaded_error" || data.Message == "" || data.Param != nil ||
			data.SequenceNumber == nil || *data.SequenceNumber != int64(k) {
			t.Errorf("K=%d: error event %s", k, events[k].Data)
		}
	}
}

func TestResponsesErrorEventWithoutStreamIsOverloaded(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	resp := postResponses(t, srv, responsesBody(t, "mock-error-event", false, nil, userInput), nil)
	if resp.StatusCode != statusOverloaded {
		t.Errorf("status %d, want %d", resp.StatusCode, statusOverloaded)
	}
	if body := readResponsesError(t, resp); body.Error.Type != "overloaded_error" {
		t.Errorf("error type %q", body.Error.Type)
	}
}
