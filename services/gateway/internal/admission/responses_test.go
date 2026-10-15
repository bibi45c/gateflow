package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// responsesBody is a request as Codex sends it, without max_output_tokens, so the
// gateway holds 32768 output tokens and sends that bound upstream.
const responsesBody = `{"model":"mock-small","stream":true,"store":false,` +
	`"input":[{"role":"user","content":"Hello ` + bodyCanary + `"}]}`

// responsesEvent returns a Responses event of eventType, numbered seq, whose data
// ends with rest.
func responsesEvent(eventType string, seq int, rest string) string {
	return event(eventType, fmt.Sprintf(`{"type":%q,"sequence_number":%d%s}`, eventType, seq, rest))
}

// responsesEnd returns the event that ends a Responses stream, with usage.
func responsesEnd(eventType string, seq int, usage string) string {
	status := strings.TrimPrefix(eventType, "response.")
	return responsesEvent(eventType, seq, `,"response":{"id":"resp_1","status":"`+status+`","usage":`+usage+`}`)
}

var (
	responseCreated = responsesEvent("response.created", 0, `,"response":{"id":"resp_1","status":"in_progress","usage":null}`)
	outputTextDelta = responsesEvent("response.output_text.delta", 1, `,"item_id":"msg_1","output_index":0,"delta":"Hi"`)
	// fixtureUsage reports 60 input tokens, 40 of them cached, and 10 output tokens:
	// the first row of the shared fixture, with the cached tokens not added again.
	fixtureUsage = `{"input_tokens":60,"input_tokens_details":{"cached_tokens":40},"output_tokens":10,` +
		`"output_tokens_details":{"reasoning_tokens":0},"total_tokens":70}`
)

// completedResponses is a completed Responses stream with fixtureUsage.
var completedResponses = streamReply(responseCreated, outputTextDelta,
	responsesEnd("response.completed", 2, fixtureUsage))

