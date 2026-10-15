// Package observability provides explicit request timing and counting hooks.
// Callers decide eligibility, admission, first model output and terminal outcomes.
// It never reads bodies, credentials or business state, or wraps ResponseWriter.
package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Result is a bounded terminal outcome, independent of the HTTP status.
type Result string

// A call that was admitted ends with one of the five outcomes of the settle contract
// (the Outcome enum of api/metering.yaml), under the same names. A call that times
// out after admission is Interrupted, as that contract's first-match rule has it.
const (
	Completed       Result = "completed"
	NotDispatched   Result = "not_dispatched"
	UpstreamError   Result = "upstream_error"
	ClientCancelled Result = "client_cancelled"
	Interrupted     Result = "interrupted"
)

// A call that is refused or fails before admission ends with one of these.
const (
	InvalidRequest       Result = "invalid_request"       // malformed or too large
	AuthenticationDenied Result = "authentication_denied" // missing or unknown key or token
	PermissionDenied     Result = "permission_denied"     // a valid key that may not do this
	InsufficientBalance  Result = "insufficient_balance"  // the cost could not be held
	TenantLimited        Result = "tenant_limited"        // over the tenant's own limit
	CapacityRejected     Result = "capacity_rejected"     // eligible, but no capacity
	DependencyError      Result = "dependency_error"      // another service or store is unavailable
	InternalError        Result = "internal_error"        // a fault of this service
)

var results = []Result{Completed, NotDispatched, UpstreamError, ClientCancelled, Interrupted,
	InvalidRequest, AuthenticationDenied, PermissionDenied, InsufficientBalance, TenantLimited,
	CapacityRejected, DependencyError, InternalError}

var (
	requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	aliasPattern     = regexp.MustCompile(`^[a-zA-Z0-9._:/-]{1,64}$`)
	latencyBuckets   = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10}
)

// Options contains process configuration, never values copied from HTTP input.
// Registry nil disables metrics; Logger nil disables request logs. GatewaySignals
// enables the two demand signals and first-output histogram for a gateway process.
type Options struct {
	Service        string
	Routes         []string
	ModelAliases   []string
	Registry       *prometheus.Registry
	Logger         *slog.Logger
	GatewaySignals bool
}

type routeMetrics struct {
	requests      map[Result]prometheus.Counter
	duration      prometheus.Observer
	requestBytes  prometheus.Counter
	responseBytes prometheus.Counter
	firstOutput   prometheus.Observer
}

// Observer owns a fixed route set and an explicit registry. Independent tests and
// processes must use separate registries; the default global registry is not used.
type Observer struct {
	service  string
	routes   map[string]*routeMetrics
	models   map[string]bool
	registry *prometheus.Registry
	logger   *slog.Logger
	active   prometheus.Gauge
	eligible prometheus.Counter
}

// New validates configuration and registers only the prototype's metric subset.
// No tenant, model, request ID or raw path is a metric label.
func New(opts Options) (*Observer, error) {
	if opts.Service != "gateway" && opts.Service != "access" && opts.Service != "metering" && opts.Service != "mock-upstream" {
		return nil, errors.New("unknown configured service")
	}
	if len(opts.Routes) == 0 || len(opts.Routes) > 16 || len(opts.ModelAliases) > 16 {
		return nil, errors.New("route or model alias configuration exceeds bounds")
	}
	if opts.GatewaySignals && opts.Service != "gateway" {
		return nil, errors.New("gateway signals require the gateway service")
	}
	o := &Observer{service: opts.Service, routes: make(map[string]*routeMetrics),
		models: make(map[string]bool), registry: opts.Registry}
	if opts.Logger != nil {
		o.logger = opts.Logger.With("service", opts.Service)
	}
	if err := o.configure(opts.Routes, opts.ModelAliases); err != nil {
		return nil, err
	}
	if opts.Registry == nil {
		return o, nil
	}
	set := newCollectors(opts.Service, opts.GatewaySignals)
	if err := registerAll(opts.Registry, set.all()); err != nil {
		return nil, err
	}
	o.bind(set)
	return o, nil
}

// configure records the route templates and model aliases the observer accepts.
func (o *Observer) configure(routes, aliases []string) error {
	for _, route := range routes {
		if len(route) > 128 || !strings.HasPrefix(route, "/") || strings.ContainsAny(route, "?#\r\n\t ") {
			return errors.New("invalid configured route template")
		}
		if _, exists := o.routes[route]; exists {
			return errors.New("duplicate configured route")
		}
		o.routes[route] = &routeMetrics{}
	}
	for _, model := range aliases {
		if !aliasPattern.MatchString(model) {
			return errors.New("invalid configured model alias")
		}
		o.models[model] = true
	}
	return nil
}

