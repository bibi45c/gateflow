package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/config"
)

// testConfig names internal services that no test reaches.
var testConfig = config.Config{
	AccessURL:             "http://access.invalid:8081",
	MeteringURL:           "http://metering.invalid:8082",
	AccessInternalToken:   "access-token-value",
	MeteringInternalToken: "metering-token-value",
	ModelAliases:          config.DefaultModelAliases,
}

// mount returns a mux with the gateway's routes, as server.Main builds it, whose
// request lines go to logs.
func mount(t *testing.T, logs io.Writer) *http.ServeMux {
	t.Helper()
	routes, err := register(testConfig, logs)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	routes(mux)
	return mux
}

// register mounts the relay on POST /v1/messages. A request without a gateway key
// is refused before any internal call, so the test needs no access or metering.
func TestRegisterMountsMessages(t *testing.T) {
	mux := mount(t, io.Discard)

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
	mux := mount(t, io.Discard)

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

// The gateway's metric families and the only labels each may have, le being the
// bucket label of a histogram.
var gatewayMetrics = map[string][]string{
	"gateflow_http_requests_total":                 {"service", "route", "result"},
	"gateflow_http_request_duration_seconds":       {"service", "route", "le"},
	"gateflow_http_request_bytes_total":            {"service", "route"},
	"gateflow_http_response_bytes_total":           {"service", "route"},
	"gateflow_gateway_active_admitted_requests":    {"service"},
	"gateflow_gateway_eligible_arrivals_total":     {"service"},
	"gateflow_gateway_first_output_seconds":        {"service", "route", "le"},
	"gateflow_http_request_duration_seconds_sum":   {"service", "route"},
	"gateflow_http_request_duration_seconds_count": {"service", "route"},
	"gateflow_gateway_first_output_seconds_sum":    {"service", "route"},
	"gateflow_gateway_first_output_seconds_count":  {"service", "route"},
}

// GET /metrics serves the helper's metrics after a few refused requests, every
// series with bounded labels only: no tenant, model, request id or raw path. The
// refusals reach neither demand signal, and each request has one request line.
func TestRegisterServesMetrics(t *testing.T) {
	logs := &bytes.Buffer{}
	mux := mount(t, logs)
	// Each body names a model but fails its output bound, so no internal call is made.
	const model = "client-model-canary-41d0"
	body := `{"model":"` + model + `","max_tokens":0,"max_output_tokens":0}`
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/messages?debug=raw-path-canary"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("x-api-key", "gk-tenant-canary")
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", rec.Code)
	}
	series := scrape(t, rec.Body.String())
	seen := map[string]bool{}
	for _, s := range series {
		seen[strings.TrimSuffix(s.name, "_bucket")] = true
		checkBounded(t, s, gatewayMetrics[strings.TrimSuffix(s.name, "_bucket")])
	}
	for name := range gatewayMetrics {
		if !seen[name] {
			t.Errorf("/metrics lacks %s", name)
		}
	}
	checkValue(t, series, "gateflow_http_requests_total",
		map[string]string{"route": "/v1/messages", "result": "invalid_request"}, 2)
	checkValue(t, series, "gateflow_http_requests_total",
		map[string]string{"route": "/v1/responses", "result": "invalid_request"}, 1)
	checkValue(t, series, "gateflow_gateway_eligible_arrivals_total", nil, 0)
	checkValue(t, series, "gateflow_gateway_active_admitted_requests", nil, 0)
	if body := rec.Body.String(); strings.Contains(body, model) || strings.Contains(body, "canary") {
		t.Errorf("/metrics holds request input:\n%s", body)
	}
	if lines := strings.Count(logs.String(), `"msg":"request"`); lines != 3 {
		t.Errorf("%d request lines, want 3:\n%s", lines, logs)
	}
	if strings.Count(logs.String(), `"service":"gateway"`) != 3 || strings.Contains(logs.String(), model) {
		t.Errorf("request lines should name the service once each and never the client's model:\n%s", logs)
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
	routes = map[string]bool{"/v1/messages": true, "/v1/responses": true}
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
		case key == "service" && value != "gateway",
			key == "route" && !routes[value],
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
