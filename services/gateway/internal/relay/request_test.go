package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// errBilling is a refusal from the stand-in admission, which the relay writes as is.
var errBilling = &PublicError{
	Status:  http.StatusPaymentRequired,
	Type:    anthropic.ErrorTypeBilling,
	Message: "The wallet balance is too low.",
}

// serveDirect runs one request through the relay without a network, with an
// admission that refuses every call with errBilling.
func serveDirect(t *testing.T, req *http.Request) (*httptest.ResponseRecorder, *stubAdmission) {
	t.Helper()
	admission := newStubAdmission(Channel{BaseURL: closedPortURL(t)})
	admission.refuse = errBilling
	handler := New(Config{Getenv: testEnv}, admission, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, admission
}

func newRequest(body string, header map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	for name, value := range header {
		req.Header.Set(name, value)
	}
	return req
}

// checkError fails the test unless rec holds an Anthropic error of the status and type.
func checkError(t *testing.T, rec *httptest.ResponseRecorder, status int, errorType string) {
	t.Helper()
	var body anthropic.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if rec.Code != status || body.Type != "error" || body.Error.Type != errorType {
		t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body, status, errorType)
	}
	if body.Error.Message == "" {
		t.Error("the error has no message")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if id := rec.Header().Get("request-id"); !requestIDPattern.MatchString(id) {
		t.Errorf("request-id = %q, want req_<uuid>", id)
	}
}

// padded returns a valid request body of exactly size bytes.
func padded(size int) string {
	body := `{"model":"mock-small","max_tokens":256}`
	return body[:len(body)-1] + strings.Repeat(" ", size-len(body)) + "}"
}

func TestRequestChecks(t *testing.T) {
	key := map[string]string{"x-api-key": gatewayKeyValue}
	tooLarge := padded(maxBodyBytes + 1)
	tests := []struct {
		name    string
		body    string
		header  map[string]string
		chunked bool
		status  int
		errType string
	}{
		{"body over 2 MiB", tooLarge, key, false, 413, anthropic.ErrorTypeRequestTooLarge},
		{"body over 2 MiB, sent chunked", tooLarge, key, true, 413, anthropic.ErrorTypeRequestTooLarge},
		{"body over 2 MiB without a key", tooLarge, nil, false, 413, anthropic.ErrorTypeRequestTooLarge},
		{"no key", `{"model":"m","max_tokens":1}`, nil, false, 401, anthropic.ErrorTypeAuthentication},
		{"no key and malformed JSON", `{"model":`, nil, false, 401, anthropic.ErrorTypeAuthentication},
		{"empty key and basic auth", `{"model":"m","max_tokens":1}`,
			map[string]string{"x-api-key": " ", "Authorization": "Basic Zm9vOmJhcg=="}, false, 401,
			anthropic.ErrorTypeAuthentication},
		{"malformed JSON", `{"model":`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"not an object", `[]`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"model missing", `{"max_tokens":1}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"model null", `{"model":null,"max_tokens":1}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"model not a string", `{"model":7,"max_tokens":1}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"model empty", `{"model":"","max_tokens":1}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"max_tokens missing", `{"model":"m"}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"max_tokens 0", `{"model":"m","max_tokens":0}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"max_tokens negative", `{"model":"m","max_tokens":-1}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"max_tokens 32769", `{"model":"m","max_tokens":32769}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"max_tokens not an integer", `{"model":"m","max_tokens":1.5}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"max_tokens a string", `{"model":"m","max_tokens":"10"}`, key, false, 400, anthropic.ErrorTypeInvalidRequest},
		{"stream not a boolean", `{"model":"m","max_tokens":1,"stream":"yes"}`, key, false, 400,
			anthropic.ErrorTypeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(tt.body, tt.header)
			if tt.chunked {
				req.ContentLength = -1
			}
			rec, admission := serveDirect(t, req)
			checkError(t, rec, tt.status, tt.errType)
			if calls := admission.admitted(); len(calls) != 0 {
				t.Errorf("a request that failed the checks reached Admit")
			}
		})
	}
}

// Requests at the bounds pass the checks, reach Admit with what it needs, and get
// Admit's refusal written as is.
func TestChecksPassAtTheBounds(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		maxTokens int64
	}{
		{"body of exactly 2 MiB", padded(maxBodyBytes), 256},
		{"max_tokens 1", `{"model":"mock-small","max_tokens":1}`, 1},
		{"max_tokens 32768", `{"model":"mock-small","max_tokens":32768}`, 32768},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, admission := serveDirect(t, newRequest(tt.body, map[string]string{"x-api-key": gatewayKeyValue}))
			want := `{"type":"error","error":{"type":"billing_error","message":"The wallet balance is too low."}}`
			if rec.Code != http.StatusPaymentRequired || rec.Body.String() != want {
				t.Errorf("got %d %s, want 402 %s", rec.Code, rec.Body, want)
			}
			calls := admission.admitted()
			if len(calls) != 1 {
				t.Fatalf("Admit ran %d times, want 1", len(calls))
			}
			wantCall := Call{
				RequestID:  rec.Header().Get("request-id"),
				GatewayKey: gatewayKeyValue,
				Model:      "mock-small",
				MaxTokens:  tt.maxTokens,
				BodyBytes:  int64(len(tt.body)),
			}
			if calls[0] != wantCall || !requestIDPattern.MatchString(calls[0].RequestID) {
				t.Errorf("Admit got %+v, want %+v", calls[0], wantCall)
			}
		})
	}
}

func TestGatewayKeySources(t *testing.T) {
	tests := []struct {
		name   string
		header map[string]string
	}{
		{"x-api-key", map[string]string{"x-api-key": gatewayKeyValue}},
		{"bearer token", map[string]string{"Authorization": "Bearer " + gatewayKeyValue}},
		{"lower-case scheme", map[string]string{"Authorization": "bearer " + gatewayKeyValue}},
		{"x-api-key first", map[string]string{"x-api-key": gatewayKeyValue, "Authorization": "Bearer other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, admission := serveDirect(t, newRequest(`{"model":"m","max_tokens":1}`, tt.header))
			calls := admission.admitted()
			if len(calls) != 1 || calls[0].GatewayKey != gatewayKeyValue {
				t.Errorf("Admit got %+v, want the key %q", calls, gatewayKeyValue)
			}
		})
	}
}

func TestRequestIDsDiffer(t *testing.T) {
	first, _ := serveDirect(t, newRequest(`{}`, nil))
	second, _ := serveDirect(t, newRequest(`{}`, nil))
	if first.Header().Get("request-id") == second.Header().Get("request-id") {
		t.Error("two requests got the same request id")
	}
}

// accessLine is one access log line.
type accessLine struct {
	Msg           string `json:"msg"`
	Method        string `json:"method"`
	Route         string `json:"route"`
	Status        int    `json:"status"`
	DurationMS    *int64 `json:"duration_ms"`
	Model         string `json:"model"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	RequestID     string `json:"request_id"`
}

// accessLines returns the access log lines by request id.
func accessLines(t *testing.T, logs string) map[string]accessLine {
	t.Helper()
	lines := make(map[string]accessLine)
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var line accessLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if line.Msg == "request" {
			if _, seen := lines[line.RequestID]; seen {
				t.Errorf("request %s has two access lines", line.RequestID)
			}
			lines[line.RequestID] = line
		}
	}
	return lines
}

// The access log has one line per request, and never the body, the gateway key, the
// upstream credential or an upstream error body.
func TestAccessLogHoldsNoSecrets(t *testing.T) {
	const upstreamCanary = "upstream-body-canary-3c7d"
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := anthropic.ParseRequest(body)
		switch req.Model {
		case "case-400":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"`+upstreamCanary+`"}}`)
		case "case-500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, upstreamCanary)
		default:
			startStream(w)
			sendEvents(w, messageStart(`{"input_tokens":5}`), textDelta(upstreamCanary), messageStop())
		}
	})
	channel.CredentialEnv = credentialEnv
	gw := startGateway(t, channel)

	bearer := http.Header{"X-Api-Key": {""}, "Authorization": {"Bearer " + gatewayKeyValue}}
	requests := []struct {
		body   string
		header http.Header
		status int
	}{
		{messagesBody("mock-small", true), nil, http.StatusOK},
		{messagesBody("case-400", false), bearer, http.StatusBadRequest},
		{messagesBody("case-500", false), nil, http.StatusBadGateway},
		{`{"model":"bad","max_tokens":0,"note":"` + bodyCanary + `"}`, nil, http.StatusBadRequest},
	}
	var ids []string
	for _, r := range requests {
		resp := gw.post(t, context.Background(), r.body, r.header)
		readAllBody(t, resp)
		if resp.StatusCode != r.status {
			t.Errorf("status = %d, want %d", resp.StatusCode, r.status)
		}
		ids = append(ids, resp.Header.Get("request-id"))
	}
	gw.Close() // waits for every handler to log

	logs := gw.logs.String()
	for _, secret := range []string{bodyCanary, gatewayKeyValue, upstreamCredential, upstreamCanary} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs hold %q:\n%s", secret, logs)
		}
	}
	lines := accessLines(t, logs)
	if len(lines) != len(requests) {
		t.Fatalf("got %d access lines, want %d:\n%s", len(lines), len(requests), logs)
	}
	for i, r := range requests {
		line := lines[ids[i]]
		if line.Method != http.MethodPost || line.Route != "POST /v1/messages" ||
			line.Status != r.status || line.DurationMS == nil ||
			line.RequestBytes != int64(len(r.body)) || line.ResponseBytes == 0 {
			t.Errorf("access line of request %d = %+v", i, line)
		}
	}
	if model := lines[ids[0]].Model; model != "mock-small" {
		t.Errorf("model = %q, want mock-small", model)
	}
}
