package relay

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// streamUsage observes a stream's events as the relay does and returns the counts.
func streamUsage(t *testing.T, stream string) (input, output *int64) {
	t.Helper()
	var u usage
	for _, e := range readEvents(t, anthropic.NewReader(strings.NewReader(stream))) {
		u.observe(e.Type(), e.Data)
	}
	return u.counts()
}

func TestStreamUsage(t *testing.T) {
	tests := []struct {
		name          string
		stream        string
		input, output *int64
	}{
		{"input with both cache counts",
			messageStart(`{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":1}`) +
				messageDelta(`{"output_tokens":40}`),
			count(60), count(40)},
		{"cache counts only",
			messageStart(`{"cache_read_input_tokens":40}`) + messageDelta(`{"output_tokens":2}`),
			count(40), count(2)},
		{"input 0 and no message_delta",
			messageStart(`{"input_tokens":0,"output_tokens":1}`) + messageStop(),
			nil, nil},
		{"a later message_delta with input above 0 replaces it",
			messageStart(`{"input_tokens":0}`) + messageDelta(`{"input_tokens":25,"output_tokens":7}`),
			count(25), count(7)},
		{"a message_delta with input 0 does not replace it",
			messageStart(`{"input_tokens":30}`) + messageDelta(`{"input_tokens":0,"output_tokens":5}`),
			count(30), count(5)},
		{"a reported output of 0 is known",
			messageStart(`{"input_tokens":30}`) + messageDelta(`{"output_tokens":0}`),
			count(30), count(0)},
		{"output from the last message_delta",
			messageStart(`{"input_tokens":30}`) + messageDelta(`{"output_tokens":5}`) +
				messageDelta(`{"output_tokens":9}`),
			count(30), count(9)},
		{"output_tokens of message_start is not output",
			messageStart(`{"input_tokens":30,"output_tokens":1}`),
			count(30), nil},
		{"no usage in message_start",
			event(`{"type":"message_start","message":{"id":"msg_1"}}`) + messageDelta(`{"output_tokens":4}`),
			nil, count(4)},
		{"a negative count is unknown",
			messageStart(`{"input_tokens":-5}`) + messageDelta(`{"output_tokens":-1}`),
			nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, output := streamUsage(t, tt.stream)
			if !sameCount(input, tt.input) || !sameCount(output, tt.output) {
				t.Errorf("counts = %s, %s; want %s, %s",
					showCount(input), showCount(output), showCount(tt.input), showCount(tt.output))
			}
		})
	}
}

func TestBodyUsage(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		input, output *int64
	}{
		{"recorded message", string(fixture(t, "message.json")), count(12), count(0)},
		{"input with both cache counts",
			`{"usage":{"input_tokens":5,"cache_read_input_tokens":6,"cache_creation_input_tokens":7,"output_tokens":8}}`,
			count(18), count(8)},
		{"input 0", `{"usage":{"input_tokens":0,"output_tokens":3}}`, nil, count(3)},
		{"no usage", `{"id":"msg_1"}`, nil, nil},
		{"not JSON", `not json`, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var u usage
			u.observeBody([]byte(tt.body))
			input, output := u.counts()
			if !sameCount(input, tt.input) || !sameCount(output, tt.output) {
				t.Errorf("counts = %s, %s; want %s, %s",
					showCount(input), showCount(output), showCount(tt.input), showCount(tt.output))
			}
		})
	}
}

// The relay reports what the upstream reported, for streaming and non-streaming
// replies alike.
func TestReportedUsage(t *testing.T) {
	tests := []struct {
		name          string
		contentType   string
		reply         string
		input, output *int64
	}{
		{"stream with cache counts", "text/event-stream",
			messageStart(`{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}`) +
				messageDelta(`{"output_tokens":40}`) + messageStop(),
			count(60), count(40)},
		{"stream with input 0 and no message_delta", "text/event-stream",
			messageStart(`{"input_tokens":0}`) + messageStop(),
			nil, nil},
		{"body with cache counts", "application/json",
			`{"type":"message","usage":{"input_tokens":5,"cache_read_input_tokens":6,"cache_creation_input_tokens":7,"output_tokens":8}}`,
			count(18), count(8)},
		{"body without usage", "application/json", `{"type":"message"}`, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = io.WriteString(w, tt.reply)
			})
			gw := startGateway(t, channel)

			resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
			if body := readAllBody(t, resp); string(body) != tt.reply {
				t.Errorf("body = %q, want the upstream's", body)
			}
			checkResult(t, gw.admission.result(t), OutcomeCompleted, tt.input, tt.output)
		})
	}
}
