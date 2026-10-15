package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/metering/internal/accessclient"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
	"github.com/prometheus/client_golang/prometheus"
)

// newObservedService is newService with an observer, whose registry and request
// lines it returns.
func newObservedService(t *testing.T, wallets map[string]int64, prices PriceSource) (*service, *prometheus.Registry, *bytes.Buffer) {
	t.Helper()
	registry, logs := prometheus.NewRegistry(), &bytes.Buffer{}
	observer, err := NewObserver(registry, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s := &service{
		mux:    http.NewServeMux(),
		store:  newFakeStore(wallets),
		prices: &fakePrices{versions: map[string]billing.Prices{"pv-1": {Input: 2, Output: 5}}},
	}
	if prices == nil {
		prices = s.prices
	}
	New(s.store, prices, token, observer).Register(s.mux)
	return s, registry, logs
}

// requests returns the requests_total of route and result.
func requests(t *testing.T, registry *prometheus.Registry, route, result string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if family.GetName() == "gateflow_http_requests_total" && labels["route"] == route && labels["result"] == result {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no requests_total for %s %s", route, result)
	return 0
}

// reserveFor is the fixture reserve with another call id and tenant.
func reserveFor(callID, tenant string) string {
	return strings.Replace(strings.Replace(fixtureReserve, "req_1", callID, 1), "tenant-01", tenant, 1)
}

// Each call is counted under its route template with the result of the error code it
// was answered with, a replay being completed, and has one request line, a call that
// succeeds included.
func TestCallsAreCountedByResult(t *testing.T) {
	s, registry, logs := newObservedService(t, map[string]int64{"tenant-01": 300, "tenant-02": 1000}, nil)
	id := s.reserved(t)
	settled := `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`
	calls := []struct {
		path, bearer, body string
		priceErr           error
		status             int
	}{
		{reservePath, "Bearer " + token, fixtureReserve, nil, http.StatusOK},
		{reservePath, "Bearer wrong", reserveFor("req_2", "tenant-01"), nil, http.StatusUnauthorized},
		{reservePath, "Bearer " + token, `{}`, nil, http.StatusBadRequest},
		{reservePath, "Bearer " + token, reserveFor("req_1", "tenant-02"), nil, http.StatusUnprocessableEntity},
		{reservePath, "Bearer " + token, reserveFor("req_3", "tenant-99"), nil, http.StatusNotFound},
		{reservePath, "Bearer " + token, reserveFor("req_4", "tenant-01"), nil, http.StatusConflict},
		{reservePath, "Bearer " + token, reserveFor("req_5", "tenant-02"), accessclient.ErrUnavailable,
			http.StatusServiceUnavailable},
		{reservePath, "Bearer " + token, reserveFor("req_6", "tenant-02"), accessclient.ErrRejected,
			http.StatusInternalServerError},
		{settlePath(id), "Bearer " + token, settled, nil, http.StatusOK},
		{settlePath("res_missing"), "Bearer " + token, settled, nil, http.StatusNotFound},
	}
	for _, c := range calls {
		s.prices.err = c.priceErr
		if got := s.send(c.path, c.bearer, c.body); got.status != c.status {
			t.Errorf("%s %s: status %d, want %d", c.path, c.body, got.status, c.status)
		}
	}

	want := []struct {
		route, result string
		count         float64
	}{
		{reserveRoute, "completed", 2},
		{reserveRoute, "authentication_denied", 1},
		{reserveRoute, "invalid_request", 2},
		{reserveRoute, "insufficient_balance", 2},
		{reserveRoute, "dependency_error", 1},
		{reserveRoute, "internal_error", 1},
		{settleRoute, "completed", 1},
		{settleRoute, "invalid_request", 1},
	}
	for _, w := range want {
		if got := requests(t, registry, w.route, w.result); got != w.count {
			t.Errorf("requests{route=%q,result=%q} = %v, want %v", w.route, w.result, got, w.count)
		}
	}
	if got := strings.Count(logs.String(), `"msg":"request"`); got != 11 {
		t.Errorf("%d request lines for 11 calls:\n%s", got, logs)
	}
	if got := strings.Count(logs.String(), `"result":"completed"`); got != 3 {
		t.Errorf("%d request lines of completed calls, want 3:\n%s", got, logs)
	}
	if strings.Contains(logs.String(), "tenant-0") || strings.Contains(logs.String(), token) {
		t.Errorf("the request lines hold a tenant id or the token:\n%s", logs)
	}
}

// panickingPrices fails every price lookup with a panic.
type panickingPrices struct{}

func (panickingPrices) PriceVersion(context.Context, string, string) (billing.Prices, error) {
	panic("price lookup failed")
}

// A panic in an operation reaches the server, and the call is still counted, as a
// fault of the service.
func TestPanicIsCountedAsInternalError(t *testing.T) {
	s, registry, _ := newObservedService(t, map[string]int64{"tenant-01": 1000}, panickingPrices{})
	request := httptest.NewRequest(http.MethodPost, reservePath, strings.NewReader(fixtureReserve))
	request.Header.Set("Authorization", "Bearer "+token)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the server")
			}
		}()
		s.mux.ServeHTTP(httptest.NewRecorder(), request)
	}()
	if got := requests(t, registry, reserveRoute, "internal_error"); got != 1 {
		t.Errorf("requests{result=internal_error} = %v, want 1", got)
	}
}
