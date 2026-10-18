package admission_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// An admitted call is verified, reserved, relayed and settled, each internal call
// with its own token and the gateway's request id.
func TestAdmittedCallIsVerifiedReservedRelayedAndSettled(t *testing.T) {
	h := start(t, setup{})

	resp := h.post(context.Background(), nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("message_stop")) {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	h.finish()
	id := resp.Header.Get("request-id")

	verify := h.verify.requests()
	if len(verify) != 1 {
		t.Fatalf("%d verify calls, want 1", len(verify))
	}
	checkInternalCall(t, verify[0], accessToken, id)
	checkJSON(t, "verify body", verify[0].Body, `{"key":"`+gatewayKey+`","model":"mock-small"}`)

	reserve := h.reserve.requests()
	if len(reserve) != 1 {
		t.Fatalf("%d reserve calls, want 1", len(reserve))
	}
	checkInternalCall(t, reserve[0], meteringToken, id)
	checkJSON(t, "reserve body", reserve[0].Body, fmt.Sprintf(`{"call_id":%q,"attempt_id":1,"tenant_id":"tenant-01",`+
		`"price_version_id":"pv-fixture-2-5","max_input_tokens":%d,"max_output_tokens":20,"model":"mock-small"}`, id, len(messagesBody)))

	settle := h.settle.requests()
	if len(settle) != 1 {
		t.Fatalf("%d settle calls, want 1", len(settle))
	}
	checkInternalCall(t, settle[0], meteringToken, id)
	if settle[0].Path != "/internal/v1/reservations/rsv_f1/settle" {
		t.Errorf("settle path = %s", settle[0].Path)
	}
	checkJSON(t, "settle body", settle[0].Body, `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`)

	upstream := h.upstream.requests()
	if len(upstream) != 1 {
		t.Fatalf("%d upstream calls, want 1", len(upstream))
	}
	for name, values := range upstream[0].Header {
		for _, secret := range []string{gatewayKey, accessToken, meteringToken} {
			if strings.Contains(strings.Join(values, " "), secret) {
				t.Errorf("the upstream got %s in its %s header", secret, name)
			}
		}
	}
	if lines := h.errorLines(); len(lines) > 0 {
		t.Errorf("error logs: %v", lines)
	}
}

// A key sent as an Authorization bearer token is the one verified.
func TestBearerKeyIsVerified(t *testing.T) {
	h := start(t, setup{})

	resp := h.post(context.Background(), http.Header{"Authorization": {"Bearer " + gatewayKey}})
	readBody(t, resp)
	h.finish()
	if verify := h.verify.requests(); len(verify) != 1 {
		t.Fatalf("%d verify calls, want 1", len(verify))
	} else {
		checkJSON(t, "verify body", verify[0].Body, `{"key":"`+gatewayKey+`","model":"mock-small"}`)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// A channel with a credential gets it from the gateway's environment, and the logs
// never hold it.
func TestCredentialChannelSendsTheCredential(t *testing.T) {
	h := start(t, setup{tlsUpstream: true})

	resp := h.post(context.Background(), nil)
	readBody(t, resp)
	h.finish()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	upstream := h.upstream.requests()
	if len(upstream) != 1 || upstream[0].Header.Get("X-Api-Key") != upstreamCredential {
		t.Errorf("the upstream did not get the credential in x-api-key")
	}
	if len(h.settle.requests()) != 1 {
		t.Errorf("not settled once")
	}
}

// A refused call is never sent upstream, never settled, and its public error holds
// no internal code.
func TestRefusedCallsNeverReachTheUpstream(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     setup
		status    int
		errorType string
		code      string // the internal code, which the client must not see
		reserves  int
		logged    bool // an error log line is expected
	}{
		{"invalid key", setup{verify: internalError(401, "invalid_key")}, 401, anthropic.ErrorTypeAuthentication, "invalid_key", 0, false},
		{"model not allowed", setup{verify: internalError(403, "model_not_allowed")}, 403, anthropic.ErrorTypePermission, "model_not_allowed", 0, false},
		{"access dependency unavailable", setup{verify: internalError(503, "dependency_unavailable")}, 503, anthropic.ErrorTypeAPI, "dependency_unavailable", 0, true},
		{"access times out", setup{verify: hang}, 503, anthropic.ErrorTypeAPI, "", 0, true},
		{"access is down", setup{accessDown: true}, 503, anthropic.ErrorTypeAPI, "", 0, true},
		{"access internal error", setup{verify: internalError(500, "internal_error")}, 500, anthropic.ErrorTypeAPI, "internal_error", 0, true},
		{"access refuses the internal token", setup{verify: internalError(401, "unauthorized")}, 500, anthropic.ErrorTypeAPI, "unauthorized", 0, true},
		{"access unknown code", setup{verify: internalError(401, "key_on_fire")}, 500, anthropic.ErrorTypeAPI, "key_on_fire", 0, true},
		{"access malformed answer", setup{verify: raw(200, `{"tenant_id":`)}, 500, anthropic.ErrorTypeAPI, "", 0, true},
		{"insufficient balance", setup{reserve: internalError(409, "insufficient_balance")}, 402, anthropic.ErrorTypeBilling, "insufficient_balance", 1, false},
		{"wallet not found", setup{reserve: internalError(404, "wallet_not_found")}, 402, anthropic.ErrorTypeBilling, "wallet_not_found", 1, false},
		{"metering dependency unavailable twice", setup{reserve: internalError(503, "dependency_unavailable")}, 503, anthropic.ErrorTypeAPI, "dependency_unavailable", 2, true},
		{"metering internal error twice", setup{reserve: internalError(500, "internal_error")}, 503, anthropic.ErrorTypeAPI, "internal_error", 2, true},
		{"metering times out twice", setup{reserve: hang}, 503, anthropic.ErrorTypeAPI, "", 2, true},
		{"metering is down", setup{meteringDown: true}, 503, anthropic.ErrorTypeAPI, "", 0, true},
		{"idempotency conflict", setup{reserve: internalError(422, "idempotency_conflict")}, 500, anthropic.ErrorTypeAPI, "idempotency_conflict", 1, true},
		{"metering refuses the internal token", setup{reserve: internalError(401, "unauthorized")}, 500, anthropic.ErrorTypeAPI, "unauthorized", 1, true},
		{"metering invalid request", setup{reserve: internalError(400, "invalid_request")}, 500, anthropic.ErrorTypeAPI, "invalid_request", 1, true},
		{"metering unknown code", setup{reserve: internalError(400, "wallet_on_fire")}, 500, anthropic.ErrorTypeAPI, "wallet_on_fire", 1, true},
		{"reserve confirms no hold", setup{reserve: raw(201, `{"max_cost":300,"status":"reserved"}`)}, 500, anthropic.ErrorTypeAPI, "", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, tc.setup)

			body := checkPublicError(t, h.post(context.Background(), nil), tc.status, tc.errorType)
			h.finish()
			if tc.code != "" && strings.Contains(body, tc.code) {
				t.Errorf("the public error %s holds the internal code %s", body, tc.code)
			}
			if n := len(h.upstream.requests()); n != 0 {
				t.Errorf("the upstream was called %d times", n)
			}
			if n := len(h.reserve.requests()); n != tc.reserves {
				t.Errorf("%d reserve calls, want %d", n, tc.reserves)
			}
			if n := len(h.settle.requests()); n != 0 {
				t.Errorf("%d settle calls, want none", n)
			}
			if logged := len(h.errorLines()) > 0; logged != tc.logged {
				t.Errorf("error logged = %v, want %v; logs:\n%s", logged, tc.logged, h.logs.String())
			}
		})
	}
}

