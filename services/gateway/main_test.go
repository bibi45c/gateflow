package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/config"
)

// register mounts the relay on POST /v1/messages. A request without a gateway key
// is refused before any internal call, so the test needs no access or metering.
func TestRegisterMountsMessages(t *testing.T) {
	mux := http.NewServeMux()
	register(config.Config{
		AccessURL:             "http://access.invalid:8081",
		MeteringURL:           "http://metering.invalid:8082",
		AccessInternalToken:   "access-token-value",
		MeteringInternalToken: "metering-token-value",
	})(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Errorf("POST: status %d, body %s; want 401 authentication_error", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Header().Get("request-id"), "req_") {
		t.Errorf("request-id = %q", rec.Header().Get("request-id"))
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
}

// register also mounts the Responses relay on POST /v1/responses, which answers in the
// Responses error body.
func TestRegisterMountsResponses(t *testing.T) {
	mux := http.NewServeMux()
	register(config.Config{
		AccessURL:             "http://access.invalid:8081",
		MeteringURL:           "http://metering.invalid:8082",
		AccessInternalToken:   "access-token-value",
		MeteringInternalToken: "metering-token-value",
	})(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
	var body struct {
		Type  *string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if rec.Code != http.StatusUnauthorized || body.Error.Type != "authentication_error" || body.Type != nil {
		t.Errorf("POST: status %d, body %s; want 401 and a Responses authentication_error", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Header().Get("request-id"), "req_") {
		t.Errorf("request-id = %q", rec.Header().Get("request-id"))
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
}
