package openai

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

func TestNewErrorBodyEncodesTheResponsesShape(t *testing.T) {
	plain := NewErrorBody(ErrorTypeAPI, "Internal server error.")
	withParam := NewErrorBody(ErrorTypeInvalidRequest, "previous_response_id is not supported.").WithParam("previous_response_id")
	cases := []struct {
		body ErrorBody
		want string
	}{
		{plain, `{"error":{"message":"Internal server error.","type":"api_error","param":null,"code":null}}`},
		{withParam, `{"error":{"message":"previous_response_id is not supported.","type":"invalid_request_error",` +
			`"param":"previous_response_id","code":null}}`},
	}
	for _, c := range cases {
		encoded, err := json.Marshal(c.body)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != c.want {
			t.Errorf("got %s, want %s", encoded, c.want)
		}
	}
	if base := NewErrorBody(ErrorTypeAPI, "x"); base.WithParam("p").Error.Param == nil || base.Error.Param != nil {
		t.Error("WithParam must set param on a copy only")
	}
}

func TestErrorBodyDecodesOllamaError(t *testing.T) {
	recorded := readFixture(t, "ollama-unknown-model.response.json")
	var body ErrorBody
	if err := json.Unmarshal(recorded, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "model 'no-such-model' not found" || body.Error.Type != "not_found_error" ||
		body.Error.Param != nil || body.Error.Code != nil {
		t.Errorf("got %+v", body.Error)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(encoded, '\n'), recorded) {
		t.Errorf("re-encoded as %s, recorded as %s", encoded, recorded)
	}
}

func TestEncodeErrorEvent(t *testing.T) {
	data := EncodeErrorEvent(ErrorTypeAPI, "The upstream model service failed.", 12)
	want := `{"type":"error","code":"api_error","message":"The upstream model service failed.","param":null,"sequence_number":12}`
	if string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
	// The example in api/errors.md, with its spaces.
	contract := `{"type": "error", "code": "api_error", "message": "The upstream model service failed.", "param": null, "sequence_number": 12}`
	var got, documented map[string]any
	if json.Unmarshal(data, &got) != nil || json.Unmarshal([]byte(contract), &documented) != nil || !reflect.DeepEqual(got, documented) {
		t.Errorf("got %v, api/errors.md gives %v", got, documented)
	}

	var out bytes.Buffer
	if err := sse.NewWriter(&out).WriteEvent(EventError, data); err != nil {
		t.Fatal(err)
	}
	if out.String() != "event: error\ndata: "+want+"\n\n" {
		t.Errorf("framed as %q", out.String())
	}
	events := readAll(t, &out)
	if len(events) != 1 || events[0].Name != EventError || events[0].Type() != EventError {
		t.Fatalf("read back as %+v", events)
	}
	if _, end := EndOf(events[0].Type()); end {
		t.Error("an error event counts as an end event")
	}
}

func int64p(n int64) *int64 {
	return &n
}

// fullUsageFixture is the usage of the recorded tool stream, with every count set.
func fullUsageFixture() Usage {
	return Usage{InputTokens: int64p(157), CachedTokens: int64p(0), OutputTokens: int64p(19),
		ReasoningTokens: int64p(0), TotalTokens: int64p(176)}
}

func TestEncodeCompletedEvent(t *testing.T) {
	item := `{"type":"message","id":"msg_1","status":"completed","role":"assistant",` +
		`"content":[{"type":"output_text","text":"OK.","annotations":[]}]}`
	data, err := EncodeCompletedEvent(CompletedResponse{
		ID:        "resp_1",
		CreatedAt: 1791973716,
		Model:     "mock-small",
		Output:    []json.RawMessage{json.RawMessage(item)},
		Usage:     fullUsageFixture(),
	}, 9)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"response.completed","sequence_number":9,"response":{"id":"resp_1","object":"response",` +
		`"created_at":1791973716,"status":"completed","model":"mock-small","output":[` + item + `],` +
		`"usage":{"input_tokens":157,"input_tokens_details":{"cached_tokens":0},"output_tokens":19,` +
		`"output_tokens_details":{"reasoning_tokens":0},"total_tokens":176}}}`
	if string(data) != want {
		t.Fatalf("got %s\nwant %s", data, want)
	}
	usage, err := EventUsage(data)
	if err != nil || describe(usage) != "input=157 output=19 total=176 cached=0 reasoning=0" {
		t.Errorf("read back usage %s, %v", describe(usage), err)
	}
	if kind, end := EndOf(sse.Event{Data: data}.Type()); kind != EndCompleted || !end {
		t.Errorf("read back as %v, %v", kind, end)
	}

	empty, err := EncodeCompletedEvent(CompletedResponse{ID: "resp_2", Usage: fullUsageFixture()}, 0)
	if err != nil || !strings.Contains(string(empty), `"output":[],"usage":{`) {
		t.Errorf("without output: %s, %v", empty, err)
	}
}

func TestEncodeCompletedEventRefusesMissingIDOrUsage(t *testing.T) {
	cases := map[string]func(*CompletedResponse){
		"no id":               func(r *CompletedResponse) { r.ID = "" },
		"no input tokens":     func(r *CompletedResponse) { r.Usage.InputTokens = nil },
		"no cached tokens":    func(r *CompletedResponse) { r.Usage.CachedTokens = nil },
		"no output tokens":    func(r *CompletedResponse) { r.Usage.OutputTokens = nil },
		"no reasoning tokens": func(r *CompletedResponse) { r.Usage.ReasoningTokens = nil },
		"no total tokens":     func(r *CompletedResponse) { r.Usage.TotalTokens = nil },
		"no usage":            func(r *CompletedResponse) { r.Usage = Usage{} },
		"invalid output item": func(r *CompletedResponse) { r.Output = []json.RawMessage{json.RawMessage(`{"type":`)} },
	}
	for name, change := range cases {
		response := CompletedResponse{ID: "resp_1", Model: "m", Usage: fullUsageFixture()}
		change(&response)
		if data, err := EncodeCompletedEvent(response, 1); err == nil || data != nil {
			t.Errorf("%s: got %s, %v, want an error", name, data, err)
		}
	}
}