// A reserve that times out, answers 5xx or loses its connection is retried once with
// the same content; the call then goes on as usual.
func TestReserveIsRetriedOnceWithTheSameContent(t *testing.T) {
	for name, first := range map[string]answer{
		"timeout":            hang,
		"503":                internalError(503, "dependency_unavailable"),
		"500":                internalError(500, "internal_error"),
		"connection dropped": dropConnection,
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, setup{reserve: failFirst(first, reserveOK)})

			resp := h.post(context.Background(), nil)
			readBody(t, resp)
			h.finish()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			reserve := h.reserve.requests()
			if len(reserve) != 2 {
				t.Fatalf("%d reserve calls, want 2", len(reserve))
			}
			if !bytes.Equal(reserve[0].Body, reserve[1].Body) {
				t.Errorf("the retry sent %s after %s", reserve[1].Body, reserve[0].Body)
			}
			id := resp.Header.Get("request-id")
			checkInternalCall(t, reserve[0], meteringToken, id)
			checkInternalCall(t, reserve[1], meteringToken, id)
			if n := len(h.upstream.requests()); n != 1 {
				t.Errorf("the upstream was called %d times, want 1", n)
			}
			if n := len(h.settle.requests()); n != 1 {
				t.Errorf("%d settle calls, want 1", n)
			}
		})
	}
}

// A misconfigured channel gets 500, is logged, and gets no reservation.
func TestMisconfiguredChannelIsNotReserved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel channelJSON
	}{
		{"base_url with a path", channelJSON{BaseURL: "http://mock-upstream:8090/v1"}},
		{"base_url with a trailing slash", channelJSON{BaseURL: "http://mock-upstream:8090/"}},
		{"base_url with a query", channelJSON{BaseURL: "http://mock-upstream:8090?x=1"}},
		{"base_url with another scheme", channelJSON{BaseURL: "ftp://mock-upstream:8090"}},
		{"empty base_url", channelJSON{}},
		{"credential_env with a bad name", channelJSON{BaseURL: "https://api.example.com", CredentialEnv: "HOME"}},
		{"credential_env in lower case", channelJSON{BaseURL: "https://api.example.com", CredentialEnv: "upstream_credential_test"}},
		{"credential_env on http", channelJSON{BaseURL: "http://api.example.com", CredentialEnv: credentialEnv}},
		{"credential_env unset", channelJSON{BaseURL: "https://api.example.com", CredentialEnv: "UPSTREAM_CREDENTIAL_UNSET"}},
		{"another protocol", channelJSON{Protocol: "openai-responses", BaseURL: "http://mock-upstream:8090"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := tc.channel
			channel.ID = "broken"
			if channel.Protocol == "" {
				channel.Protocol = "anthropic-messages"
			}
			h := start(t, setup{verify: verifyOK(channel)})

			checkPublicError(t, h.post(context.Background(), nil), http.StatusInternalServerError, anthropic.ErrorTypeAPI)
			h.finish()
			if n := len(h.reserve.requests()); n != 0 {
				t.Errorf("%d reserve calls, want none", n)
			}
			if n := len(h.upstream.requests()); n != 0 {
				t.Errorf("the upstream was called %d times", n)
			}
			lines := h.errorLines()
			if len(lines) != 1 || lines[0]["channel_id"] != "broken" {
				t.Errorf("error logs = %v, want one naming the channel", lines)
			}
		})
	}
}
