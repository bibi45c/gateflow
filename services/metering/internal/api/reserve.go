package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"

	"github.com/bibi45c/gateflow/services/metering/internal/accessclient"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// Limits on what a request may carry. Internal requests are a few short fields.
const (
	maxBodyBytes = 64 << 10
	maxIDBytes   = 200
)

type reserveRequest struct {
	CallID          *string `json:"call_id"`
	AttemptID       *int64  `json:"attempt_id"`
	TenantID        *string `json:"tenant_id"`
	PriceVersionID  *string `json:"price_version_id"`
	MaxInputTokens  *int64  `json:"max_input_tokens"`
	MaxOutputTokens *int64  `json:"max_output_tokens"`
}

type reserveResponse struct {
	ReservationID string `json:"reservation_id"`
	MaxCost       int64  `json:"max_cost"`
	Status        string `json:"status"`
}

// decodeBody reads one JSON object into v. A field the request schema does not have
// is an error, and so is anything after the object.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func validID(id *string) bool {
	return id != nil && *id != "" && len(*id) <= maxIDBytes
}

// reservation checks a reserve request and turns it into the reservation to record,
// still without prices, maximum cost or id.
func (q reserveRequest) reservation() (Reservation, bool) {
	if !validID(q.CallID) || !validID(q.TenantID) || !validID(q.PriceVersionID) ||
		q.AttemptID == nil || *q.AttemptID < 1 || *q.AttemptID > math.MaxInt32 ||
		q.MaxInputTokens == nil || *q.MaxInputTokens < 0 ||
		q.MaxOutputTokens == nil || *q.MaxOutputTokens < 0 {
		return Reservation{}, false
	}
	bounds := billing.Bounds{MaxInput: *q.MaxInputTokens, MaxOutput: *q.MaxOutputTokens}
	return Reservation{
		CallID:         *q.CallID,
		AttemptID:      int32(*q.AttemptID),
		TenantID:       *q.TenantID,
		PriceVersionID: *q.PriceVersionID,
		Bounds:         bounds,
		Digest:         billing.ReserveDigest(*q.TenantID, *q.PriceVersionID, bounds),
	}, true
}

func (h *Handler) reserve(w http.ResponseWriter, r *http.Request) {
	var request reserveRequest
	if !decodeBody(w, r, &request) {
		writeError(w, r, codeInvalidRequest)
		return
	}
	wanted, ok := request.reservation()
	if !ok {
		writeError(w, r, codeInvalidRequest)
		return
	}

	// A replay is answered from the record alone: no price lookup, no balance check.
	recorded, found, err := h.store.FindReservation(r.Context(), wanted.CallID, wanted.AttemptID)
	if err != nil {
		h.storeFailure(w, r, "reserve", err)
		return
	}
	if found {
		writeReplay(w, r, recorded, wanted)
		return
	}

	// The price lookup is a network call, so it happens before any transaction.
	wanted.Prices, err = h.prices.PriceVersion(r.Context(), wanted.PriceVersionID, requestID(r))
	if errors.Is(err, accessclient.ErrUnavailable) {
		writeFailure(w, r, codeDependencyUnavailable, "reserve", err)
		return
	}
	if err == nil && !wanted.Prices.Valid() {
		err = errors.New("price version outside the allowed price range")
	}
	if err != nil {
		writeFailure(w, r, codeInternalError, "reserve", err)
		return
	}
	if wanted.MaxCost, ok = billing.MaxCost(wanted.Prices, wanted.Bounds); !ok {
		writeError(w, r, codeInvalidRequest)
		return
	}

	stored, created, err := h.store.Reserve(r.Context(), wanted)
	switch {
	case err != nil:
		h.storeFailure(w, r, "reserve", err)
	case created:
		writeJSON(w, http.StatusCreated, reserveResponse{stored.ID, stored.MaxCost, "reserved"})
	default: // a concurrent duplicate recorded the key first
		writeReplay(w, r, stored, wanted)
	}
}

// writeReplay answers a repeated key: the recorded reservation when the content is
// the same, even after it was settled, and a conflict otherwise.
func writeReplay(w http.ResponseWriter, r *http.Request, recorded, wanted Reservation) {
	if !bytes.Equal(recorded.Digest, wanted.Digest) {
		writeError(w, r, codeIdempotencyConflict)
		return
	}
	writeJSON(w, http.StatusOK, reserveResponse{recorded.ID, recorded.MaxCost, "reserved"})
}

// storeFailure answers for an error from the store.
func (h *Handler) storeFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	switch {
	case errors.Is(err, ErrWalletNotFound):
		writeError(w, r, codeWalletNotFound)
	case errors.Is(err, ErrInsufficientBalance):
		writeError(w, r, codeInsufficientBalance)
	case errors.Is(err, ErrReservationNotFound):
		writeError(w, r, codeReservationNotFound)
	case errors.Is(err, ErrConflict):
		writeError(w, r, codeIdempotencyConflict)
	case errors.Is(err, ErrUnavailable):
		writeFailure(w, r, codeDependencyUnavailable, operation, err)
	default:
		writeFailure(w, r, codeInternalError, operation, err)
	}
}
