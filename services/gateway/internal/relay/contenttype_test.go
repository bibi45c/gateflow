package relay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// The two endpoints of endpoints.
var messagesEndpoint, responsesEndpoint = endpoints[0], endpoints[1]

// post sends the endpoint's request body, with stream as given.
func (e endpoint) post(gw *gateway, t *testing.T, stream bool) *http.Response {
	t.Helper()
	return e.send(t, gw, context.Background(), e.body("mock-small", stream), nil)
}

// setContentType sets the reply's content type, or stops the server from adding one
// when it is "".
func setContentType(w http.ResponseWriter, contentType string) {
	if contentType == "" {
		w.Header()["Content-Type"] = nil
		return
	}
	w.Header().Set("Content-Type", contentType)
}

// firstEventLength returns the length of the first event of stream.
func firstEventLength(t *testing.T, stream []byte) int {
	t.Helper()
	e, err := sse.NewReader(bytes.NewReader(stream)).Next()
	if err != nil {
		t.Fatal(err)
	}
	return len(e.Raw)
}

// A stream request whose reply is an event stream under another content type, as
// Ollama 0.40.3 sends a Responses tool call as application/x-ndjson, is relayed as a
// stream: byte for byte, with the upstream's content type, each event flushed before
// the next is read, and settled once on the usage of its end event. The upstream sends
// its first event and blocks until the test opens the gate, so the client can only
// have that event if the gateway relayed it without reading the whole reply.
func TestStreamRequestIsRelayedAsAStreamWhateverTheContentType(t *testing.T) {
	messagesStream := []byte(messageStart(`{"input_tokens":30,"output_tokens":1}`) + textDelta("Hi") +
		messageDelta(`{"output_tokens":5}`) + messageStop())
	for _, tc := range []struct {
		endpoint      endpoint
		contentType   string
		stream        []byte
		input, output int64
	}{
		{responsesEndpoint, "application/x-ndjson", fixture(t, "ollama-tool-stream.sse"), 157, 19},
		{responsesEndpoint, "", fixture(t, "ollama-tool-stream.sse"), 157, 19},
		{messagesEndpoint, "application/x-ndjson", messagesStream, 30, 5},
	} {
		t.Run(tc.endpoint.route+" "+tc.contentType, func(t *testing.T) {
			first, rest := tc.stream[:firstEventLength(t, tc.stream)], tc.stream[firstEventLength(t, tc.stream):]
			blocked := newGate(t)
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				setContentType(w, tc.contentType)
				w.WriteHeader(http.StatusOK)
				sendEvents(w, string(first))
				select {
				case <-blocked.ch:
				case <-r.Context().Done():
					return
				}
				sendEvents(w, string(rest))
			})
			gw := tc.endpoint.start(t, channel)

			// A gateway that buffers the reply sends nothing while the upstream is
			// blocked, so the request is cancelled after 5 s and the test fails.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := time.AfterFunc(5*time.Second, cancel)
			resp := tc.endpoint.send(t, gw, ctx, tc.endpoint.body("mock-small", true), nil)
			head := make([]byte, len(first))
			_, err := io.ReadFull(resp.Body, head)
			if !timer.Stop() {
				t.Fatal("the first event did not arrive while the upstream was blocked")
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if tc.contentType != "" && resp.Header.Get("Content-Type") != tc.contentType {
				t.Errorf("content-type = %q, want %q", resp.Header.Get("Content-Type"), tc.contentType)
			}
			if !bytes.Equal(head, first) {
				t.Errorf("first event = %q, want %q", head, first)
			}

			blocked.open()
			if tail := readAllBody(t, resp); !bytes.Equal(tail, rest) {
				t.Errorf("relayed stream differs from the upstream's after the first event:\n%s", tail)
			}
			checkResult(t, gw.admission.result(t), OutcomeCompleted, count(tc.input), count(tc.output))
			gw.Close() // waits for the handler to report
			if n := gw.admission.reportCount(); n != 1 {
				t.Errorf("reported %d times, want 1", n)
			}
		})
	}
}

