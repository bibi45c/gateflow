package mock

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// successCases are the scenarios that answer with a whole message.
var successCases = []struct {
	model      string
	id         string
	stopReason string
	blockTypes []string
}{
	{"mock-text", "msg_mock_text_0", "end_turn", []string{"text"}},
	{"mock-tool", "msg_mock_tool_0", "tool_use", []string{"tool_use"}},
	{"mock-coding", "msg_mock_coding_0", "tool_use", []string{"text", "tool_use"}},
}

func TestStreamSendsTheFullSequenceWithUsage(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	for _, tc := range successCases {
		t.Run(tc.model, func(t *testing.T) {
			body := requestBody(t, tc.model, true, openCodeTools, userMessages)
			resp := post(t, srv, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			events, err := readEvents(t, resp)
			if err != nil {
				t.Fatalf("stream ended with %v", err)
			}
			got := assemble(t, events)
			if got.messageID != tc.id || got.model != tc.model || got.stopReason != tc.stopReason {
				t.Errorf("id %q model %q stop %q, want %q %q %q", got.messageID, got.model,
					got.stopReason, tc.id, tc.model, tc.stopReason)
			}
			var types []string
			for _, block := range got.blocks {
				types = append(types, block.Type)
			}
			if !slices.Equal(types, tc.blockTypes) {
				t.Errorf("blocks %v, want %v", types, tc.blockTypes)
			}
			start, end := got.usageStart, got.usageDelta
			if count(start.InputTokens) != wantInputTokens(body) || count(start.OutputTokens) != 0 {
				t.Errorf("message_start usage in %d out %d, want in %d out 0",
					count(start.InputTokens), count(start.OutputTokens), wantInputTokens(body))
			}
			if count(start.CacheReadInputTokens) != 0 || count(start.CacheCreationInputTokens) != 0 {
				t.Errorf("message_start cache counts %+v, want 0", start)
			}
			if count(end.OutputTokens) != got.deltas || end.InputTokens != nil {
				t.Errorf("message_delta usage out %d in %d, want out %d and no input",
					count(end.OutputTokens), count(end.InputTokens), got.deltas)
			}
			if got.deltas < 2 {
				t.Errorf("only %d deltas", got.deltas)
			}
		})
	}
}

func TestToolInputStreamsInSeveralDeltas(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	resp := post(t, srv, requestBody(t, "mock-tool", true, openCodeTools, userMessages), nil)
	events, err := readEvents(t, resp)
	if err != nil {
		t.Fatal(err)
	}
	var deltas int
	for _, event := range events {
		if bytes.Contains(event.Data, []byte(`"input_json_delta"`)) {
			deltas++
		}
	}
	if deltas < 2 {
		t.Errorf("tool input sent in %d deltas, want several", deltas)
	}
}

type gotMessage struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Role         string     `json:"role"`
	Model        string     `json:"model"`
	Content      []gotBlock `json:"content"`
	StopReason   *string    `json:"stop_reason"`
	StopSequence *string    `json:"stop_sequence"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// readMessage reads a non-streaming response and checks the members every message
// has.
func readMessage(t *testing.T, resp *http.Response) gotMessage {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id", "type", "role", "model", "content", "stop_reason",
		"stop_sequence", "usage"} {
		if _, ok := members[name]; !ok {
			t.Errorf("message has no %s: %s", name, data)
		}
	}
	var message gotMessage
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "message" || message.Role != "assistant" || message.StopSequence != nil {
		t.Errorf("type %q role %q stop_sequence %v", message.Type, message.Role, message.StopSequence)
	}
	return message
}

func TestNonStreamingMessageMatchesTheStream(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	for _, tc := range successCases {
		t.Run(tc.model, func(t *testing.T) {
			body := requestBody(t, tc.model, false, openCodeTools, userMessages)
			message := readMessage(t, post(t, srv, body, nil))
			events, err := readEvents(t, post(t, srv,
				requestBody(t, tc.model, true, openCodeTools, userMessages), nil))
			if err != nil {
				t.Fatal(err)
			}
			streamed := assemble(t, events)
			if message.ID != tc.id || message.Model != tc.model || message.StopReason == nil ||
				*message.StopReason != tc.stopReason {
				t.Errorf("id %q model %q stop %v", message.ID, message.Model, message.StopReason)
			}
			if message.Usage.InputTokens != wantInputTokens(body) {
				t.Errorf("input_tokens %d, want %d", message.Usage.InputTokens, wantInputTokens(body))
			}
			if message.Usage.OutputTokens != streamed.deltas {
				t.Errorf("output_tokens %d, want the stream's %d deltas",
					message.Usage.OutputTokens, streamed.deltas)
			}
			if len(message.Content) != len(streamed.blocks) {
				t.Fatalf("content %+v, stream %+v", message.Content, streamed.blocks)
			}
			for i, block := range message.Content {
				other := streamed.blocks[i]
				if block.Type != other.Type || block.Text != other.Text || block.ID != other.ID ||
					block.Name != other.Name || string(block.Input) != string(other.Input) {
					t.Errorf("block %d: message %+v, stream %+v", i, block, other)
				}
			}
		})
	}
}

func TestSameRequestGivesTheSameBytes(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	models := []string{"mock-text", "mock-tool", "mock-coding", "mock-429", "mock-500",
		"mock-drop", "mock-error-event", "mock-unknown"}
	for _, model := range models {
		for _, stream := range []bool{true, false} {
			if model == "mock-drop" && !stream {
				continue // no response at all
			}
			body := requestBody(t, model, stream, openCodeTools, userMessages)
			first, second := rawResponse(t, srv, body), rawResponse(t, srv, body)
			if !bytes.Equal(first, second) {
				t.Errorf("%s stream=%v: two runs differ:\n%s\n%s", model, stream, first, second)
			}
			if len(first) == 0 {
				t.Errorf("%s stream=%v: empty body", model, stream)
			}
		}
	}
}

// rawResponse returns the status, headers that the mock sets, and body of a response,
// keeping what arrived before a dropped connection.
func rawResponse(t *testing.T, srv *httptest.Server, body []byte) []byte {
	t.Helper()
	resp := post(t, srv, body, nil)
	data, _ := io.ReadAll(resp.Body)
	var out bytes.Buffer
	out.WriteString(resp.Status + "\n")
	for _, name := range []string{"content-type", "cache-control", "retry-after"} {
		out.WriteString(name + ": " + resp.Header.Get(name) + "\n")
	}
	out.Write(data)
	return out.Bytes()
}
