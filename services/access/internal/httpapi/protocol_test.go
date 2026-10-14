package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/access/internal/store"
)

// verifyBodyOver is a verify body that names the protocol, or leaves it out when it
// is empty.
func verifyBodyOver(key, model, protocol string) string {
	if protocol == "" {
		return verifyBody(key, model)
	}
	body, _ := json.Marshal(map[string]string{"key": key, "model": model, "protocol": protocol})
	return string(body)
}

// The protocol is optional, but when present it must be exactly one of the two, once,
// under exactly that name.
func TestVerifyRejectsABadProtocol(t *testing.T) {
	const secret = "gf_secret-input-value"
	start := `{"key":"` + secret + `","model":"mock-fast",`
	wantBadRequests(t, secret, map[string]string{
		"unknown protocol":                verifyBodyOver(secret, "mock-fast", "openai-chat"),
		"empty protocol":                  start + `"protocol":""}`,
		"upper-case protocol":             verifyBodyOver(secret, "mock-fast", "OPENAI-RESPONSES"),
		"protocol with a space":           verifyBodyOver(secret, "mock-fast", "anthropic-messages "),
		"null protocol":                   start + `"protocol":null}`,
		"number protocol":                 start + `"protocol":1}`,
		"array protocol":                  start + `"protocol":["openai-responses"]}`,
		"object protocol":                 start + `"protocol":{"name":"openai-responses"}}`,
		"capitalised protocol name":       start + `"Protocol":"openai-responses"}`,
		"duplicate protocol":              start + `"protocol":"anthropic-messages","protocol":"openai-responses"}`,
		"case-variant protocol duplicate": start + `"protocol":"anthropic-messages","PROTOCOL":"openai-responses"}`,
		"protocol without a model":        `{"key":"` + secret + `","protocol":"openai-responses"}`,
	})
}

// A body without protocol asks for Messages channels, exactly as one naming
// anthropic-messages does, and the answer is the same.
func TestVerifyAsksTheStoreForTheProtocol(t *testing.T) {
	channels := map[string]store.Channel{
		"anthropic-messages": {ID: "mock", Protocol: "anthropic-messages", BaseURL: "http://mock-upstream:8090"},
		"openai-responses":   {ID: "mock-responses", Protocol: "openai-responses", BaseURL: "http://mock-upstream:8090"},
	}
	const prefix = `{"tenant_id":"tenant-01","key_id":"key-tenant-01","price_version_id":"pv-seed-1","channel":`
	tests := []struct{ protocol, want, body string }{
		{"", "anthropic-messages", prefix +
			`{"id":"mock","protocol":"anthropic-messages","base_url":"http://mock-upstream:8090","credential_env":""}}`},
		{"anthropic-messages", "anthropic-messages", prefix +
			`{"id":"mock","protocol":"anthropic-messages","base_url":"http://mock-upstream:8090","credential_env":""}}`},
		{"openai-responses", "openai-responses", prefix +
			`{"id":"mock-responses","protocol":"openai-responses","base_url":"http://mock-upstream:8090","credential_env":""}}`},
	}
	for _, tt := range tests {
		fake := &fakeStore{key: store.VerifiedKey{TenantID: "tenant-01", KeyID: "key-tenant-01",
			PriceVersionID: "pv-seed-1", Channel: channels[tt.want]}}
		handler, _ := newTestServer(fake, testToken)
		got := call(handler, "POST", "/internal/v1/keys/verify", verifyBodyOver(testKey, "mock-fast", tt.protocol), authorized())
		if got.Code != http.StatusOK {
			t.Fatalf("protocol %q: status %d, want 200: %s", tt.protocol, got.Code, got.Body)
		}
		if fake.calls != 1 || fake.gotProtocol != tt.want || fake.gotModel != "mock-fast" {
			t.Errorf("protocol %q: the store was asked %d times, for %q over %q; want once, over %q",
				tt.protocol, fake.calls, fake.gotModel, fake.gotProtocol, tt.want)
		}
		if body := strings.TrimSpace(got.Body.String()); body != tt.body {
			t.Errorf("protocol %q: body = %s\nwant   %s", tt.protocol, body, tt.body)
		}
	}
}

// A model the key may not use over the protocol is still 403, whichever protocol.
func TestVerifyModelNotAllowedOverAProtocol(t *testing.T) {
	for _, protocol := range []string{"", "anthropic-messages", "openai-responses"} {
		fake := &fakeStore{err: store.ErrModelNotAllowed}
		handler, _ := newTestServer(fake, testToken)
		got := call(handler, "POST", "/internal/v1/keys/verify", verifyBodyOver(testKey, "claude-not-seeded", protocol), authorized())
		wantError(t, got, http.StatusForbidden, CodeModelNotAllowed)
		if fake.calls != 1 {
			t.Errorf("protocol %q: the store was asked %d times, want once", protocol, fake.calls)
		}
	}
}
