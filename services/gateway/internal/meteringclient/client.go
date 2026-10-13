// Package meteringclient calls the metering service's internal API, as
// api/metering.yaml gives it: the gateway reserves the most a call can cost before
// it dispatches the call, and settles the reservation after the call ends.
package meteringclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
)

const reservationsPath = "/internal/v1/reservations"

// Error codes of metering, from api/errors.md, that the gateway answers with their
// own public error. Every other code gets 500.
const (
	CodeInsufficientBalance = "insufficient_balance"
	CodeWalletNotFound      = "wallet_not_found"
)

// statusReserved is the status of every reserve answer, a replay included.
const statusReserved = "reserved"

// ReserveRequest holds the most one call attempt can cost.
type ReserveRequest struct {
	CallID          string `json:"call_id"`
	AttemptID       int    `json:"attempt_id"`
	TenantID        string `json:"tenant_id"`
	PriceVersionID  string `json:"price_version_id"`
	MaxInputTokens  int64  `json:"max_input_tokens"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
}

// Reservation is the answer to a reserve call.
type Reservation struct {
	ReservationID string `json:"reservation_id"`
	MaxCost       int64  `json:"max_cost"`
	Status        string `json:"status"`
}

// SettleRequest charges a call. A nil count is sent as null: unknown.
type SettleRequest struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	Outcome      string `json:"outcome"`
}

// Settlement is the answer to a settle call.
type Settlement struct {
	Charged       int64  `json:"charged"`
	LedgerEntryID string `json:"ledger_entry_id"`
	Status        string `json:"status"`
}

// Client calls metering.
type Client struct {
	api *internalapi.Caller
}

// New returns a client that sends its calls through api.
func New(api *internalapi.Caller) *Client {
	return &Client{api: api}
}

// Reserve holds the most a call attempt can cost. It accepts a new hold (201) and a
// replay of one (200). Its errors are those of internalapi.Caller.Post; an answer
// that confirms no reservation is internalapi.ErrMalformed.
func (c *Client) Reserve(ctx context.Context, requestID string, req ReserveRequest) (Reservation, error) {
	var reservation Reservation
	err := c.api.Post(ctx, reservationsPath, requestID, req, &reservation, http.StatusCreated, http.StatusOK)
	if err != nil {
		return Reservation{}, err
	}
	if reservation.ReservationID == "" || reservation.Status != statusReserved {
		return Reservation{}, internalapi.ErrMalformed
	}
	return reservation, nil
}

// Settle charges a call against its reservation and releases the rest. Its errors
// are those of internalapi.Caller.Post.
func (c *Client) Settle(ctx context.Context, requestID, reservationID string, req SettleRequest) (Settlement, error) {
	var settlement Settlement
	path := reservationsPath + "/" + url.PathEscape(reservationID) + "/settle"
	if err := c.api.Post(ctx, path, requestID, req, &settlement, http.StatusOK); err != nil {
		return Settlement{}, err
	}
	return settlement, nil
}
