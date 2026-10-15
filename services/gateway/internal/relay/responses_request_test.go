package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

// paddedResponses returns a valid Responses request body of exactly size bytes.
func paddedResponses(size int) string {
	body := `{"model":"mock-small","max_output_tokens":256}`
	return body[:len(body)-1] + strings.Repeat(" ", size-len(body)) + "}"
}

// A request that fails a check gets its error in the Responses body and never reaches
// Admit, so nothing is held for it.
func TestResponsesRequestChecks(t *testing.T) {
	key := map[string]string{"Authorization": "Bearer " + gatewayKeyValue}
	invalid := openai.ErrorTypeInvalidRequest
	tests := []struct {
		name    string
		body    string
		header  map[string]string
		status  int
		errType string
		param   string
	}{
		{"body over 2 MiB", paddedResponses(maxBodyBytes + 1), key, 413, openai.ErrorTypeRequestTooLarge, ""},
		{"no key", `{"model":"m"}`, nil, 401, openai.ErrorTypeAuthentication, ""},
		{"no key and previous_response_id", `{"model":"m","previous_response_id":"resp_1"}`, nil, 401,
			openai.ErrorTypeAuthentication, ""},
		{"malformed JSON", `{"model":`, key, 400, invalid, ""},
		{"not an object", `[]`, key, 400, invalid, ""},
		{"model missing", `{"input":"hi"}`, key, 400, invalid, ""},
		{"model null", `{"model":null}`, key, 400, invalid, ""},
		{"model empty", `{"model":""}`, key, 400, invalid, ""},
		{"model not a string", `{"model":7}`, key, 400, invalid, ""},
		{"max_output_tokens 0", `{"model":"m","max_output_tokens":0}`, key, 400, invalid, ""},
		{"max_output_tokens negative", `{"model":"m","max_output_tokens":-1}`, key, 400, invalid, ""},
		{"max_output_tokens 32769", `{"model":"m","max_output_tokens":32769}`, key, 400, invalid, ""},
		{"max_output_tokens not an integer", `{"model":"m","max_output_tokens":1.5}`, key, 400, invalid, ""},
		{"max_output_tokens a string", `{"model":"m","max_output_tokens":"10"}`, key, 400, invalid, ""},
		{"max_output_tokens in another case", `{"model":"m","MAX_OUTPUT_TOKENS":120}`, key, 400, invalid, ""},
		{"max_output_tokens again in another case", `{"model":"m","max_output_tokens":16,"Max_Output_Tokens":120}`,
			key, 400, invalid, ""},
		{"max_output_tokens twice", `{"model":"m","max_output_tokens":null,"max_output_tokens":99999}`, key, 400,
			invalid, ""},
		{"stream not a boolean", `{"model":"m","stream":"yes"}`, key, 400, invalid, ""},
		{"tools not an array", `{"model":"m","tools":{}}`, key, 400, invalid, ""},
		{"previous_response_id", `{"model":"m","previous_response_id":"resp_1"}`, key, 400, invalid,
			"previous_response_id"},
		{"empty previous_response_id", `{"model":"m","previous_response_id":""}`, key, 400, invalid,
			"previous_response_id"},
		{"previous_response_id with max_output_tokens", `{"model":"m","max_output_tokens":100,"previous_response_id":"resp_1"}`,
			key, 400, invalid, "previous_response_id"},
		{"previous_response_id not a string", `{"model":"m","previous_response_id":5}`, key, 400, invalid, ""},
		{"previous_response_id in another case", `{"model":"m","Previous_Response_ID":"resp_1"}`, key, 400, invalid, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, admission := serveResponsesDirect(t, tt.body, tt.header)
			checkResponsesError(t, rec, tt.status, tt.errType, tt.param)
			if calls := admission.admitted(); len(calls) != 0 {
				t.Errorf("a request that failed the checks reached Admit")
			}
		})
	}
}