// A stream request whose reply is application/json is still read as one body: a JSON
// body passes back unchanged and completes the call on its usage.
func TestStreamRequestWithJSONReplyIsReadAsABody(t *testing.T) {
	for _, tc := range []struct {
		endpoint      endpoint
		reply         []byte
		input, output int64
	}{
		{messagesEndpoint, fixture(t, "message.json"), 12, 0},
		{responsesEndpoint, []byte(`{"id":"resp_1","object":"response","status":"completed",` +
			`"usage":{"input_tokens":9,"output_tokens":2}}`), 9, 2},
	} {
		t.Run(tc.endpoint.route, func(t *testing.T) {
			channel, _ := recordingUpstream(t, tc.reply)
			gw := tc.endpoint.start(t, channel)

			resp := tc.endpoint.post(gw, t, true)
			if body := readAllBody(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(body, tc.reply) {
				t.Errorf("got %d %q, want 200 and the upstream body", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			checkResult(t, gw.admission.result(t), OutcomeCompleted, count(tc.input), count(tc.output))
		})
	}
}

// A 2xx body read as one body that is not a JSON object holds no usage and no outcome:
// the client gets it with the upstream's status and content type, the call ends
// interrupted with unknown counts, and the failure is logged without the body.
func TestNonJSONBodyIsInterrupted(t *testing.T) {
	const canary = "not-json-canary-3e8a"
	sseText := "event: message_stop\ndata: {\"type\":\"message_stop\"} " + canary + "\n\n"
	for _, e := range endpoints {
		for _, tc := range []struct {
			name, contentType, body string
			status                  int
			stream                  bool
		}{
			{"plain text", "text/plain; charset=utf-8", "upstream says hello " + canary, http.StatusOK, false},
			{"event stream text as ndjson", "application/x-ndjson", sseText, http.StatusOK, false},
			{"event stream text as JSON", "application/json", sseText, http.StatusCreated, false},
			{"stream request, event stream text as JSON", "application/json", sseText, http.StatusOK, true},
			{"JSON array", "application/json", `[{"id":"` + canary + `"}]`, http.StatusOK, false},
		} {
			t.Run(e.route+" "+tc.name, func(t *testing.T) {
				channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				})
				gw := e.start(t, channel)

				resp := e.post(gw, t, tc.stream)
				if body := readAllBody(t, resp); resp.StatusCode != tc.status || string(body) != tc.body {
					t.Errorf("got %d %q, want %d and the upstream body", resp.StatusCode, body, tc.status)
				}
				if ct := resp.Header.Get("Content-Type"); ct != tc.contentType {
					t.Errorf("content-type = %q, want %q", ct, tc.contentType)
				}
				checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)

				gw.Close() // waits for the handler to report and log
				logs := gw.logs.String()
				if !strings.Contains(logs, "upstream reply is not JSON") {
					t.Errorf("the failure was not logged:\n%s", logs)
				}
				if strings.Contains(logs, canary) {
					t.Errorf("the logs hold the upstream body:\n%s", logs)
				}
			})
		}
	}
}

func TestReadsAsStream(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		stream      bool
		want        bool
	}{
		{"text/event-stream", false, true},
		{"text/event-stream; charset=utf-8", false, true},
		{"Text/Event-Stream", true, true},
		{"application/json", true, false},
		{"application/json; charset=utf-8", true, false},
		{"application/json", false, false},
		{"application/x-ndjson", true, true},
		{"application/x-ndjson", false, false},
		{"text/plain", true, true},
		{"", true, true},
		{"", false, false},
		{"not a media type;;", true, true},
	} {
		header := http.Header{}
		if tc.contentType != "" {
			header.Set("Content-Type", tc.contentType)
		}
		if got := readsAsStream(header, tc.stream); got != tc.want {
			t.Errorf("%q, stream %t: got %t, want %t", tc.contentType, tc.stream, got, tc.want)
		}
	}
}

func TestIsJSONObject(t *testing.T) {
	for body, want := range map[string]bool{
		`{"id":"resp_1"}`:        true,
		" \r\n\t{}\n":            true,
		`{"id":`:                 false,
		`[]`:                     false,
		`"text"`:                 false,
		`null`:                   false,
		``:                       false,
		`not json`:               false,
		"event: x\ndata: {}\n\n": false,
		`{} {}`:                  false,
	} {
		if got := isJSONObject([]byte(body)); got != want {
			t.Errorf("%q: got %t, want %t", body, got, want)
		}
	}
}
