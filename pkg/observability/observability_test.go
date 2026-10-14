package observability

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestObserver(t *testing.T, logs io.Writer) (*Observer, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	var logger *slog.Logger
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	o, err := New(Options{Service: "gateway", Routes: []string{"/probe"}, ModelAliases: []string{"probe"},
		Registry: registry, Logger: logger, GatewaySignals: true})
	if err != nil {
		t.Fatal(err)
	}
	return o, registry
}

func beginTest(t *testing.T, o *Observer) *Request {
	t.Helper()
	r, err := o.Begin(RequestInfo{Route: "/probe", Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func histogramCount(t *testing.T, registry *prometheus.Registry, name string) uint64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			var count uint64
			for _, metric := range family.Metric {
				count += metric.GetHistogram().GetSampleCount()
			}
			return count
		}
	}
	t.Fatalf("missing metric %s", name)
	return 0
}

func TestDemandStartsAtZeroAndIncludesWaitingRequests(t *testing.T) {
	o, registry := newTestObserver(t, nil)
	if testutil.ToFloat64(o.active) != 0 || testutil.ToFloat64(o.eligible) != 0 {
		t.Fatal("demand must initialize as real zero")
	}
	r := beginTest(t, o)
	r.MarkEligible()
	r.MarkEligible()
	r.MarkAdmitted()
	r.MarkAdmitted()
	if testutil.ToFloat64(o.active) != 1 || testutil.ToFloat64(o.eligible) != 1 {
		t.Fatal("waiting request or duplicate hooks miscounted")
	}
	if got := histogramCount(t, registry, "gateflow_gateway_first_output_seconds"); got != 0 {
		t.Fatalf("invented first output: %d", got)
	}
	r.Finish(Summary{Result: ClientCancelled, HTTPStatus: 0})
	if testutil.ToFloat64(o.active) != 0 {
		t.Fatal("cancelled request remains active")
	}
	if got := histogramCount(t, registry, "gateflow_gateway_first_output_seconds"); got != 0 {
		t.Fatalf("no-output failure produced a sample: %d", got)
	}
}

func TestCapacityRejectionIsEligibleButNotAdmitted(t *testing.T) {
	o, _ := newTestObserver(t, nil)
	r := beginTest(t, o)
	r.MarkEligible()
	r.Finish(Summary{Result: CapacityRejected, HTTPStatus: 503})
	if testutil.ToFloat64(o.eligible) != 1 || testutil.ToFloat64(o.active) != 0 {
		t.Fatal("capacity rejection disappeared from demand")
	}
	unverified := beginTest(t, o)
	unverified.Finish(Summary{Result: AuthenticationDenied, HTTPStatus: 401})
	limited := beginTest(t, o)
	limited.Finish(Summary{Result: TenantLimited, HTTPStatus: 429})
	if testutil.ToFloat64(o.eligible) != 1 {
		t.Fatal("ineligible traffic counted")
	}
}

func TestCompetingTerminalPathsRecordOneOutcomeAndReleaseOnce(t *testing.T) {
	var logs bytes.Buffer
	o, registry := newTestObserver(t, &logs)
	r := beginTest(t, o)
	r.MarkAdmitted()
	r.MarkEligible()
	var wins atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r.MarkAdmitted()
			r.MarkEligible()
			r.MarkFirstOutput()
			if r.Finish(Summary{Result: Completed, HTTPStatus: 200, RequestBytes: 7, ResponseBytes: 9}) {
				wins.Add(1)
			}
		}()
	}
	group.Wait()
	if wins.Load() != 1 || testutil.ToFloat64(o.active) != 0 || testutil.ToFloat64(o.eligible) != 1 {
		t.Fatalf("wins=%d, demand not released exactly once", wins.Load())
	}
	if testutil.ToFloat64(r.metrics.requests[Completed]) != 1 || testutil.ToFloat64(r.metrics.responseBytes) != 9 {
		t.Fatal("duplicate result or byte count")
	}
	if histogramCount(t, registry, "gateflow_gateway_first_output_seconds") != 1 || histogramCount(t, registry, "gateflow_http_request_duration_seconds") != 1 {
		t.Fatal("duplicate timing")
	}
	if strings.Count(logs.String(), `"msg":"request"`) != 1 {
		t.Fatal("duplicate terminal log")
	}
	r.MarkAdmitted()
	r.MarkEligible()
	r.MarkFirstOutput()
	if testutil.ToFloat64(o.active) != 0 || testutil.ToFloat64(o.eligible) != 1 {
		t.Fatal("late hook resurrected finished request")
	}
}