// Requests at the bounds pass the checks and reach Admit with what it needs, and
// Admit's refusal is written in the Responses body. A max_output_tokens that is absent
// or null is held at 32768.
func TestResponsesChecksPassAtTheBounds(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		maxTokens int64
	}{
		{"body of exactly 2 MiB", paddedResponses(maxBodyBytes), 256},
		{"max_output_tokens 1", `{"model":"mock-small","max_output_tokens":1}`, 1},
		{"max_output_tokens 32768", `{"model":"mock-small","max_output_tokens":32768}`, 32768},
		{"max_output_tokens absent", `{"model":"mock-small"}`, 32768},
		{"max_output_tokens null", `{"model":"mock-small","max_output_tokens":null}`, 32768},
		{"previous_response_id null", `{"model":"mock-small","previous_response_id":null}`, 32768},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, admission := serveResponsesDirect(t, tt.body, map[string]string{"x-api-key": gatewayKeyValue})
			want := `{"error":{"message":"The wallet balance is too low.","type":"billing_error","param":null,"code":null}}`
			if rec.Code != http.StatusPaymentRequired || rec.Body.String() != want {
				t.Errorf("got %d %s, want 402 %s", rec.Code, rec.Body, want)
			}
			calls := admission.admitted()
			if len(calls) != 1 {
				t.Fatalf("Admit ran %d times, want 1", len(calls))
			}
			wantCall := Call{
				RequestID:  rec.Header().Get("request-id"),
				Protocol:   ProtocolResponses,
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

// A request without max_output_tokens, or with null, goes upstream with
// max_output_tokens 32768 and every other byte as sent; one with a value goes
// upstream unchanged.
func TestResponsesSendsTheOutputBoundUpstream(t *testing.T) {
	tests := []struct{ name, body, upstream string }{
		{"absent", `{"model":"mock-small","stream":false,"input":"Hi"}`,
			`{"model":"mock-small","stream":false,"input":"Hi","max_output_tokens":32768}`},
		{"null", "{\n  \"max_output_tokens\": null,\n  \"model\": \"mock-small\"\n}",
			"{\n  \"max_output_tokens\": 32768,\n  \"model\": \"mock-small\"\n}"},
		{"absent, with a nested max_output_tokens", `{"model":"mock-small","text":{"max_output_tokens":5}}`,
			`{"model":"mock-small","text":{"max_output_tokens":5},"max_output_tokens":32768}`},
		{"a value", responsesBody("mock-small", false), responsesBody("mock-small", false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply := []byte(`{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":9,"output_tokens":2}}`)
			channel, received := recordingUpstream(t, reply)
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), tt.body, nil)
			if body := readAllBody(t, resp); resp.StatusCode != http.StatusOK || !bytes.Equal(body, reply) {
				t.Errorf("got %d %s, want 200 and the upstream body", resp.StatusCode, body)
			}
			if got := received().body; string(got) != tt.upstream {
				t.Errorf("upstream body = %s, want %s", got, tt.upstream)
			}
			if calls := gw.admission.admitted(); len(calls) != 1 || calls[0].BodyBytes != int64(len(tt.body)) {
				t.Errorf("Admit got %+v, want the client's body size %d", calls, len(tt.body))
			}
			checkResult(t, gw.admission.result(t), OutcomeCompleted, count(9), count(2))
		})
	}
}

// The upstream gets only content-type of the client's headers, and the channel
// credential as a bearer token, at /v1/responses. The client's key, in either header,
// and its other headers stay behind.
func TestResponsesForwardsOnlyContentTypeAndTheCredential(t *testing.T) {
	channel, received := recordingUpstream(t, []byte(`{"id":"resp_1","status":"completed"}`))
	channel.CredentialEnv = credentialEnv
	gw := startResponsesGateway(t, channel)

	body := responsesBody("mock-small", false)
	resp := gw.postResponses(t, context.Background(), body, http.Header{
		"X-Api-Key":           {gatewayKeyValue},
		"Anthropic-Version":   {"2023-06-01"},
		"Anthropic-Beta":      {"tools-2024-04-04"},
		"Openai-Beta":         {"responses=v1"},
		"Openai-Organization": {"org-canary"},
		"Cookie":              {"session=secret"},
		"X-Request-Id":        {"req_from_client"},
		"X-Forwarded-For":     {"203.0.113.7"},
	})
	readAllBody(t, resp)
	got := received()

	if got.method != http.MethodPost || got.path != "/v1/responses" {
		t.Errorf("upstream got %s %s, want POST /v1/responses", got.method, got.path)
	}
	if string(got.body) != body {
		t.Errorf("upstream body = %q, want the client's bytes", got.body)
	}
	if auth := got.header.Values("Authorization"); !slices.Equal(auth, []string{"Bearer " + upstreamCredential}) {
		t.Errorf("authorization = %q, want the channel credential only", auth)
	}
	if ct := got.header.Values("Content-Type"); !slices.Equal(ct, []string{"application/json"}) {
		t.Errorf("content-type = %q", ct)
	}
	// The transport adds User-Agent and Content-Length itself.
	allowed := []string{"Content-Type", "Authorization", "User-Agent", "Content-Length"}
	for name, values := range got.header {
		if !slices.Contains(allowed, name) {
			t.Errorf("header %s was forwarded", name)
		}
		if strings.Contains(strings.Join(values, " "), gatewayKeyValue) {
			t.Errorf("the gateway key went upstream in %s", name)
		}
	}
}

func TestResponsesSendsNoCredentialWithoutCredentialEnv(t *testing.T) {
	channel, received := recordingUpstream(t, []byte(`{"id":"resp_1","status":"completed"}`))
	gw := startResponsesGateway(t, channel)

	resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", false), nil)
	readAllBody(t, resp)
	if got := received(); got.header.Get("Authorization") != "" || got.header.Get("X-Api-Key") != "" {
		t.Errorf("upstream got auth headers: %v", got.header)
	}
}

