package relay

import (
	"log/slog"
	"net/http"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/prometheus/client_golang/prometheus"
)

// NewObserver returns the observer of the two endpoints, under the route templates
// /v1/messages and /v1/responses, with the gateway's demand signals and first-output
// histogram. Its metrics go to registry and its request lines to logger; either may
// be nil. A request's model is logged as it is only when modelAliases lists it, and as
// other otherwise. No tenant, model or request id is a metric label.
func NewObserver(registry *prometheus.Registry, logger *slog.Logger, modelAliases []string) (*observability.Observer, error) {
	return observability.New(observability.Options{
		Service:        "gateway",
		Routes:         []string{messages{}.path(), responses{}.path()},
		ModelAliases:   modelAliases,
		Registry:       registry,
		Logger:         logger,
		GatewaySignals: true,
	})
}

// observe begins the observation of a request. It runs once the body is read and
// checked, because the helper takes the model alias when observation begins and has
// no later hook for it; this is still before the key is verified or any internal call
// is made. A call that failed its checks is observed too, with what was read.
func (h *handler) observe(r *http.Request, call Call) *observability.Request {
	observed, err := h.observer.Begin(observability.RequestInfo{
		Route:      h.proto.path(),
		Method:     r.Method,
		RequestID:  call.RequestID,
		ModelAlias: call.Model,
	})
	if err != nil {
		// Only an observer that NewObserver did not make lacks the route.
		h.logger.Error("request is not observed", "request_id", call.RequestID, "err", err.Error())
	}
	return observed
}

// refuse writes a refusal, from the checks or from admission, and ends the call's
// observation with the result of its status.
func (h *handler) refuse(rec *recorder, call *Call, e *PublicError) {
	h.writeError(rec, e)
	finish(rec, call, refusalResult(e.Status))
}

// refusalResult maps the status of a refusal to its result, by the errors that
// api/errors.md lists before dispatch: 400 and 413 from the checks; 401, 402, 403,
// 500 and 503 from the checks or admission. An unknown status is internal_error.
func refusalResult(status int) observability.Result {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return observability.InvalidRequest
	case http.StatusUnauthorized:
		return observability.AuthenticationDenied
	case http.StatusPaymentRequired:
		return observability.InsufficientBalance
	case http.StatusForbidden:
		return observability.PermissionDenied
	case http.StatusServiceUnavailable:
		return observability.DependencyError
	}
	return observability.InternalError
}

// finish ends the call's observation with result. A handler that returns without
// writing gets a 200 from the server, so a status not yet written is reported as 200.
func finish(rec *recorder, call *Call, result observability.Result) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	call.Observation.Finish(summary(rec, call, result, status))
}

// summary is the terminal metadata of a call: its result, status and byte counts.
// A body that was not read in full counts 0 request bytes.
func summary(rec *recorder, call *Call, result observability.Result, status int) observability.Summary {
	return observability.Summary{
		Result:        result,
		HTTPStatus:    status,
		RequestBytes:  call.BodyBytes,
		ResponseBytes: rec.bytes,
	}
}