// collectors are the metric families of one observer, before any is registered. The
// last three are nil unless the gateway signals are enabled.
type collectors struct {
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	requestBytes  *prometheus.CounterVec
	responseBytes *prometheus.CounterVec
	active        prometheus.Gauge
	eligible      prometheus.Counter
	firstOutput   *prometheus.HistogramVec
}

func newCollectors(service string, gatewaySignals bool) collectors {
	labels := prometheus.Labels{"service": service}
	set := collectors{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateflow_http_requests_total", Help: "Terminal requests by explicitly reported result.", ConstLabels: labels}, []string{"route", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateflow_http_request_duration_seconds", Help: "Observed request lifetime, including any work before Finish.",
			ConstLabels: labels, Buckets: append(append([]float64{}, latencyBuckets...), 30, 60, 90, 120)}, []string{"route"}),
		requestBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateflow_http_request_bytes_total", Help: "Application request bytes reported by the caller.", ConstLabels: labels}, []string{"route"}),
		responseBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateflow_http_response_bytes_total", Help: "Successfully written application response bytes reported by the caller.", ConstLabels: labels}, []string{"route"}),
	}
	if gatewaySignals {
		set.active = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gateflow_gateway_active_admitted_requests",
			Help: "Admitted model requests, including those waiting for first output.", ConstLabels: labels})
		set.eligible = prometheus.NewCounter(prometheus.CounterOpts{Name: "gateflow_gateway_eligible_arrivals_total",
			Help: "Eligible arrivals reported before capacity rejection, excluding invalid and tenant-limited traffic.", ConstLabels: labels})
		set.firstOutput = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "gateflow_gateway_first_output_seconds",
			Help:        "Receipt to explicitly reported first model output, excluding metadata and keep-alives.",
			ConstLabels: labels, Buckets: latencyBuckets}, []string{"route"})
	}
	return set
}

// all lists the collectors in registration order.
func (c collectors) all() []prometheus.Collector {
	list := []prometheus.Collector{c.requests, c.duration, c.requestBytes, c.responseBytes}
	if c.firstOutput != nil {
		list = append(list, c.active, c.eligible, c.firstOutput)
	}
	return list
}

// registerAll registers every collector or none: after a failure it takes back the
// ones it had added, so the registry is left as it was.
func registerAll(registry *prometheus.Registry, all []prometheus.Collector) error {
	var registered []prometheus.Collector
	for _, collector := range all {
		if err := registry.Register(collector); err != nil {
			for _, previous := range registered {
				registry.Unregister(previous)
			}
			return err
		}
		registered = append(registered, collector)
	}
	return nil
}

// bind gives every configured route its own counters and histograms, one counter
// for each result, so a result that never happened still reads as a real zero.
func (o *Observer) bind(c collectors) {
	o.active, o.eligible = c.active, c.eligible
	for route, metrics := range o.routes {
		metrics.requests = make(map[Result]prometheus.Counter)
		for _, result := range results {
			metrics.requests[result] = c.requests.WithLabelValues(route, string(result))
		}
		metrics.duration = c.duration.WithLabelValues(route)
		metrics.requestBytes = c.requestBytes.WithLabelValues(route)
		metrics.responseBytes = c.responseBytes.WithLabelValues(route)
		if c.firstOutput != nil {
			metrics.firstOutput = c.firstOutput.WithLabelValues(route)
		}
	}
}

// RequestInfo is metadata only. TenantSlot must come from verified identity; this
// package bounds its representation, but does not authenticate it. ModelAlias must
// be a configured alias. Untrusted or unrecognized strings are never logged raw.
type RequestInfo struct {
	Route      string
	Method     string
	RequestID  string
	TenantSlot string
	ModelAlias string
}

// Summary reports terminal metadata. HTTP 200 alone does not mean SSE success.
// Bytes are supplied by the caller, so observation never consumes or buffers data.
type Summary struct {
	Result        Result
	HTTPStatus    int
	RequestBytes  int64
	ResponseBytes int64
}

// Request is a concurrency-safe lifecycle handle. Finish wins once; late hooks
// have no effect. A nil handle is a no-op, used by the uninstrumented baseline.
type Request struct {
	mu          sync.Mutex
	owner       *Observer
	metrics     *routeMetrics
	info        RequestInfo
	started     time.Time
	firstOutput time.Duration
	seenFirst   bool
	admitted    bool
	eligible    bool
	finished    bool
}