// A non-streaming reply read in full completes the call unless its status is failed;
// either way the client gets it unchanged and it is charged on its usage.
func TestResponsesNonStreamingReply(t *testing.T) {
	tests := []struct {
		name, body    string
		outcome       string
		input, output *int64
	}{
		{"recorded on Ollama, cached tokens not added", string(fixture(t, "ollama-text.response.json")),
			OutcomeCompleted, count(11), count(12)},
		{"incomplete", `{"id":"resp_1","object":"response","status":"incomplete","usage":{"input_tokens":40,"output_tokens":256}}`,
			OutcomeCompleted, count(40), count(256)},
		{"failed with usage", `{"id":"resp_1","object":"response","status":"failed","usage":{"input_tokens":40,"output_tokens":3}}`,
			OutcomeInterrupted, count(40), count(3)},
		{"failed without usage", `{"id":"resp_1","object":"response","status":"failed","usage":null}`,
			OutcomeInterrupted, nil, nil},
		{"without usage", `{"id":"resp_1","object":"response","status":"completed"}`, OutcomeCompleted, nil, nil},
		{"not JSON", `not json`, OutcomeCompleted, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", false), nil)
			if body := readAllBody(t, resp); resp.StatusCode != http.StatusOK || string(body) != tt.body {
				t.Errorf("got %d %q, want 200 and the upstream body", resp.StatusCode, body)
			}
			checkResult(t, gw.admission.result(t), tt.outcome, tt.input, tt.output)
		})
	}
}

// 400 and 429 pass through with the upstream's status, body and retry-after; any
// other status gets the gateway's 502 in the Responses body. None is charged.
func TestResponsesUpstreamStatuses(t *testing.T) {
	const canary = "upstream-error-canary-0b7e"
	for _, status := range []int{400, 429, 401, 403, 404, 307, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			reply := fmt.Sprintf(`{"error":{"message":"%s","type":"rate_limit_error","param":null,"code":null}}`, canary)
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Retry-After", "17")
				w.Header().Set("Location", "http://127.0.0.1:9/v1/responses")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, reply)
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			body := readAllBody(t, resp)
			wantStatus, wantBody := http.StatusBadGateway, responsesErrorJSON(openai.ErrorTypeAPI, upstreamFailedMessage, "")
			if status == http.StatusBadRequest || status == http.StatusTooManyRequests {
				wantStatus, wantBody = status, reply
			}
			if resp.StatusCode != wantStatus || string(body) != wantBody {
				t.Errorf("got %d %s, want %d %s", resp.StatusCode, body, wantStatus, wantBody)
			}
			if got := resp.Header.Get("Retry-After"); got != "17" {
				t.Errorf("retry-after = %q, want the upstream's", got)
			}
			if got := resp.Header.Get("Request-Id"); !requestIDPattern.MatchString(got) {
				t.Errorf("request-id = %q, want the gateway's own", got)
			}
			if strings.Contains(gw.logs.String(), canary) {
				t.Error("the logs hold the upstream error body")
			}
			checkResult(t, gw.admission.result(t), OutcomeUpstreamError, count(0), count(0))
		})
	}
}

func TestResponsesUnreachableUpstreamIsNotDispatched(t *testing.T) {
	gw := startResponsesGateway(t, Channel{BaseURL: closedPortURL(t)})

	resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
	body := readAllBody(t, resp)
	if want := responsesErrorJSON(openai.ErrorTypeAPI, upstreamFailedMessage, ""); resp.StatusCode != http.StatusBadGateway ||
		string(body) != want {
		t.Errorf("got %d %s, want 502 %s", resp.StatusCode, body, want)
	}
	checkResult(t, gw.admission.result(t), OutcomeNotDispatched, count(0), count(0))
}

// The access log has one line per Responses request on its own route, and never the
// body, the gateway key or the upstream credential.
func TestResponsesAccessLogHoldsNoSecrets(t *testing.T) {
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, responseCreated(0), outputTextDelta(1, "Hi"),
			responseEnd(openai.EventCompleted, 2, `{"input_tokens":5,"output_tokens":1}`))
	})
	channel.CredentialEnv = credentialEnv
	gw := startResponsesGateway(t, channel)

	requests := []struct {
		body   string
		status int
	}{
		{responsesBody("mock-small", true), http.StatusOK},
		{`{"model":"bad","previous_response_id":"resp_1","note":"` + bodyCanary + `"}`, http.StatusBadRequest},
	}
	var ids []string
	for _, r := range requests {
		resp := gw.postResponses(t, context.Background(), r.body, nil)
		readAllBody(t, resp)
		if resp.StatusCode != r.status {
			t.Errorf("status = %d, want %d", resp.StatusCode, r.status)
		}
		ids = append(ids, resp.Header.Get("request-id"))
	}
	gw.Close() // waits for every handler to log

	logs := gw.logs.String()
	for _, secret := range []string{bodyCanary, gatewayKeyValue, upstreamCredential} {
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
		if line.Method != http.MethodPost || line.Route != "POST /v1/responses" || line.Status != r.status ||
			line.DurationMS == nil || line.RequestBytes != int64(len(r.body)) || line.ResponseBytes == 0 {
			t.Errorf("access line of request %d = %+v", i, line)
		}
	}
	if model := lines[ids[0]].Model; model != "mock-small" {
		t.Errorf("model = %q, want mock-small", model)
	}
}
