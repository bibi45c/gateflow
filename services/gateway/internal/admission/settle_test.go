package admission_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// Metering gets the usage the upstream reported and the outcome of the case; for
// upstream_error and not_dispatched both counts are 0.
func TestSettleReportsTheOutcomeAndUsage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		upstream    answer
		unreachable bool // the channel's address refuses connections
		status      int
		settle      string
	}{
		{
			name:     "completed stream with cache counts",
			upstream: completedStream,
			status:   http.StatusOK,
			settle:   `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`,
		},
		{
			name: "completed body",
			upstream: raw(http.StatusOK, `{"id":"msg_1","type":"message","role":"assistant","content":[],`+
				`"model":"mock-small","usage":{"input_tokens":20,"output_tokens":4}}`),
			status: http.StatusOK,
			settle: `{"input_tokens":20,"output_tokens":4,"outcome":"completed"}`,
		},
		{
			name: "upstream error",
			upstream: raw(http.StatusInternalServerError,
				`{"type":"error","error":{"type":"api_error","message":"boom"}}`),
			status: http.StatusBadGateway,
			settle: `{"input_tokens":0,"output_tokens":0,"outcome":"upstream_error"}`,
		},
		{
			name:        "not dispatched",
			unreachable: true,
			status:      http.StatusBadGateway,
			settle:      `{"input_tokens":0,"output_tokens":0,"outcome":"not_dispatched"}`,
		},
		{
			name:     "interrupted stream",
			upstream: streamReply(messageStart(`{"input_tokens":20,"output_tokens":1}`), textDelta),
			status:   http.StatusOK,
			settle:   `{"input_tokens":20,"output_tokens":null,"outcome":"interrupted"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setup{upstream: tc.upstream}
			if tc.unreachable {
				s.verify = verifyOK(channelJSON{ID: "mock", Protocol: "anthropic-messages", BaseURL: closedPortURL(t)})
			}
			h := start(t, s)

			resp := h.post(context.Background(), nil)
			readBody(t, resp)
			h.finish()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			settle := h.settle.requests()
			if len(settle) != 1 {
				t.Fatalf("%d settle calls, want 1", len(settle))
			}
			checkInternalCall(t, settle[0], meteringToken, resp.Header.Get("request-id"))
			checkJSON(t, "settle body", settle[0].Body, tc.settle)
		})
	}
}

// The unreachable channel stays unreachable however many servers start after its
// URL is taken, the harness's own upstream among them (#109).
func TestClosedPortURLIsNeverReused(t *testing.T) {
	url := closedPortURL(t)
	for range 8 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("a test server answered the unreachable URL")
		}))
		t.Cleanup(server.Close)
	}
	h := start(t, setup{verify: verifyOK(channelJSON{ID: "mock", Protocol: "anthropic-messages", BaseURL: url})})

	if resp, err := http.Post(url+"/v1/messages", "application/json", strings.NewReader(messagesBody)); err == nil {
		resp.Body.Close()
		t.Fatalf("POST %s got %d, want a dial error", url, resp.StatusCode)
	}
	resp := h.post(context.Background(), nil)
	readBody(t, resp)
	h.finish()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if got := h.upstream.requests(); len(got) != 0 {
		t.Errorf("the harness upstream got %d requests, want 0", len(got))
	}
	settle := h.settle.requests()
	if len(settle) != 1 {
		t.Fatalf("%d settle calls, want 1", len(settle))
	}
	checkJSON(t, "settle body", settle[0].Body, `{"input_tokens":0,"output_tokens":0,"outcome":"not_dispatched"}`)
}

// A client that leaves mid-stream is still charged: settle does not use the
// request's context, which has ended.
func TestClientDisconnectStillSettles(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request, n int) {
		streamReply(messageStart(`{"input_tokens":20,"output_tokens":1}`))(w, r, n)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	h := start(t, setup{upstream: upstream})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := h.post(ctx, nil)
	if _, err := anthropic.NewReader(resp.Body).Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	h.finish()

	settle := h.settle.requests()
	if len(settle) != 1 {
		t.Fatalf("%d settle calls, want 1", len(settle))
	}
	checkJSON(t, "settle body", settle[0].Body, `{"input_tokens":20,"output_tokens":null,"outcome":"client_cancelled"}`)
	if lines := h.errorLines(); len(lines) > 0 {
		t.Errorf("error logs: %v", lines)
	}
}

// A settle that times out, answers 5xx or loses its connection is sent again
// unchanged.
func TestSettleIsRetried(t *testing.T) {
	for name, first := range map[string]answer{
		"timeout":            hang,
		"503":                internalError(503, "dependency_unavailable"),
		"500":                internalError(500, "internal_error"),
		"connection dropped": dropConnection,
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, setup{settle: failFirst(first, settleOK)})

			resp := h.post(context.Background(), nil)
			readBody(t, resp)
			h.finish()
			settle := h.settle.requests()
			if len(settle) != 2 {
				t.Fatalf("%d settle calls, want 2", len(settle))
			}
			if !bytes.Equal(settle[0].Body, settle[1].Body) || settle[0].Path != settle[1].Path {
				t.Errorf("the retry sent %s %s after %s %s", settle[1].Path, settle[1].Body, settle[0].Path, settle[0].Body)
			}
			checkInternalCall(t, settle[1], meteringToken, resp.Header.Get("request-id"))
			if lines := h.errorLines(); len(lines) > 0 {
				t.Errorf("error logs: %v", lines)
			}
		})
	}
}

// A settle that finally fails is logged at error level with the call and
// reservation ids; the client's answer is unchanged. A 4xx is not retried.
func TestSettleThatFinallyFailsIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settle   answer
		attempts int
	}{
		{"three 5xx", internalError(500, "internal_error"), 3},
		{"three timeouts", hang, 3},
		{"reservation not found", internalError(404, "reservation_not_found"), 1},
		{"idempotency conflict", internalError(422, "idempotency_conflict"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, setup{settle: tc.settle})

			resp := h.post(context.Background(), nil)
			body, _ := io.ReadAll(resp.Body)
			h.finish()
			if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("message_stop")) {
				t.Errorf("status %d, body %s; want the completed stream", resp.StatusCode, body)
			}
			if n := len(h.settle.requests()); n != tc.attempts {
				t.Errorf("%d settle calls, want %d", n, tc.attempts)
			}
			lines := h.errorLines()
			if len(lines) != 1 {
				t.Fatalf("error logs = %v, want one", lines)
			}
			if lines[0]["call_id"] != resp.Header.Get("request-id") || lines[0]["reservation_id"] != "rsv_f1" {
				t.Errorf("error log %v lacks the call or reservation id", lines[0])
			}
		})
	}
}