func TestHTTP200CanFinishWithStreamError(t *testing.T) {
	var logs bytes.Buffer
	o, _ := newTestObserver(t, &logs)
	r := beginTest(t, o)
	r.MarkAdmitted()
	r.Finish(Summary{Result: UpstreamError, HTTPStatus: 200})
	if testutil.ToFloat64(r.metrics.requests[Completed]) != 0 || testutil.ToFloat64(r.metrics.requests[UpstreamError]) != 1 {
		t.Fatal("HTTP200 misclassified")
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["result"] != "upstream_error" || entry["status"] != float64(200) {
		t.Fatalf("wrong log: %v", entry)
	}
}

// contractOutcomes reads the Outcome enum of the settle contract.
func contractOutcomes(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "api", "metering.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	inOutcome, inEnum := false, false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		item := strings.TrimSpace(line)
		switch {
		case line == "    Outcome:":
			inOutcome = true
		case inOutcome && item == "enum:":
			inEnum = true
		case inEnum && strings.HasPrefix(item, "- "):
			names = append(names, strings.TrimPrefix(item, "- "))
		case inEnum, inOutcome && line != "" && !strings.HasPrefix(line, "      "):
			return names
		}
	}
	return names
}

func TestAdmittedResultsAreTheOutcomesOfTheSettleContract(t *testing.T) {
	admitted := map[string]bool{}
	for _, result := range []Result{Completed, NotDispatched, UpstreamError, ClientCancelled, Interrupted} {
		admitted[string(result)] = true
	}
	outcomes := contractOutcomes(t)
	if len(outcomes) != len(admitted) {
		t.Fatalf("the contract has outcomes %v, the helper has %v", outcomes, admitted)
	}
	for _, outcome := range outcomes {
		if !admitted[outcome] {
			t.Errorf("contract outcome %q has no result of the same name", outcome)
		}
	}

	// Every result is a distinct, known label value with its own counter from the start.
	o, registry := newTestObserver(t, nil)
	seen := map[Result]bool{}
	for _, result := range results {
		if seen[result] {
			t.Errorf("result %q is listed twice", result)
		}
		seen[result] = true
		if _, exists := o.routes["/probe"].requests[result]; !exists {
			t.Errorf("result %q has no counter", result)
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "gateflow_http_requests_total" && len(family.Metric) != len(results) {
			t.Errorf("series: got %d, want one for each of the %d results", len(family.Metric), len(results))
		}
	}
}

func TestRequestLineUsesTheNamesTheServicesAlreadyWrite(t *testing.T) {
	var logs bytes.Buffer
	o, _ := newTestObserver(t, &logs)
	r, err := o.Begin(RequestInfo{Route: "/probe", Method: http.MethodPost, ModelAlias: "probe",
		RequestID: "req_00000000-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	r.Finish(Summary{Result: Completed, HTTPStatus: 200, RequestBytes: 7, ResponseBytes: 9})
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	// The names of the gateway's and access's own request lines, then what the helper adds.
	for _, name := range []string{"method", "route", "status", "duration_ms", "model", "request_bytes",
		"response_bytes", "request_id", "service", "result", "tenant_slot"} {
		if _, exists := entry[name]; !exists {
			t.Errorf("the request line has no %q field: %v", name, entry)
		}
	}
	if entry["msg"] != "request" || entry["status"] != float64(200) || entry["result"] != "completed" {
		t.Errorf("request line: got %v", entry)
	}
	if _, exists := entry["http_status"]; exists {
		t.Error("the request line still has the old http_status field")
	}
}

func TestUnsafeMetadataNeverBecomesRawLogsOrLabels(t *testing.T) {
	const canary = "secret-prompt-credential-CANARY"
	var logs bytes.Buffer
	o, registry := newTestObserver(t, &logs)
	for i := 0; i < 100; i++ {
		r, err := o.Begin(RequestInfo{Route: "/probe", Method: canary, RequestID: canary, TenantSlot: canary, ModelAlias: canary})
		if err != nil {
			t.Fatal(err)
		}
		r.Finish(Summary{Result: Result(canary), HTTPStatus: 999, RequestBytes: -1, ResponseBytes: -1})
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatal("sensitive metadata leaked")
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["tenant_slot"] != "other" || entry["model"] != "other" || entry["result"] != "interrupted" {
			t.Fatalf("unsafe metadata not bounded: %v", entry)
		}
		if _, exists := entry["request_id"]; exists {
			t.Fatal("invalid request ID recorded")
		}
		if _, exists := entry["first_output_ms"]; exists {
			t.Fatal("missing output represented as zero")
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "service" && label.GetName() != "route" && label.GetName() != "result" {
					t.Fatalf("unbounded label %s", label.GetName())
				}
				if strings.Contains(label.GetValue(), canary) {
					t.Fatal("label leak")
				}
			}
		}
	}
	if _, err := o.Begin(RequestInfo{Route: "/probe/" + canary}); err == nil {
		t.Fatal("dynamic route accepted")
	}
}

func TestTenantSlotsAreExactlyTheConfiguredExperimentSet(t *testing.T) {
	for _, slot := range []string{"tenant-00", "tenant-01", "tenant-09", "tenant-10", "other", "unverified"} {
		if !knownTenantSlot(slot) {
			t.Errorf("valid slot %s rejected", slot)
		}
	}
	for _, slot := range []string{"tenant-0X", "tenant-0:", "tenant-11", "tenant-99", "tenant-000", "raw-tenant"} {
		if knownTenantSlot(slot) {
			t.Errorf("unbounded slot %s accepted", slot)
		}
	}
}

func TestIndependentRegistriesAndScrapesDoNotChangeDemand(t *testing.T) {
	first, _ := newTestObserver(t, nil)
	second, _ := newTestObserver(t, nil)
	r := beginTest(t, first)
	r.MarkEligible()
	r.MarkAdmitted()
	for i := 0; i < 3; i++ {
		response := httptest.NewRecorder()
		first.MetricsHandler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), "gateflow_gateway_active_admitted_requests") {
			t.Fatal("scrape failed")
		}
	}
	if testutil.ToFloat64(first.eligible) != 1 || testutil.ToFloat64(second.eligible) != 0 || testutil.ToFloat64(second.active) != 0 {
		t.Fatal("scrapes counted or registries shared")
	}
	r.Finish(Summary{Result: Completed})
}

