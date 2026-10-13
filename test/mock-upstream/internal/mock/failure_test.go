package mock

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

func TestErrorStatusScenarios(t *testing.T) {
	srv := newServer(t, DefaultTiming)
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
			resp := post(t, srv, requestBody(t, tc.model, stream, nil, userMessages), nil)
			if resp.StatusCode != tc.status {
				t.Errorf("%s stream=%v: status %d, want %d", tc.model, stream, resp.StatusCode, tc.status)
			}
			if got := resp.Header.Get("retry-after"); got != tc.retryAfter {
				t.Errorf("%s: retry-after %q, want %q", tc.model, got, tc.retryAfter)
			}
			if body := readError(t, resp); body.Error.Type != tc.errorType {
				t.Errorf("%s: error type %q, want %q", tc.model, body.Error.Type, tc.errorType)
			}
		}
	}
}

func TestBadRequests(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	noMaxTokens, _ := json.Marshal(map[string]any{"model": "mock-text", "messages": userMessages})
	cases := []struct {
		name      string
		body      []byte
		status    int
		errorType string
	}{
		{"invalid JSON", []byte(`{"model":"mock-text",`), 400, "invalid_request_error"},
		{"not an object", []byte(`[1,2]`), 400, "invalid_request_error"},
		{"missing max_tokens", noMaxTokens, 400, "invalid_request_error"},
		{"mock-tool without tools", requestBody(t, "mock-tool", true, nil, userMessages), 400,
			"invalid_request_error"},
		{"mock-tool with empty tools", requestBody(t, "mock-tool", false, []any{}, userMessages), 400,
			"invalid_request_error"},
		{"unknown model", requestBody(t, "claude-opus", true, openCodeTools, userMessages), 404,
			"not_found_error"},
	}
	for _, tc := range cases {
		resp := post(t, srv, tc.body, nil)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
		if body := readError(t, resp); body.Error.Type != tc.errorType {
			t.Errorf("%s: error type %q, want %q", tc.name, body.Error.Type, tc.errorType)
		}
	}
}

func TestMockCodingAnswersWithoutToolsOnceTheScriptIsDone(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	results := []any{}
	for i := range 3 {
		results = append(results, map[string]any{"type": "tool_result",
			"tool_use_id": "toolu_mock_coding_" + strconv.Itoa(i), "content": "done"})
	}
	messages := append(slices.Clone(userMessages), map[string]any{"role": "user", "content": results})
	message := readMessage(t, post(t, srv, requestBody(t, "mock-coding", false, nil, messages), nil))
	if message.ID != "msg_mock_coding_3" || *message.StopReason != "end_turn" {
		t.Errorf("id %s stop %s", message.ID, *message.StopReason)
	}
}

func TestMockCodingAnswersARequestWithoutTools(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	for name, tools := range map[string][]any{"missing": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			body := requestBody(t, "mock-coding", true, tools, userMessages)
			resp := post(t, srv, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			events, err := readEvents(t, resp)
			if err != nil {
				t.Fatalf("stream ended with %v", err)
			}
			streamed := assemble(t, events)
			if streamed.messageID != "msg_mock_coding_0" || streamed.stopReason != "end_turn" ||
				len(streamed.blocks) != 1 || streamed.blocks[0].Type != "text" ||
				streamed.blocks[0].Text != codingNoToolsAnswer {
				t.Errorf("stream: id %s stop %s blocks %+v", streamed.messageID, streamed.stopReason,
					streamed.blocks)
			}
			if count(streamed.usageStart.InputTokens) != wantInputTokens(body) ||
				count(streamed.usageDelta.OutputTokens) != streamed.deltas {
				t.Errorf("stream usage in %d out %d, want in %d out %d",
					count(streamed.usageStart.InputTokens), count(streamed.usageDelta.OutputTokens),
					wantInputTokens(body), streamed.deltas)
			}

			plain := requestBody(t, "mock-coding", false, tools, userMessages)
			message := readMessage(t, post(t, srv, plain, nil))
			if message.ID != "msg_mock_coding_0" || message.StopReason == nil ||
				*message.StopReason != "end_turn" || len(message.Content) != 1 ||
				message.Content[0].Type != "text" || message.Content[0].Text != codingNoToolsAnswer {
				t.Errorf("message: id %s stop %v content %+v", message.ID, message.StopReason,
					message.Content)
			}
			if message.Usage.InputTokens != wantInputTokens(plain) ||
				message.Usage.OutputTokens != streamed.deltas {
				t.Errorf("message usage in %d out %d, want in %d out %d", message.Usage.InputTokens,
					message.Usage.OutputTokens, wantInputTokens(plain), streamed.deltas)
			}

			for _, request := range [][]byte{body, plain} {
				first, second := rawResponse(t, srv, request), rawResponse(t, srv, request)
				if !bytes.Equal(first, second) {
					t.Errorf("two runs differ:\n%s\n%s", first, second)
				}
			}
		})
	}
}

