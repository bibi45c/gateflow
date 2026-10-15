package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/bibi45c/gateflow/pkg/health"
	"github.com/bibi45c/gateflow/services/access/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// newObservedServer is newTestServer with an observer, whose registry it returns.
func newObservedServer(t *testing.T, s Store) (http.Handler, *bytes.Buffer, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	observer, err := NewObserver(registry)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler("access"))
	New(s, []byte(testPepper), testToken, slog.New(slog.NewJSONHandler(logs, nil)), observer).Register(mux)
	return mux, logs, registry
}

// counted returns the requests_total series of route and result.
func counted(t *testing.T, registry *prometheus.Registry, route, result string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, family := range families {
		if family.GetName() != "gateflow_http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels["route"] == route && (result == "" || labels["result"] == result) {
				sum += metric.GetCounter().GetValue()
			}
		}
	}
	return sum
}

// Each request is counted once, under its route template, with the result of the
// error code it was answered with; its log line names the same result. The probe of
// /healthz is not counted.
func TestRequestsAreCountedByResult(t *testing.T) {
	verified := store.VerifiedKey{TenantID: "tenant-01", KeyID: "key-tenant-01"}
	tests := []struct {
		name         string
		store        *fakeStore
		method, path string
		body         string
		token        bool
		route        string
		result       string
	}{
		{"verified", &fakeStore{key: verified}, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), true,
			verifyRoute, "completed"},
		{"wrong token", &fakeStore{}, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), false,
			verifyRoute, "authentication_denied"},
		{"bad request", &fakeStore{}, "POST", "/internal/v1/keys/verify", `{}`, true, verifyRoute, "invalid_request"},
		{"invalid key", &fakeStore{err: store.ErrInvalidKey}, "POST", "/internal/v1/keys/verify",
			verifyBody(testKey, "mock-fast"), true, verifyRoute, "authentication_denied"},
		{"model not allowed", &fakeStore{key: verified, err: store.ErrModelNotAllowed}, "POST", "/internal/v1/keys/verify",
			verifyBody(testKey, "mock-fast"), true, verifyRoute, "permission_denied"},
		{"database unavailable", &fakeStore{err: store.ErrUnavailable}, "POST", "/internal/v1/keys/verify",
			verifyBody(testKey, "mock-fast"), true, verifyRoute, "dependency_error"},
		{"unexpected error", &fakeStore{err: errors.New("unexpected")}, "POST", "/internal/v1/keys/verify",
			verifyBody(testKey, "mock-fast"), true, verifyRoute, "internal_error"},
		{"panic", &fakeStore{panics: true}, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), true,
			verifyRoute, "internal_error"},
		{"price version", &fakeStore{}, "GET", "/internal/v1/price-versions/pv-1", "", true, priceVersionRoute, "completed"},
		{"price version not found", &fakeStore{err: store.ErrPriceVersionNotFound}, "GET",
			"/internal/v1/price-versions/pv-1", "", true, priceVersionRoute, "invalid_request"},
		{"no such route", &fakeStore{}, "GET", "/internal/v1/no-such-route", "", true, otherRoute, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, logs, registry := newObservedServer(t, tt.store)
			headers := map[string]string{}
			if tt.token {
				headers = authorized()
			}
			call(handler, tt.method, tt.path, tt.body, headers)
			call(handler, "GET", "/healthz", "", nil)

			if got := counted(t, registry, tt.route, tt.result); got != 1 {
				t.Errorf("requests{route=%q,result=%q} = %v, want 1", tt.route, tt.result, got)
			}
			for _, route := range []string{verifyRoute, priceVersionRoute, otherRoute} {
				want := 0.0
				if route == tt.route {
					want = 1
				}
				if got := counted(t, registry, route, ""); got != want {
					t.Errorf("requests{route=%q} = %v, want %v", route, got, want)
				}
			}
			var line map[string]any
			if err := json.Unmarshal(lastLine(logs.Bytes()), &line); err != nil {
				t.Fatalf("log %q: %v", logs, err)
			}
			if line["msg"] != "request" || line["result"] != tt.result {
				t.Errorf("log line %v, want the result %s", line, tt.result)
			}
		})
	}
}

// lastLine returns the last line of logs, which is the request line: a panic line
// comes before it.
func lastLine(logs []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(logs), []byte("\n"))
	return lines[len(lines)-1]
}
