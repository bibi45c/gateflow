package admission

import (
	"context"
	"net/http"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/ratelimit"
	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
)

// Limiter checks a tenant's rate and concurrency limits; *ratelimit.Limiter is one.
type Limiter interface {
	Acquire(ctx context.Context, tenant, requestID string, lim ratelimit.Limits) (release func(), refusal *ratelimit.Refusal, err error)
}

// Public messages of a call refused by its tenant's limits. They name the gateway's
// own limit, so a client can tell them from an upstream 429, which keeps the
// upstream's body.
const (
	msgRPMLimit         = "The gateway key's tenant is over its plan's request rate limit."
	msgConcurrencyLimit = "The gateway key's tenant is over its plan's concurrent request limit."
)

// limit checks the verified tenant's limits before anything is reserved (#157). It
// returns the function that frees the call's concurrency lease, to call once when
// the call ends, or the public error of a refusal. With no Limiter, or for an
// unlimited plan, it admits without asking. When the limits cannot be checked, it
// refuses with 503: admitting would let a tenant exceed the limits it is held to.
func (a *Admission) limit(ctx context.Context, call relay.Call, verified accessclient.Verified) (func(), *relay.PublicError) {
	if a.limiter == nil {
		return func() {}, nil
	}
	lim := ratelimit.Limits{RPM: verified.Limits.RPM, MaxConcurrent: verified.Limits.MaxConcurrent}
	release, refusal, err := a.limiter.Acquire(ctx, verified.TenantID, call.RequestID, lim)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			a.logger.Error("rate limits cannot be checked; call refused",
				"request_id", call.RequestID, "plan_id", verified.Limits.PlanID, "error", err.Error())
		}
		return nil, errUnavailable
	case refusal != nil:
		message := msgRPMLimit
		if refusal.Reason == ratelimit.ReasonConcurrency {
			message = msgConcurrencyLimit
		}
		return nil, &relay.PublicError{Status: http.StatusTooManyRequests, Type: anthropic.ErrorTypeRateLimit,
			Message: message, RetryAfter: refusal.RetryAfterSeconds()}
	}
	return release, nil
}
