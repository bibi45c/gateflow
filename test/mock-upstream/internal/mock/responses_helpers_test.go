package mock

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

func newResponsesServer(t *testing.T, timing Timing) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewResponsesHandler(timing))
	t.Cleanup(srv.Close)
	return srv
}

// userInput is the input of a first Responses request.
var userInput = []any{map[string]any{"type": "message", "role": "user",
	"content": []any{map[string]any{"type": "input_text", "text": "Fix the add function in calc.py."}}}}

// responsesBody returns a Responses request without max_output_tokens, as Codex
// sends it.
func responsesBody(t *testing.T, model string, stream bool, tools []any, input any) []byte {
	t.Helper()
	body := map[string]any{"model": model, "stream": stream, "input": input, "store": false}
	if tools != nil {
		body["tools"] = tools
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// functionDef returns a Responses function tool.
func functionDef(name string, parameters map[string]any) map[string]any {
	return map[string]any{"type": "function", "name": name, "description": "Test tool " + name,
		"strict": false, "parameters": parameters}
}

func postResponses(t *testing.T, srv *httptest.Server, body []byte, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", bytes.NewReader(body))
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

// gotItem is an output item as a client reads it.
type gotItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Role      string `json:"role"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// text returns the text of a message item.
func (item gotItem) text() string {
	var text strings.Builder
	for _, part := range item.Content {
		text.WriteString(part.Text)
	}
	return text.String()
}

// gotResponse is a response object as a client reads it.
type gotResponse struct {
	ID     string    `json:"id"`
	Object string    `json:"object"`
	Status string    `json:"status"`
	Model  string    `json:"model"`
	Output []gotItem `json:"output"`
}

// responseResult is what a client puts together from a whole Responses stream.
type responseResult struct {
	created   gotResponse
	completed gotResponse
	items     []gotItem // from output_item.done
	deltas    int64
	usage     openai.Usage
}

type responseEventData struct {
	Type           string      `json:"type"`
	SequenceNumber *int64      `json:"sequence_number"`
	Response       gotResponse `json:"response"`
	OutputIndex    int         `json:"output_index"`
	ItemID         string      `json:"item_id"`
	Item           gotItem     `json:"item"`
	Delta          string      `json:"delta"`
	Text           string      `json:"text"`
	Arguments      string      `json:"arguments"`
}

// assembleResponse checks that a complete stream numbers its events from 0, that
// each item's deltas add up to what its done events say, and puts the response
// together.
func assembleResponse(t *testing.T, events []sse.Event) responseResult {
	t.Helper()
	var result responseResult
	var streamed []string // the text or arguments of each item, from its deltas
	for i, event := range events {
		var data responseEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("event %s: %v", event.Name, err)
		}
		if data.SequenceNumber == nil || *data.SequenceNumber != int64(i) {
			t.Fatalf("event %d (%s): sequence_number %v", i, data.Type, data.SequenceNumber)
		}
		switch data.Type {
		case openai.EventCreated:
			result.created = data.Response
		case openai.EventOutputItemAdded:
			if data.OutputIndex != len(streamed) || data.Item.Status != statusInProgress {
				t.Fatalf("item added at %d with status %s, want %d in_progress", data.OutputIndex,
					data.Item.Status, len(streamed))
			}
			streamed = append(streamed, "")
		case openai.EventOutputTextDelta, openai.EventFunctionCallArgumentsDelta:
			result.deltas++
			streamed[data.OutputIndex] += data.Delta
		case openai.EventOutputTextDone:
			checkSame(t, data.Type, data.Text, streamed[data.OutputIndex])
		case openai.EventFunctionCallArgumentsDone:
			checkSame(t, data.Type, data.Arguments, streamed[data.OutputIndex])
		case openai.EventOutputItemDone:
			checkSame(t, data.Type, data.Item.text()+data.Item.Arguments, streamed[data.OutputIndex])
			result.items = append(result.items, data.Item)
		case openai.EventCompleted:
			result.completed = data.Response
			usage, err := openai.EventUsage(event.Data)
			if err != nil {
				t.Fatalf("completed usage: %v", err)
			}
			result.usage = usage
		}
	}
	return result
}

func checkSame(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s says %q, the deltas %q", what, got, want)
	}
}

// textItemEvents and callItemEvents are the event types of one output item.
func textItemEvents(deltas int) []string {
	types := []string{openai.EventOutputItemAdded, openai.EventContentPartAdded}
	types = append(types, slices.Repeat([]string{openai.EventOutputTextDelta}, deltas)...)
	return append(types, openai.EventOutputTextDone, openai.EventContentPartDone,
		openai.EventOutputItemDone)
}

func callItemEvents(deltas int) []string {
	types := []string{openai.EventOutputItemAdded}
	types = append(types, slices.Repeat([]string{openai.EventFunctionCallArgumentsDelta}, deltas)...)
	return append(types, openai.EventFunctionCallArgumentsDone, openai.EventOutputItemDone)
}

// responseEvents is the event types of a whole stream with the given items.
func responseEvents(items ...[]string) []string {
	types := []string{openai.EventCreated, openai.EventInProgress}
	for _, item := range items {
		types = append(types, item...)
	}
	return append(types, openai.EventCompleted)
}

func words(text string) int {
	return len(strings.SplitAfter(text, " "))
}

// readResponseBody reads a non-streaming Responses body and its usage.
func readResponseBody(t *testing.T, resp *http.Response) (gotResponse, openai.Usage) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body gotResponse
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	usage, err := openai.ResponseUsage(data)
	if err != nil {
		t.Fatal(err)
	}
	return body, usage
}

// readResponsesError reads a Responses error body.
func readResponsesError(t *testing.T, resp *http.Response) openai.ErrorBody {
	t.Helper()
	if got := resp.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	var raw map[string]map[string]any
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("error body %s: %v", data, err)
	}
	for _, member := range []string{"message", "type", "param", "code"} {
		if _, ok := raw["error"][member]; !ok {
			t.Errorf("error body %s lacks error.%s", data, member)
		}
	}
	var body openai.ErrorBody
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message == "" {
		t.Errorf("error body %s has no message", data)
	}
	return body
}

// checkUsage checks the usage formulas: input_tokens = ceil(body bytes / 4),
// output_tokens = deltas, total_tokens their sum, and the details 0.
func checkUsage(t *testing.T, what string, usage openai.Usage, body []byte, deltas int64) {
	t.Helper()
	in := wantInputTokens(body)
	if count(usage.InputTokens) != in || count(usage.OutputTokens) != deltas ||
		count(usage.TotalTokens) != in+deltas || count(usage.CachedTokens) != 0 ||
		count(usage.ReasoningTokens) != 0 {
		t.Errorf("%s usage in %d out %d total %d cached %d reasoning %d, want %d %d %d 0 0", what,
			count(usage.InputTokens), count(usage.OutputTokens), count(usage.TotalTokens),
			count(usage.CachedTokens), count(usage.ReasoningTokens), in, deltas, in+deltas)
	}
}
