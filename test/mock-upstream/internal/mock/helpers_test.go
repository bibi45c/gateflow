package mock

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

func newServer(t *testing.T, timing Timing) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewHandler(timing))
	t.Cleanup(srv.Close)
	return srv
}

// userMessages is the conversation of a first request.
var userMessages = []any{map[string]any{"role": "user", "content": "Fix the add function in calc.py."}}

func requestBody(t *testing.T, model string, stream bool, tools, messages []any) []byte {
	t.Helper()
	body := map[string]any{"model": model, "max_tokens": 1024, "stream": stream, "messages": messages}
	if tools != nil {
		body["tools"] = tools
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func post(t *testing.T, srv *httptest.Server, body []byte, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	for name, value := range header {
		req.Header.Set(name, value)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readEvents reads every complete event of a stream and returns the error that
// ended it, or nil when it ended cleanly.
func readEvents(t *testing.T, resp *http.Response) ([]anthropic.Event, error) {
	t.Helper()
	if got := resp.Header.Get("content-type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", got)
	}
	reader := anthropic.NewReader(resp.Body)
	var events []anthropic.Event
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		if event.Name != event.Type() {
			t.Errorf("event name %q differs from data type %q", event.Name, event.Type())
		}
		events = append(events, event)
	}
}

func eventTypes(events []anthropic.Event) []string {
	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.Type()
	}
	return types
}

// gotBlock is a content block as a client reads it.
type gotBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

// streamResult is what a client puts together from a whole stream.
type streamResult struct {
	messageID  string
	model      string
	blocks     []gotBlock
	stopReason string
	deltas     int64
	usageStart anthropic.Usage
	usageDelta anthropic.Usage
}

type eventData struct {
	Type         string   `json:"type"`
	Index        int      `json:"index"`
	ContentBlock gotBlock `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	} `json:"message"`
}

// assemble checks the order of a complete stream and puts its message together.
func assemble(t *testing.T, events []anthropic.Event) streamResult {
	t.Helper()
	var result streamResult
	var input []string
	var err error
	want := ""
	for _, event := range events {
		var data eventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("event %s: %v", event.Name, err)
		}
		if !nextAllowed(want, data.Type) {
			t.Fatalf("event %s after %s; sequence %v", data.Type, want, eventTypes(events))
		}
		want = data.Type
		switch data.Type {
		case anthropic.EventMessageStart:
			result.messageID, result.model = data.Message.ID, data.Message.Model
			result.usageStart, err = anthropic.MessageStartUsage(event.Data)
		case anthropic.EventContentBlockStart:
			if data.Index != len(result.blocks) {
				t.Fatalf("block index %d, want %d", data.Index, len(result.blocks))
			}
			result.blocks = append(result.blocks, data.ContentBlock)
			input = append(input, "")
		case anthropic.EventContentBlockDelta:
			result.deltas++
			block := &result.blocks[data.Index]
			switch data.Delta.Type {
			case "text_delta":
				block.Text += data.Delta.Text
			case "input_json_delta":
				input[data.Index] += data.Delta.PartialJSON
			default:
				t.Fatalf("delta type %q", data.Delta.Type)
			}
		case anthropic.EventMessageDelta:
			result.stopReason = data.Delta.StopReason
			result.usageDelta, err = anthropic.MessageDeltaUsage(event.Data)
		}
		if err != nil {
			t.Fatalf("event %s usage: %v", data.Type, err)
		}
	}
	if want != anthropic.EventMessageStop {
		t.Fatalf("stream ends with %s, want message_stop", want)
	}
	for i := range result.blocks {
		if result.blocks[i].Type == "tool_use" {
			result.blocks[i].Input = json.RawMessage(input[i])
		}
	}
	return result
}

// nextAllowed reports whether an event of type next may follow one of type prev,
// where prev is "" for the first event.
func nextAllowed(prev, next string) bool {
	switch next {
	case anthropic.EventMessageStart:
		return prev == ""
	case anthropic.EventContentBlockStart:
		return prev == anthropic.EventMessageStart || prev == anthropic.EventContentBlockStop
	case anthropic.EventContentBlockDelta:
		return prev == anthropic.EventContentBlockStart || prev == anthropic.EventContentBlockDelta
	case anthropic.EventContentBlockStop:
		return prev == anthropic.EventContentBlockDelta
	case anthropic.EventMessageDelta:
		return prev == anthropic.EventContentBlockStop
	case anthropic.EventMessageStop:
		return prev == anthropic.EventMessageDelta
	}
	return false
}

// wantInputTokens is the documented formula: ceil(body bytes / 4).
func wantInputTokens(body []byte) int64 {
	return int64(math.Ceil(float64(len(body)) / 4))
}

func count(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// readError reads an Anthropic error body.
func readError(t *testing.T, resp *http.Response) anthropic.ErrorBody {
	t.Helper()
	if got := resp.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	var body anthropic.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Message == "" {
		t.Errorf("error body = %+v", body)
	}
	return body
}
