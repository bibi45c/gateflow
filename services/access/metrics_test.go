package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/httpapi"
	"github.com/bibi45c/gateflow/services/access/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeStore verifies every key for tenant-01 and knows no price version.
type fakeStore struct{}

func (fakeStore) VerifyKey(context.Context, []byte, string, string) (store.VerifiedKey, error) {
	return store.VerifiedKey{TenantID: "tenant-01", KeyID: "key-tenant-01", PriceVersionID: "pv-1"}, nil
}

func (fakeStore) PriceVersion(context.Context, string) (store.PriceVersion, error) {
	return store.PriceVersion{}, store.ErrPriceVersionNotFound
}

// The access metric families and the only labels each may have, le being the bucket
// label of a histogram.
var accessMetrics = map[string][]string{
	"gateflow_http_requests_total":                 {"service", "route", "result"},
	"gateflow_http_request_duration_seconds":       {"service", "route", "le"},
	"gateflow_http_request_duration_seconds_sum":   {"service", "route"},
	"gateflow_http_request_duration_seconds_count": {"service", "route"},
	"gateflow_http_request_bytes_total":            {"service", "route"},
	"gateflow_http_response_bytes_total":           {"service", "route"},
}

// GET /metrics, without a token, serves the helper's metrics after a few calls, every
// series with bounded labels only: no tenant, key, model, request id or raw path.
func TestRoutesServeMetrics(t *testing.T) {
	observer, err := httpapi.NewObserver(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	const token = "test-internal-token"
	mux := http.NewServeMux()
	routes(fakeStore{}, nil, config.Service{KeyPepper: []byte("test-pepper"), InternalToken: token}, observer)(mux)
	calls := []struct{ method, path, body, token string }{
		{"POST", "/internal/v1/keys/verify", `{"key":"gf_key-canary","model":"model-canary"}`, token},
		{"POST", "/internal/v1/keys/verify", `{"key":"gf_key-canary","model":"model-canary"}`, "wrong"},
		{"GET", "/internal/v1/price-versions/pv-canary", "", token},
		{"GET", "/internal/v1/raw-path-canary?q=1", "", token},
	}
	for _, c := range calls {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-Request-Id", "req_id-canary")
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", rec.Code)
	}
	all := scrape(t, rec.Body.String())
	seen := map[string]bool{}
	for _, s := range all {
		name := strings.TrimSuffix(s.name, "_bucket")
		seen[name] = true
		checkBounded(t, s, accessMetrics[name])
	}
	for name := range accessMetrics {
		if !seen[name] {
			t.Errorf("/metrics lacks %s", name)
		}
	}
	for _, want := range []struct {
		route, result string
	}{
		{"/internal/v1/keys/verify", "completed"},
		{"/internal/v1/keys/verify", "authentication_denied"},
		{"/internal/v1/price-versions/{id}", "invalid_request"},
		{"/internal/v1/", "invalid_request"},
	} {
		checkValue(t, all, "gateflow_http_requests_total", map[string]string{"route": want.route, "result": want.result}, 1)
	}
	if strings.Contains(rec.Body.String(), "canary") || strings.Contains(rec.Body.String(), "tenant-01") {
		t.Errorf("/metrics holds request input or a tenant:\n%s", rec.Body)
	}
}

// series is one sample of the text exposition format.
type series struct {
	name   string
	labels map[string]string
	value  float64
}

// scrape parses the samples of a /metrics body. Label values here never hold a quote,
// a comma or a brace, so a simple split suffices.
func scrape(t *testing.T, body string) []series {
	t.Helper()
	var all []series
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		head, value, _ := strings.Cut(line, " ")
		name, rest, hasLabels := strings.Cut(head, "{")
		s := series{name: name, labels: map[string]string{}}
		if hasLabels {
			for _, pair := range strings.Split(strings.TrimSuffix(rest, "}"), ",") {
				key, quoted, _ := strings.Cut(pair, "=")
				s.labels[key] = strings.Trim(quoted, `"`)
			}
		}
		var err error
		if s.value, err = strconv.ParseFloat(value, 64); err != nil {
			t.Fatalf("sample %q: %v", line, err)
		}
		all = append(all, s)
	}
	return all
}

var (
	results = map[string]bool{"completed": true, "not_dispatched": true, "upstream_error": true,
		"client_cancelled": true, "interrupted": true, "invalid_request": true, "authentication_denied": true,
		"permission_denied": true, "insufficient_balance": true, "tenant_limited": true,
		"capacity_rejected": true, "dependency_error": true, "internal_error": true}
	accessRoutes = map[string]bool{"/internal/v1/keys/verify": true, "/internal/v1/price-versions/{id}": true,
		"/internal/v1/": true, "/internal/v1/console/login": true, "/internal/v1/tenants": true,
		"/internal/v1/tenants/{tenant_id}/keys": true, "/internal/v1/tenants/{tenant_id}/keys/{key_id}/revoke": true,
		"/internal/v1/channels": true, "/internal/v1/price-versions": true}
)

// checkBounded fails the test unless s has exactly the allowed labels, each with a
// value from its fixed set.
func checkBounded(t *testing.T, s series, allowed []string) {
	t.Helper()
	if len(allowed) == 0 {
		t.Errorf("unexpected metric %s", s.name)
		return
	}
	if len(s.labels) != len(allowed) {
		t.Errorf("%s has labels %v, want only %v", s.name, s.labels, allowed)
	}
	for _, key := range allowed {
		value, ok := s.labels[key]
		switch {
		case !ok:
			t.Errorf("%s lacks the label %s", s.name, key)
		case key == "service" && value != "access",
			key == "route" && !accessRoutes[value],
			key == "result" && !results[value]:
			t.Errorf("%s has %s=%q, outside its fixed set", s.name, key, value)
		}
	}
}

// checkValue fails the test unless the sample of name with the labels in match has
// the value want.
func checkValue(t *testing.T, all []series, name string, match map[string]string, want float64) {
	t.Helper()
	for _, s := range all {
		matches := s.name == name
		for key, value := range match {
			matches = matches && s.labels[key] == value
		}
		if matches {
			if s.value != want {
				t.Errorf("%s%v = %v, want %v", name, match, s.value, want)
			}
			return
		}
	}
	t.Errorf("no sample %s%v", name, match)
}