func TestLogsOnlyAndNilBaseline(t *testing.T) {
	var logs bytes.Buffer
	o, err := New(Options{Service: "gateway", Routes: []string{"/probe"}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if o.MetricsHandler() != nil {
		t.Fatal("logs only unexpectedly exposes metrics")
	}
	r := beginTest(t, o)
	r.MarkAdmitted()
	r.MarkFirstOutput()
	r.Finish(Summary{Result: Completed, HTTPStatus: 200})
	if !strings.Contains(logs.String(), "first_output_ms") {
		t.Fatal("logs only timing missing")
	}
	var baseline *Observer
	none, err := baseline.Begin(RequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	none.MarkEligible()
	none.MarkAdmitted()
	none.MarkFirstOutput()
	if none.Finish(Summary{}) {
		t.Fatal("nil baseline claims observation")
	}
}

func TestDeferredCleanupReleasesAdmissionWithoutLoggingPanicContent(t *testing.T) {
	var logs bytes.Buffer
	o, _ := newTestObserver(t, &logs)
	func() {
		defer func() { _ = recover() }()
		r := beginTest(t, o)
		defer r.Finish(Summary{Result: Interrupted})
		r.MarkAdmitted()
		panic("sensitive-panic-canary")
	}()
	if testutil.ToFloat64(o.active) != 0 || strings.Contains(logs.String(), "sensitive-panic-canary") {
		t.Fatal("panic cleanup leaked or admission stuck")
	}
}

func TestConfigurationAndRegistrationFailuresLeaveRegistryUnchanged(t *testing.T) {
	for _, opts := range []Options{
		{Service: "unknown", Routes: []string{"/probe"}},
		{Service: "gateway"},
		{Service: "gateway", Routes: []string{"/probe", "/probe"}},
		{Service: "gateway", Routes: []string{"/probe?secret=yes"}},
		{Service: "access", Routes: []string{"/probe"}, GatewaySignals: true},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("invalid options accepted: %+v", opts)
		}
	}
	registry := prometheus.NewRegistry()
	prior := prometheus.NewGauge(prometheus.GaugeOpts{Name: "gateflow_http_request_duration_seconds", Help: "Existing collector."})
	registry.MustRegister(prior)
	prior.Set(3)
	_, err := New(Options{Service: "gateway", Routes: []string{"/probe"}, Registry: registry})
	if err == nil {
		t.Fatal("conflicting registry accepted")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || testutil.ToFloat64(prior) != 3 {
		t.Fatal("partially registered or changed previous collector")
	}
}

func BenchmarkRequestHooks(b *testing.B) {
	for _, mode := range []string{"baseline", "json", "metrics"} {
		b.Run(mode, func(b *testing.B) {
			var o *Observer
			if mode != "baseline" {
				opts := Options{Service: "gateway", Routes: []string{"/probe"}, ModelAliases: []string{"probe"},
					Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), GatewaySignals: true}
				if mode == "metrics" {
					opts.Registry = prometheus.NewRegistry()
				}
				var err error
				o, err = New(opts)
				if err != nil {
					b.Fatal(err)
				}
			}
			info := RequestInfo{Route: "/probe", Method: "POST", ModelAlias: "probe", TenantSlot: "tenant-01",
				RequestID: "req_00000000-0000-4000-8000-000000000001"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, err := o.Begin(info)
				if err != nil {
					b.Fatal(err)
				}
				r.MarkEligible()
				r.MarkAdmitted()
				r.MarkFirstOutput()
				r.Finish(Summary{Result: Completed, HTTPStatus: 200, RequestBytes: 1024, ResponseBytes: 64})
			}
		})
	}
}