// postResponses sends a Responses request with the gateway key as a bearer token, as
// Codex sends it, or with header instead when it is not nil.
func (h *harness) postResponses(ctx context.Context, body string, header http.Header) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.gateway.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if header == nil {
		req.Header.Set("Authorization", "Bearer "+gatewayKey)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// checkResponsesError fails the test unless resp is a Responses error of status and
// type, and returns its body.
func checkResponsesError(t *testing.T, resp *http.Response, status int, errorType string) string {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != status {
		t.Errorf("status = %d, want %d; body %s", resp.StatusCode, status, body)
	}
	var decoded map[string]map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	detail := decoded["error"]
	if message, _ := detail["message"].(string); len(decoded) != 1 || detail["type"] != errorType || message == "" {
		t.Errorf("body = %s, want a Responses error of type %s", body, errorType)
	}
	return string(body)
}

// A Responses call is verified over openai-responses, held at 32768 output tokens,
// relayed with that bound and settled on input_tokens alone.
func TestResponsesCallIsVerifiedReservedRelayedAndSettled(t *testing.T) {
	h := start(t, setup{responses: true})

	resp := h.postResponses(context.Background(), responsesBody, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	h.finish()
	id := resp.Header.Get("request-id")

	verify := h.verify.requests()
	if len(verify) != 1 {
		t.Fatalf("%d verify calls, want 1", len(verify))
	}
	checkInternalCall(t, verify[0], accessToken, id)
	checkJSON(t, "verify body", verify[0].Body,
		`{"key":"`+gatewayKey+`","model":"mock-small","protocol":"openai-responses"}`)

	reserve := h.reserve.requests()
	if len(reserve) != 1 {
		t.Fatalf("%d reserve calls, want 1", len(reserve))
	}
	checkJSON(t, "reserve body", reserve[0].Body, fmt.Sprintf(`{"call_id":%q,"attempt_id":1,"tenant_id":"tenant-01",`+
		`"price_version_id":"pv-fixture-2-5","max_input_tokens":%d,"max_output_tokens":32768}`, id, len(responsesBody)))

	upstream := h.upstream.requests()
	if len(upstream) != 1 {
		t.Fatalf("%d upstream calls, want 1", len(upstream))
	}
	wantBody := strings.TrimSuffix(responsesBody, "}") + `,"max_output_tokens":32768}`
	if upstream[0].Path != "/v1/responses" || string(upstream[0].Body) != wantBody {
		t.Errorf("upstream got %s %s, want /v1/responses %s", upstream[0].Path, upstream[0].Body, wantBody)
	}
	for name, values := range upstream[0].Header {
		for _, secret := range []string{gatewayKey, accessToken, meteringToken} {
			if strings.Contains(strings.Join(values, " "), secret) {
				t.Errorf("the upstream got %s in its %s header", secret, name)
			}
		}
	}

	settle := h.settle.requests()
	if len(settle) != 1 {
		t.Fatalf("%d settle calls, want 1", len(settle))
	}
	checkInternalCall(t, settle[0], meteringToken, id)
	checkJSON(t, "settle body", settle[0].Body, `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`)
	if lines := h.errorLines(); len(lines) > 0 {
		t.Errorf("error logs: %v", lines)
	}
}

// The output bound reaches reserve and the upstream: a max_output_tokens that is
// absent or null is held at 32768 and sent as 32768, and one that is set is held at
// its value and sent unchanged. The input bound is always the client's body size.
func TestResponsesHoldsAndSendsTheOutputBound(t *testing.T) {
	for _, tc := range []struct {
		name, body, upstream string
		hold                 int
	}{
		{"absent", `{"model":"mock-small","stream":true}`,
			`{"model":"mock-small","stream":true,"max_output_tokens":32768}`, 32768},
		{"null", `{"model":"mock-small","max_output_tokens":null,"stream":true}`,
			`{"model":"mock-small","max_output_tokens":32768,"stream":true}`, 32768},
		{"set", `{"model":"mock-small","max_output_tokens":200,"stream":true}`,
			`{"model":"mock-small","max_output_tokens":200,"stream":true}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, setup{responses: true})

			resp := h.postResponses(context.Background(), tc.body, nil)
			readBody(t, resp)
			h.finish()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			reserve := h.reserve.requests()
			if len(reserve) != 1 {
				t.Fatalf("%d reserve calls, want 1", len(reserve))
			}
			checkJSON(t, "reserve body", reserve[0].Body, fmt.Sprintf(`{"call_id":%q,"attempt_id":1,`+
				`"tenant_id":"tenant-01","price_version_id":"pv-fixture-2-5","max_input_tokens":%d,`+
				`"max_output_tokens":%d}`, resp.Header.Get("request-id"), len(tc.body), tc.hold))
			if upstream := h.upstream.requests(); len(upstream) != 1 || string(upstream[0].Body) != tc.upstream {
				t.Errorf("upstream got %v, want one request with %s", upstream, tc.upstream)
			}
			if n := len(h.settle.requests()); n != 1 {
				t.Errorf("%d settle calls, want 1", n)
			}
		})
	}
}

// A channel with a credential gets it as a bearer token, never in x-api-key.
func TestResponsesCredentialChannelSendsABearerCredential(t *testing.T) {
	h := start(t, setup{responses: true, tlsUpstream: true})

	resp := h.postResponses(context.Background(), responsesBody, nil)
	readBody(t, resp)
	h.finish()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	upstream := h.upstream.requests()
	if len(upstream) != 1 || upstream[0].Header.Get("Authorization") != "Bearer "+upstreamCredential ||
		upstream[0].Header.Get("X-Api-Key") != "" {
		t.Errorf("the upstream did not get the credential as a bearer token alone")
	}
	if len(h.settle.requests()) != 1 {
		t.Errorf("not settled once")
	}
}

// A Responses call whose channel speaks another protocol gets 500, is logged, and
// gets no reservation; so does a Messages call on a Responses channel.
func TestChannelOfAnotherProtocolIsNotReserved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string // the channel's
		post     func(h *harness) *http.Response
		check    func(t *testing.T, resp *http.Response, status int, errorType string) string
	}{
		{"responses call on a messages channel", "anthropic-messages",
			func(h *harness) *http.Response { return h.postResponses(context.Background(), responsesBody, nil) },
			checkResponsesError},
		{"messages call on a responses channel", "openai-responses",
			func(h *harness) *http.Response { return h.post(context.Background(), nil) },
			checkPublicError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := channelJSON{ID: "other-protocol", Protocol: tc.protocol, BaseURL: "http://mock-upstream:8090"}
			h := start(t, setup{verify: verifyOK(channel)})

			tc.check(t, tc.post(h), http.StatusInternalServerError, "api_error")
			h.finish()
			if n := len(h.reserve.requests()); n != 0 {
				t.Errorf("%d reserve calls, want none", n)
			}
			if n := len(h.upstream.requests()); n != 0 {
				t.Errorf("the upstream was called %d times", n)
			}
			lines := h.errorLines()
			if len(lines) != 1 || lines[0]["channel_id"] != "other-protocol" {
				t.Errorf("error logs = %v, want one naming the channel", lines)
			}
		})
	}
}

// A refused Responses call is never sent upstream nor settled, and gets its public
// error in the Responses body.
func TestResponsesRefusedCallsNeverReachTheUpstream(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     setup
		status    int
		errorType string
		reserves  int
	}{
		{"invalid key", setup{verify: internalError(401, "invalid_key")}, 401, "authentication_error", 0},
		{"model not allowed", setup{verify: internalError(403, "model_not_allowed")}, 403, "permission_error", 0},
		{"access is down", setup{accessDown: true}, 503, "api_error", 0},
		{"insufficient balance", setup{reserve: internalError(409, "insufficient_balance")}, 402, "billing_error", 1},
		{"metering times out twice", setup{reserve: hang}, 503, "api_error", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.setup
			s.responses = true
			h := start(t, s)

			checkResponsesError(t, h.postResponses(context.Background(), responsesBody, nil), tc.status, tc.errorType)
			h.finish()
			if n := len(h.upstream.requests()); n != 0 {
				t.Errorf("the upstream was called %d times", n)
			}
			if n := len(h.reserve.requests()); n != tc.reserves {
				t.Errorf("%d reserve calls, want %d", n, tc.reserves)
			}
			if n := len(h.settle.requests()); n != 0 {
				t.Errorf("%d settle calls, want none", n)
			}
		})
	}
}

// A body that fails the Responses checks gets 400 before verify, so nothing is held.
func TestResponsesBodyChecksHoldNothing(t *testing.T) {
	for name, body := range map[string]string{
		"previous_response_id":    `{"model":"mock-small","previous_response_id":"resp_1"}`,
		"max_output_tokens 0":     `{"model":"mock-small","max_output_tokens":0}`,
		"max_output_tokens 32769": `{"model":"mock-small","max_output_tokens":32769}`,
		"max_output_tokens case":  `{"model":"mock-small","MAX_OUTPUT_TOKENS":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, setup{responses: true})

			checkResponsesError(t, h.postResponses(context.Background(), body, nil), http.StatusBadRequest,
				"invalid_request_error")
			h.finish()
			for route, n := range map[string]int{"verify": len(h.verify.requests()),
				"reserve": len(h.reserve.requests()), "settle": len(h.settle.requests()),
				"upstream": len(h.upstream.requests())} {
				if n != 0 {
					t.Errorf("%d %s calls, want none", n, route)
				}
			}
		})
	}
}

// dropAfter streams events, then closes the connection without ending the stream.
func dropAfter(events ...string) answer {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		streamReply(events...)(w, r, n)
		panic(http.ErrAbortHandler)
	}
}

