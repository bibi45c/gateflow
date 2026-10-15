package relay

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// A recorded Ollama stream whose connection drops after its first events gets the
// gateway's error event numbered one past the last event relayed, as the recorded data
// numbers it.
func TestResponsesRecordedStreamCutShort(t *testing.T) {
	events := readEvents(t, sse.NewReader(bytes.NewReader(fixture(t, "ollama-tool-stream.sse"))))
	for _, relayed := range []int{1, 2, 4} {
		t.Run(events[relayed-1].Type(), func(t *testing.T) {
			var head []byte
			for _, e := range events[:relayed] {
				head = append(head, e.Raw...)
			}
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				startStream(w)
				sendEvents(w, string(head))
				panic(http.ErrAbortHandler)
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			// The recording numbers its events from 0.
			want := string(head) + responsesGatewayErrorEvent(relayed)
			if body := readAllBody(t, resp); string(body) != want {
				t.Errorf("body ends %q, want it to end %q", tail(body), tail([]byte(want)))
			}
			checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)
		})
	}
}

// tail returns the last bytes of a body, enough to show the event that ends it.
func tail(body []byte) string {
	return string(body[max(0, len(body)-200):])
}

// A failure before the first event gets the gateway's 502 in the Responses error body,
// and settles as interrupted without counts: the request was sent.
func TestResponsesFailureBeforeAnyEventGets502(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upstream http.HandlerFunc
	}{
		{"stream ends before any event", func(w http.ResponseWriter, r *http.Request) {
			startStream(w)
		}},
		{"connection drops before any event", func(w http.ResponseWriter, r *http.Request) {
			startStream(w)
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		}},
		{"first event over 1 MiB", func(w http.ResponseWriter, r *http.Request) {
			startStream(w)
			sendEvents(w, outputTextDelta(0, strings.Repeat("a", maxEventBytes)))
		}},
		{"connection closes without a reply", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := startResponsesGateway(t, startUpstream(t, tc.upstream))

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			body := readAllBody(t, resp)
			want := responsesErrorJSON(openai.ErrorTypeAPI, upstreamFailedMessage, "")
			if resp.StatusCode != http.StatusBadGateway || string(body) != want {
				t.Errorf("got %d %s, want 502 %s", resp.StatusCode, body, want)
			}
			checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)
		})
	}
}

// Report runs exactly once on /v1/responses too, even when the relay panics.
func TestResponsesReportedOnceWhenTheRelayPanics(t *testing.T) {
	admission := newStubAdmission(Channel{BaseURL: closedPortURL(t), CredentialEnv: credentialEnv})
	getenv := func(string) string { panic("getenv failed") }
	handler := NewResponses(Config{Getenv: getenv}, admission, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(responsesBody("mock-small", false)))
	req.Header.Set("Authorization", "Bearer "+gatewayKeyValue)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the server")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	checkResult(t, admission.result(t), OutcomeNotDispatched, count(0), count(0))
	if n := admission.reportCount(); n != 1 {
		t.Errorf("reported %d times, want 1", n)
	}
}
