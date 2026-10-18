package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/prometheus/client_golang/prometheus"
)

// Route templates of the two operations, as the mux patterns name them without the
// method, and as metrics and request lines name them.
const (
	reserveRoute = "/internal/v1/reservations"
	settleRoute  = "/internal/v1/reservations/{reservation_id}/settle"
)

// NewObserver returns the observer of the two operations. Its metrics go to registry
// and one request line per call to logger; either may be nil. No tenant, reservation,
// request id or raw path is a metric label.
func NewObserver(registry *prometheus.Registry, logger *slog.Logger) (*observability.Observer, error) {
	return observability.New(observability.Options{
		Service:  "metering",
		Routes:   []string{reserveRoute, settleRoute, walletsRoute, usageRoute, adjustmentsRoute},
		Registry: registry,
		Logger:   logger,
	})
}

// callState is what a call's observation needs and only later steps know.
type callState struct {
	code errorCode // the error code answered, if any
}

type callKey struct{}

// observed wraps an operation with its observation under route template. It runs
// inside identified, so the request line has the call's request id when that id has
// the gateway's req_<uuid> form.
func (h *Handler) observed(template string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		observed, err := h.observer.Begin(observability.RequestInfo{
			Route: template, Method: r.Method, RequestID: requestID(r)})
		if err != nil {
			// Only an observer that NewObserver did not make lacks the route.
			slog.Error("metering request is not observed", "request_id", requestID(r), "err", err.Error())
		}
		call := &callState{}
		r = r.WithContext(context.WithValue(r.Context(), callKey{}, call))
		body := &countingBody{ReadCloser: r.Body}
		r.Body = body // on the copy WithContext made, not the server's request
		sw := &statusWriter{ResponseWriter: w}
		// A panic, which the server sees, ends the call as a fault of the service.
		defer func() { observed.Finish(summary(observability.InternalError, sw, body)) }()
		next(sw, r)
		observed.Finish(summary(resultOf(call.code, sw.status), sw, body))
	}
}

// codeResults gives each error code its result in the helper's fixed set. A tenant
// without a wallet cannot have the cost held, as the gateway also reports it.
var codeResults = map[errorCode]observability.Result{
	codeInvalidRequest:        observability.InvalidRequest,
	codeUnauthorized:          observability.AuthenticationDenied,
	codeWalletNotFound:        observability.InsufficientBalance,
	codeReservationNotFound:   observability.InvalidRequest,
	codeInsufficientBalance:   observability.InsufficientBalance,
	codeIdempotencyConflict:   observability.InvalidRequest,
	codeInternalError:         observability.InternalError,
	codeDependencyUnavailable: observability.DependencyError,
}

// resultOf returns the result of a call: that of the error code it was answered with,
// completed for a 2xx answer, a replay included, and internal_error otherwise.
func resultOf(code errorCode, status int) observability.Result {
	if result, coded := codeResults[code]; coded {
		return result
	}
	if status >= 200 && status <= 299 {
		return observability.Completed
	}
	return observability.InternalError
}

// summary is the terminal metadata of a call.
func summary(result observability.Result, w *statusWriter, body *countingBody) observability.Summary {
	return observability.Summary{Result: result, HTTPStatus: w.status, RequestBytes: body.n, ResponseBytes: w.bytes}
}

// statusWriter remembers the status an operation wrote and counts the bytes, which
// the helper takes from its caller.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// countingBody counts the request body bytes that the operation reads.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.ReadCloser == nil {
		return 0, io.EOF
	}
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

func (b *countingBody) Close() error {
	if b.ReadCloser == nil {
		return nil
	}
	return b.ReadCloser.Close()
}