// Every Responses call that reached the upstream is settled exactly once, with the
// outcome and usage of api/metering.yaml.
func TestResponsesSettlesEachCallOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		upstream    answer
		unreachable bool // the channel's address refuses connections
		status      int
		settle      string
	}{
		{"completed", completedResponses, false, http.StatusOK,
			`{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`},
		{"incomplete", streamReply(responseCreated, responsesEnd("response.incomplete", 1, fixtureUsage)), false,
			http.StatusOK, `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`},
		{"failed", streamReply(responseCreated, outputTextDelta,
			responsesEnd("response.failed", 2, `{"input_tokens":60,"output_tokens":4}`)), false,
			http.StatusOK, `{"input_tokens":60,"output_tokens":4,"outcome":"interrupted"}`},
		{"error event", streamReply(responseCreated, outputTextDelta,
			responsesEvent("error", 2, `,"code":"server_error","message":"Overloaded","param":null`)), false,
			http.StatusOK, `{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`},
		{"connection drops", dropAfter(responseCreated, outputTextDelta), false, http.StatusOK,
			`{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`},
		{"failed body", raw(http.StatusOK, `{"id":"resp_1","object":"response","status":"failed",`+
			`"usage":{"input_tokens":60,"output_tokens":0}}`), false, http.StatusOK,
			`{"input_tokens":60,"output_tokens":0,"outcome":"interrupted"}`},
		{"upstream error", raw(http.StatusInternalServerError, `{"error":{"message":"boom","type":"server_error"}}`),
			false, http.StatusBadGateway, `{"input_tokens":0,"output_tokens":0,"outcome":"upstream_error"}`},
		{"not dispatched", nil, true, http.StatusBadGateway,
			`{"input_tokens":0,"output_tokens":0,"outcome":"not_dispatched"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setup{upstream: tc.upstream, responses: true}
			if tc.unreachable {
				s.verify = verifyOK(channelJSON{ID: "mock-responses", Protocol: "openai-responses", BaseURL: closedPortURL(t)})
			}
			h := start(t, s)

			resp := h.postResponses(context.Background(), responsesBody, nil)
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

// A client that leaves a Responses stream is still charged, once.
func TestResponsesClientDisconnectStillSettles(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request, n int) {
		streamReply(responseCreated)(w, r, n)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	h := start(t, setup{upstream: upstream, responses: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := h.postResponses(ctx, responsesBody, nil)
	if _, err := sse.NewReader(resp.Body).Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	h.finish()

	settle := h.settle.requests()
	if len(settle) != 1 {
		t.Fatalf("%d settle calls, want 1", len(settle))
	}
	checkJSON(t, "settle body", settle[0].Body, `{"input_tokens":null,"output_tokens":null,"outcome":"client_cancelled"}`)
	if lines := h.errorLines(); len(lines) > 0 {
		t.Errorf("error logs: %v", lines)
	}
}
