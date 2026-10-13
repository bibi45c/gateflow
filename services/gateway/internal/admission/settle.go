package admission

import (
	"context"

	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
	"github.com/bibi45c/gateflow/services/gateway/internal/meteringclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
)

// settle charges a call with the outcome and counts the relay reported. It runs on
// the request's goroutine after the relay is done, so it is bounded: each attempt
// gives up after the metering caller's timeout, and it retries the identical request
// at most twice, after a timeout, a 5xx or a connection error. When it finally
// fails, it logs the call and reservation ids at error level, and the reservation
// stays held until recovery (UC6).
func (a *Admission) settle(callID, reservationID string, result relay.Result) {
	req := settleRequest(result)
	var err error
	for range settleAttempts {
		// A fresh context: the request's own has ended once the client has left.
		if _, err = a.metering.Settle(context.Background(), callID, reservationID, req); err == nil {
			return
		}
		if !internalapi.Retryable(err) {
			break
		}
	}
	a.logger.Error("settle failed; the reservation stays held",
		append([]any{"call_id", callID, "reservation_id", reservationID, "outcome", req.Outcome}, errAttrs(err)...)...)
}

// settleRequest returns the settle body of a result. Metering charges 0 for
// upstream_error and not_dispatched and ignores their counts; #18 has the gateway
// send both counts as 0.
func settleRequest(result relay.Result) meteringclient.SettleRequest {
	req := meteringclient.SettleRequest{
		InputTokens:  result.InputTokens,
		OutputTokens: result.OutputTokens,
		Outcome:      result.Outcome,
	}
	if result.Outcome == relay.OutcomeUpstreamError || result.Outcome == relay.OutcomeNotDispatched {
		req.InputTokens, req.OutputTokens = new(int64), new(int64)
	}
	return req
}
