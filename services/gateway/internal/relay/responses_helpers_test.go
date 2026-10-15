package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

// startResponsesGateway serves the Responses relay in front of channel, as
// startGateway serves the Messages one, and checks in the same way that every
// admitted request was reported exactly once.
func startResponsesGateway(t *testing.T, channel Channel) *gateway {
	t.Helper()
	return serveGateway(t, channel, "POST /v1/responses", NewResponses)
}

// postResponses sends a Responses request to the gateway with the gateway key as a
// bearer token, as Codex sends it.
func (g *gateway) postResponses(t *testing.T, ctx context.Context, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gatewayKeyValue)
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// serveResponsesDirect runs one Responses request through the relay without a
// network, with an admission that refuses every call with errBilling.
func serveResponsesDirect(t *testing.T, body string, header map[string]string) (*httptest.ResponseRecorder, *stubAdmission) {
	t.Helper()
	admission := newStubAdmission(Channel{BaseURL: closedPortURL(t)})
	admission.refuse = errBilling
	handler := NewResponses(Config{Getenv: testEnv}, admission, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	for name, value := range header {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, admission
}

// responsesBody returns a Responses request body that holds bodyCanary and sets
// max_output_tokens, so that the relay sends it upstream unchanged.
func responsesBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"max_output_tokens":256,`+
		`"input":[{"role":"user","content":"What is the weather in Paris? %s"}]}`, model, stream, bodyCanary)
}

// responseEvent returns an event with a response object, numbered seq.
func responseEvent(eventType string, seq int, response string) string {
	return event(fmt.Sprintf(`{"type":%q,"sequence_number":%d,"response":%s}`, eventType, seq, response))
}

func responseCreated(seq int) string {
	return responseEvent(openai.EventCreated, seq,
		`{"id":"resp_1","object":"response","status":"in_progress","output":[],"usage":null}`)
}

func outputTextDelta(seq int, text string) string {
	return event(fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,`+
		`"item_id":"msg_1","output_index":0,"content_index":0,"delta":%q}`, seq, text))
}

// responseEnd returns an event that ends the stream: response.completed,
// response.incomplete or response.failed, with the status of its type and usage.
func responseEnd(eventType string, seq int, usage string) string {
	status := strings.TrimPrefix(eventType, "response.")
	return responseEvent(eventType, seq,
		`{"id":"resp_1","object":"response","status":"`+status+`","output":[],"usage":`+usage+`}`)
}

// responsesErrorJSON returns the Responses error body the relay writes, with param
// null when it is "".
func responsesErrorJSON(errorType, message, param string) string {
	paramJSON := "null"
	if param != "" {
		paramJSON = fmt.Sprintf("%q", param)
	}
	return fmt.Sprintf(`{"error":{"message":%q,"type":%q,"param":%s,"code":null}}`, message, errorType, paramJSON)
}

// responsesGatewayErrorEvent is the event the gateway sends when a started Responses
// stream fails after the event numbered seq-1, as api/errors.md shows it.
func responsesGatewayErrorEvent(seq int) string {
	return fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"code\":\"api_error\",\"message\":%q,"+
		"\"param\":null,\"sequence_number\":%d}\n\n", upstreamFailedMessage, seq)
}

// checkResponsesError fails the test unless rec holds a Responses error of the status,
// type and param, with "" for a null param.
func checkResponsesError(t *testing.T, rec *httptest.ResponseRecorder, status int, errorType, param string) {
	t.Helper()
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	detail, wantParam := body["error"], any(nil)
	if param != "" {
		wantParam = param
	}
	if rec.Code != status || len(body) != 1 || detail["type"] != errorType || detail["param"] != wantParam {
		t.Errorf("got %d %s, want %d %s with param %v", rec.Code, rec.Body, status, errorType, wantParam)
	}
	if code, ok := detail["code"]; !ok || code != nil {
		t.Errorf("code = %v, %v; want null", code, ok)
	}
	if message, _ := detail["message"].(string); message == "" {
		t.Error("the error has no message")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if id := rec.Header().Get("request-id"); !requestIDPattern.MatchString(id) {
		t.Errorf("request-id = %q, want req_<uuid>", id)
	}
}
