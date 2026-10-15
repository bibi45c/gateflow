// Package admission decides whether the relay may send a call upstream, and charges
// the call once the relay has reported how it ended, as issues #18 and #39 ask. It
// verifies the gateway key with access, checks the channel access returns, holds the
// most the call can cost with metering, and settles that hold with metering. The
// public errors it returns are those of api/errors.md.
package admission

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
	"github.com/bibi45c/gateflow/services/gateway/internal/meteringclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
)

const (
	// attemptID is the only call attempt the gateway makes in S1.
	attemptID = 1
	// reserveAttempts and settleAttempts bound the calls of one reserve and one
	// settle: a first try and the retries of #18.
	reserveAttempts = 2
	settleAttempts  = 3
)

// Public errors of a refused call. Their messages are fixed and carry no internal
// code or detail.
var (
	errInvalidKey = &relay.PublicError{
		Status:  http.StatusUnauthorized,
		Type:    anthropic.ErrorTypeAuthentication,
		Message: "The gateway key is invalid.",
	}
	errModelNotAllowed = &relay.PublicError{
		Status:  http.StatusForbidden,
		Type:    anthropic.ErrorTypePermission,
		Message: "The gateway key may not use this model.",
	}
	errBilling = &relay.PublicError{
		Status:  http.StatusPaymentRequired,
		Type:    anthropic.ErrorTypeBilling,
		Message: "The balance cannot cover this request.",
	}
	errUnavailable = &relay.PublicError{
		Status:  http.StatusServiceUnavailable,
		Type:    anthropic.ErrorTypeAPI,
		Message: "The service is temporarily unavailable.",
	}
	errInternal = &relay.PublicError{
		Status:  http.StatusInternalServerError,
		Type:    anthropic.ErrorTypeAPI,
		Message: "Internal server error.",
	}
)

// Config holds what an Admission needs.
type Config struct {
	Access   *accessclient.Client
	Metering *meteringclient.Client
	// Getenv returns the value of the variable that a channel's credential_env
	// names; it must read what the relay reads. Nil means os.Getenv.
	Getenv func(name string) string
	// Logger gets the failures. Nil means slog.Default().
	Logger *slog.Logger
}

// Admission is the relay.Admission of the gateway. It is safe for concurrent use.
type Admission struct {
	access   *accessclient.Client
	metering *meteringclient.Client
	getenv   func(string) string
	logger   *slog.Logger
}

var _ relay.Admission = (*Admission)(nil)

// New returns an Admission that calls access and metering through cfg's clients.
func New(cfg Config) *Admission {
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Admission{access: cfg.Access, metering: cfg.Metering, getenv: cfg.Getenv, logger: cfg.Logger}
}

// Admit verifies the call's key and model, checks the channel, and reserves, in the
// order of #18. It admits a call only with a confirmed reservation, and its report
// function settles that reservation.
func (a *Admission) Admit(ctx context.Context, call relay.Call) (relay.Admitted, *relay.PublicError) {
	verified, perr := a.verify(ctx, call)
	if perr != nil {
		return relay.Admitted{}, perr
	}
	if err := checkChannel(verified.Channel, call.Protocol.String(), a.getenv); err != nil {
		a.logger.Error("channel is misconfigured; no reservation made",
			"request_id", call.RequestID, "channel_id", verified.Channel.ID, "reason", err.Error())
		return relay.Admitted{}, errInternal
	}
	reservation, perr := a.reserve(ctx, call, verified)
	if perr != nil {
		return relay.Admitted{}, perr
	}
	return relay.Admitted{
		Channel: relay.Channel{BaseURL: verified.Channel.BaseURL, CredentialEnv: verified.Channel.CredentialEnv},
		Report: func(result relay.Result) {
			a.settle(call.RequestID, reservation.ReservationID, result)
		},
	}, nil
}

// verify asks access whether the call's key may use its model over the protocol of
// the endpoint called. It does not retry.
func (a *Admission) verify(ctx context.Context, call relay.Call) (accessclient.Verified, *relay.PublicError) {
	verified, err := a.access.Verify(ctx, call.RequestID, call.GatewayKey, call.Model, call.Protocol.String())
	if err == nil {
		return verified, nil
	}
	switch internalapi.Code(err) {
	case accessclient.CodeInvalidKey:
		return accessclient.Verified{}, errInvalidKey
	case accessclient.CodeModelNotAllowed:
		return accessclient.Verified{}, errModelNotAllowed
	}
	if ctx.Err() == nil {
		// Otherwise the client left and so ended the call; access is not at fault.
		a.logger.Error("access verify failed", append([]any{"request_id", call.RequestID}, errAttrs(err)...)...)
	}
	return accessclient.Verified{}, commonError(err)
}

// reserve holds the most the call can cost: the body size in bytes for input, which
// bounds the input tokens, and the call's output bound for output. It retries once,
// with the same content, after a timeout, a 5xx or a connection error.
func (a *Admission) reserve(ctx context.Context, call relay.Call, verified accessclient.Verified) (meteringclient.Reservation, *relay.PublicError) {
	req := meteringclient.ReserveRequest{
		CallID:          call.RequestID,
		AttemptID:       attemptID,
		TenantID:        verified.TenantID,
		PriceVersionID:  verified.PriceVersionID,
		MaxInputTokens:  call.BodyBytes,
		MaxOutputTokens: call.MaxTokens,
	}
	// A client that leaves does not cancel a hold under way: a hold that was made is
	// released by the not_dispatched settle that follows.
	ctx = context.WithoutCancel(ctx)
	var err error
	for range reserveAttempts {
		var reservation meteringclient.Reservation
		if reservation, err = a.metering.Reserve(ctx, call.RequestID, req); err == nil {
			return reservation, nil
		}
		if !internalapi.Retryable(err) {
			break
		}
	}
	switch internalapi.Code(err) {
	case meteringclient.CodeInsufficientBalance, meteringclient.CodeWalletNotFound:
		return meteringclient.Reservation{}, errBilling
	}
	a.logger.Error("reserve failed; the call is not relayed",
		append([]any{"request_id", call.RequestID, "call_id", call.RequestID}, errAttrs(err)...)...)
	if internalapi.Retryable(err) {
		// Both attempts failed, so metering is unavailable.
		return meteringclient.Reservation{}, errUnavailable
	}
	return meteringclient.Reservation{}, commonError(err)
}

// commonError maps a failed internal call to 503 when the service is unavailable,
// and to 500 for any other code, or a reply without a known code.
func commonError(err error) *relay.PublicError {
	if errors.Is(err, internalapi.ErrUnavailable) || internalapi.Code(err) == internalapi.CodeDependencyUnavailable {
		return errUnavailable
	}
	return errInternal
}

// errAttrs returns the log attributes of a failed internal call: the status and
// code of an error reply, or the error, which holds no token, key or body.
func errAttrs(err error) []any {
	var reply *internalapi.Error
	if errors.As(err, &reply) {
		return []any{"status", reply.Status, "code", reply.Code}
	}
	return []any{"err", err.Error()}
}
