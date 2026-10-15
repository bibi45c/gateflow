package main

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeStore records every reservation as new and knows no reservation to settle.
type fakeStore struct{}

func (fakeStore) FindReservation(context.Context, string, int32) (api.Reservation, bool, error) {
	return api.Reservation{}, false, nil
}

func (fakeStore) Reserve(_ context.Context, r api.Reservation) (api.Reservation, bool, error) {
	r.ID = "res_1"
	return r, true, nil
}

func (fakeStore) Settle(context.Context, string, api.Settlement) (api.Settled, error) {
	return api.Settled{}, api.ErrReservationNotFound
}

// fakePrices answers every price lookup with the same prices.
type fakePrices struct{}

func (fakePrices) PriceVersion(context.Context, string, string) (billing.Prices, error) {
	return billing.Prices{Input: 2, Output: 5}, nil
}

// The metering metric families and the only labels each may have, le being the
// bucket label of a histogram.
var meteringMetrics = map[string][]string{
	"gateflow_http_requests_total":                 {"service", "route", "result"},
	"gateflow_http_request_duration_seconds":       {"service", "route", "le"},
	"gateflow_http_request_duration_seconds_sum":   {"service", "route"},
	"gateflow_http_request_duration_seconds_count": {"service", "route"},
	"gateflow_http_request_bytes_total":            {"service", "route"},
	"gateflow_http_response_bytes_total":           {"service", "route"},
}

// GET /metrics, without a token, serves the helper's metrics after a few calls, every
// series with bounded labels only: no tenant, reservation, request id or raw path.
// Each call, a successful one included, has one request line.
func TestRoutesServeMetrics(t *testing.T) {
	logs := &bytes.Buffer{}
	observer, err := api.NewObserver(prometheus.NewRegistry(), slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	const token = "metering-token"
	mux := http.NewServeMux()
	routes(api.New(fakeStore{}, fakePrices{}, token, observer), observer)(mux)
	reserve := `{"call_id":"req_call-canary","attempt_id":1,"tenant_id":"tenant-canary","price_version_id":"pv-1",` +
		`"max_input_tokens":100,"max_output_tokens":20}`
	settle := `{"input_tokens":1,"output_tokens":1,"outcome":"completed"}`
	calls := []struct{ path, body, token string }{
		{"/internal/v1/reservations", reserve, token},
		{"/internal/v1/reservations", reserve, "wrong"},
		{"/internal/v1/reservations/res_canary/settle", settle, token},
	}
	for _, c := range calls {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+c.token)
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
		checkBounded(t, s, meteringMetrics[name])
	}
	for name := range meteringMetrics {
		if !seen[name] {
			t.Errorf("/metrics lacks %s", name)
		}
	}
	checkValue(t, all, map[string]string{"route": "/internal/v1/reservations", "result": "completed"}, 1)
	checkValue(t, all, map[string]string{"route": "/internal/v1/reservations", "result": "authentication_denied"}, 1)
	checkValue(t, all, map[string]string{"route": "/internal/v1/reservations/{reservation_id}/settle",
		"result": "invalid_request"}, 1)
	if strings.Contains(rec.Body.String(), "canary") {
		t.Errorf("/metrics holds request input:\n%s", rec.Body)
	}
	if got := strings.Count(logs.String(), `"msg":"request"`); got != len(calls) {
		t.Errorf("%d request lines for %d calls:\n%s", got, len(calls), logs)
	}
	if strings.Count(logs.String(), `"service":"metering"`) != len(calls) || strings.Contains(logs.String(), "canary") {
		t.Errorf("request lines should name the service once each and hold no request input:\n%s", logs)
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
	meteringRoutes = map[string]bool{"/internal/v1/reservations": true,
		"/internal/v1/reservations/{reservation_id}/settle": true}
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
		case key == "service" && value != "metering",
			key == "route" && !meteringRoutes[value],
			key == "result" && !results[value]:
			t.Errorf("%s has %s=%q, outside its fixed set", s.name, key, value)
		}
	}
}

// checkValue fails the test unless the requests_total sample with the labels in
// match has the value want.
func checkValue(t *testing.T, all []series, match map[string]string, want float64) {
	t.Helper()
	for _, s := range all {
		matches := s.name == "gateflow_http_requests_total"
		for key, value := range match {
			matches = matches && s.labels[key] == value
		}
		if matches {
			if s.value != want {
				t.Errorf("requests_total%v = %v, want %v", match, s.value, want)
			}
			return
		}
	}
	t.Errorf("no requests_total sample %v", match)
}