// Begin starts timing at receipt. Eligibility and admission require explicit calls.
func (o *Observer) Begin(info RequestInfo) (*Request, error) {
	if o == nil {
		return nil, nil
	}
	started := time.Now()
	metrics, exists := o.routes[info.Route]
	if !exists {
		return nil, errors.New("route is not configured")
	}
	switch info.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		info.Method = "OTHER"
	}
	if !requestIDPattern.MatchString(info.RequestID) {
		info.RequestID = ""
	}
	if !o.models[info.ModelAlias] {
		info.ModelAlias = "other"
	}
	if info.TenantSlot == "" {
		info.TenantSlot = "unverified"
	} else if !knownTenantSlot(info.TenantSlot) {
		info.TenantSlot = "other"
	}
	return &Request{owner: o, metrics: metrics, info: info, started: started}, nil
}

// SetVerifiedTenantSlot allows a receipt-time handle to acquire tenant metadata
// after authentication, without restarting timing. The caller must verify identity;
// this method only bounds its diagnostic representation.
func (r *Request) SetVerifiedTenantSlot(slot string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		if knownTenantSlot(slot) {
			r.info.TenantSlot = slot
		} else {
			r.info.TenantSlot = "other"
		}
	}
}

func knownTenantSlot(slot string) bool {
	if slot == "other" || slot == "unverified" {
		return true
	}
	return slot == "tenant-10" || (len(slot) == 9 && strings.HasPrefix(slot, "tenant-0") && slot[8] >= '0' && slot[8] <= '9')
}

// MarkEligible records eligible arrival demand exactly once. Call before a
// capacity decision, including when that decision rejects this request.
func (r *Request) MarkEligible() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished && !r.eligible {
		r.eligible = true
		if r.owner.eligible != nil {
			r.owner.eligible.Inc()
		}
	}
}

// MarkAdmitted includes the time waiting for first output. Finish releases it.
func (r *Request) MarkAdmitted() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished && !r.admitted {
		r.admitted = true
		if r.owner.active != nil {
			r.owner.active.Inc()
		}
	}
}

// MarkFirstOutput is called after real model output has been written and flushed,
// never for headers, metadata or keep-alives. The caller owns protocol recognition.
func (r *Request) MarkFirstOutput() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished && !r.seenFirst {
		r.seenFirst = true
		r.firstOutput = time.Since(r.started)
		if r.metrics.firstOutput != nil {
			r.metrics.firstOutput.Observe(r.firstOutput.Seconds())
		}
	}
}

// Finish records one result and releases admission, even when called concurrently.
// Defer Finish(Interrupted) as cleanup, then finish with an explicit terminal result
// on the normal/error paths. Invalid results/counts are recorded as interrupted.
func (r *Request) Finish(summary Summary) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return false
	}
	r.finished = true
	duration := time.Since(r.started)
	valid := false
	for _, result := range results {
		if result == summary.Result {
			valid = true
			break
		}
	}
	if !valid {
		summary.Result = Interrupted
	}
	if summary.RequestBytes < 0 {
		summary.RequestBytes = 0
		summary.Result = Interrupted
	}
	if summary.ResponseBytes < 0 {
		summary.ResponseBytes = 0
		summary.Result = Interrupted
	}
	if summary.HTTPStatus < 100 || summary.HTTPStatus > 599 {
		summary.HTTPStatus = 0
	}
	if r.admitted && r.owner.active != nil {
		r.owner.active.Dec()
	}
	if r.metrics.duration != nil {
		r.metrics.requests[summary.Result].Inc()
		r.metrics.duration.Observe(duration.Seconds())
		r.metrics.requestBytes.Add(float64(summary.RequestBytes))
		r.metrics.responseBytes.Add(float64(summary.ResponseBytes))
	}
	info, first, seen := r.info, r.firstOutput, r.seenFirst
	r.mu.Unlock()
	if r.owner.logger != nil {
		attrs := []slog.Attr{slog.String("method", info.Method), slog.String("route", info.Route),
			slog.Int("status", summary.HTTPStatus), slog.String("result", string(summary.Result)),
			slog.Float64("duration_ms", float64(duration)/float64(time.Millisecond)),
			slog.Int64("request_bytes", summary.RequestBytes), slog.Int64("response_bytes", summary.ResponseBytes),
			slog.String("tenant_slot", info.TenantSlot), slog.String("model", info.ModelAlias)}
		if info.RequestID != "" {
			attrs = append(attrs, slog.String("request_id", info.RequestID))
		}
		if seen {
			attrs = append(attrs, slog.Float64("first_output_ms", float64(first)/float64(time.Millisecond)))
		}
		// The message and field names are those of the request lines the services
		// already write, so adopting the helper adds fields without renaming any.
		r.owner.logger.LogAttrs(context.Background(), slog.LevelInfo, "request", attrs...)
	}
	return true
}

// MetricsHandler is for an internal listener or test mux, never the public model
// ingress. It is nil in logs-only mode. It is not instrumented as a model request.
func (o *Observer) MetricsHandler() http.Handler {
	if o == nil || o.registry == nil {
		return nil
	}
	return promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{})
}