func TestDropClosesTheConnectionAfterKEvents(t *testing.T) {
	cases := []struct {
		name   string
		timing Timing
		header map[string]string
		want   int
	}{
		{"default", DefaultTiming, nil, 3},
		{"configured", Timing{FailAfterEvents: 5}, nil, 5},
		{"header", DefaultTiming, map[string]string{"X-Mock-Fail-After-Events": "1"}, 1},
		{"none", Timing{FailAfterEvents: 0}, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, tc.timing)
			resp := post(t, srv, requestBody(t, "mock-drop", true, nil, userMessages), tc.header)
			events, err := readEvents(t, resp)
			if err == nil || errors.Is(err, io.EOF) {
				t.Errorf("stream ended with %v, want a broken connection", err)
			}
			if len(events) != tc.want {
				t.Errorf("%d events %v, want %d", len(events), eventTypes(events), tc.want)
			}
			if slices.Contains(eventTypes(events), anthropic.EventMessageStop) {
				t.Error("stream sent message_stop")
			}
		})
	}
}

func TestDropNeverSendsMessageStop(t *testing.T) {
	srv := newServer(t, Timing{FailAfterEvents: 10000})
	events, err := readEvents(t, post(t, srv, requestBody(t, "mock-drop", true, nil, userMessages), nil))
	if err == nil {
		t.Error("stream ended cleanly")
	}
	types := eventTypes(events)
	if len(types) < 10 || types[len(types)-1] != anthropic.EventMessageDelta {
		t.Errorf("events %v, want all but message_stop", types)
	}
}

func TestDropWithoutStreamClosesTheConnection(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	body := requestBody(t, "mock-drop", false, nil, userMessages)
	resp, err := srv.Client().Post(srv.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got status %d, want no response", resp.StatusCode)
	}
}

func TestErrorEventAfterKEvents(t *testing.T) {
	for _, k := range []int{0, 3, 7} {
		srv := newServer(t, Timing{FailAfterEvents: k})
		resp := post(t, srv, requestBody(t, "mock-error-event", true, nil, userMessages), nil)
		events, err := readEvents(t, resp)
		if err != nil {
			t.Fatalf("K=%d: stream ended with %v, want a clean end", k, err)
		}
		types := eventTypes(events)
		if len(events) != k+1 || types[k] != anthropic.EventError {
			t.Fatalf("K=%d: events %v, want %d then error", k, types, k)
		}
		if slices.Contains(types, anthropic.EventMessageStop) {
			t.Errorf("K=%d: stream sent message_stop", k)
		}
		if k > 0 && types[0] != anthropic.EventMessageStart {
			t.Errorf("K=%d: first event %s", k, types[0])
		}
		var body anthropic.ErrorBody
		if err := json.Unmarshal(events[k].Data, &body); err != nil {
			t.Fatal(err)
		}
		if body.Type != "error" || body.Error.Type != "overloaded_error" || body.Error.Message == "" {
			t.Errorf("K=%d: error event %s", k, events[k].Data)
		}
	}
}

func TestErrorEventWithoutStreamIsOverloaded(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	resp := post(t, srv, requestBody(t, "mock-error-event", false, nil, userMessages), nil)
	if resp.StatusCode != 529 {
		t.Errorf("status %d, want 529", resp.StatusCode)
	}
	if body := readError(t, resp); body.Error.Type != "overloaded_error" {
		t.Errorf("error type %q", body.Error.Type)
	}
}
