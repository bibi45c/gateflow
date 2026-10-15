package httpapi

import (
	"io"
	"net/http"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/prometheus/client_golang/prometheus"
)

// Route templates, as the mux patterns name them without the method, and as metrics
// name them. otherRoute is every other path under /internal/v1/.
const (
	verifyRoute       = "/internal/v1/keys/verify"
	priceVersionRoute = "/internal/v1/price-versions/{id}"
	otherRoute        = "/internal/v1/"
)

// NewObserver returns the observer of the API's routes, whose metrics go to registry.
// It writes no request line: the API writes its own, which also names the verified
// key and tenant. No tenant, key, request id or raw path is a metric label.
func NewObserver(registry *prometheus.Registry) (*observability.Observer, error) {
	return observability.New(observability.Options{
		Service:  "access",
		Routes:   []string{verifyRoute, priceVersionRoute, otherRoute},
		Registry: registry,
	})
}

// observe begins the observation of a request on route template.
func (a *API) observe(r *http.Request, template, requestID string) *observability.Request {
	observed, err := a.observer.Begin(observability.RequestInfo{Route: template, Method: r.Method, RequestID: requestID})
	if err != nil {
		// Only an observer that NewObserver did not make lacks the route.
		a.logger.Error("request is not observed", "request_id", requestID, "err", err.Error())
	}
	return observed
}

// codeResults gives each error code its result in the helper's fixed set.
var codeResults = map[string]observability.Result{
	CodeInvalidRequest:        observability.InvalidRequest,
	CodeUnauthorized:          observability.AuthenticationDenied,
	CodeInvalidKey:            observability.AuthenticationDenied,
	CodeModelNotAllowed:       observability.PermissionDenied,
	CodePriceVersionNotFound:  observability.InvalidRequest,
	CodeDependencyUnavailable: observability.DependencyError,
	CodeInternalError:         observability.InternalError,
}

// resultOf returns the result of a request: internal_error after a panic, else that
// of the error code it was answered with, completed for a 2xx answer, and
// invalid_request for the 404 of a path that is no route.
func resultOf(code string, status int, panicked bool) observability.Result {
	result, coded := codeResults[code]
	switch {
	case panicked:
		return observability.InternalError
	case coded:
		return result
	case status >= 200 && status <= 299:
		return observability.Completed
	case status == http.StatusNotFound:
		return observability.InvalidRequest
	}
	return observability.InternalError
}

// summary is the terminal metadata of a request.
func summary(result observability.Result, w *statusWriter, body *countingBody) observability.Summary {
	return observability.Summary{Result: result, HTTPStatus: w.status, RequestBytes: body.n, ResponseBytes: w.bytes}
}

// countingBody counts the request body bytes that the handler reads, as the helper
// takes them from its caller.
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
