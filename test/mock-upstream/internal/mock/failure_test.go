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
		{"mock-coding without tools", requestBody(t, "mock-coding", false, nil, userMessages), 400,
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
