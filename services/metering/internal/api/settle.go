package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// settleRequest keeps the counts as raw JSON, so a count that is null (unknown) can
// be told from one that is missing (a malformed request).
type settleRequest struct {
	InputTokens  json.RawMessage `json:"input_tokens"`
	OutputTokens json.RawMessage `json:"output_tokens"`
	Outcome      *string         `json:"outcome"`
}

type settleResponse struct {
	Charged       int64  `json:"charged"`
	LedgerEntryID string `json:"ledger_entry_id"`
	Status        string `json:"status"`
}

// tokenCount reads a count that must be present: null is unknown, otherwise a whole
// number that is not negative.
func tokenCount(raw json.RawMessage) (*int64, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if bytes.Equal(raw, []byte("null")) {
		return nil, true
	}
	var count int64
	if err := json.Unmarshal(raw, &count); err != nil || count < 0 {
		return nil, false
	}
	return &count, true
}

// settlement checks a settle request and turns it into the settlement to record.
func (q settleRequest) settlement() (Settlement, bool) {
	input, inputOK := tokenCount(q.InputTokens)
	output, outputOK := tokenCount(q.OutputTokens)
	if !inputOK || !outputOK || q.Outcome == nil || !billing.Outcome(*q.Outcome).Valid() {
		return Settlement{}, false
	}
	outcome, usage := billing.Outcome(*q.Outcome), billing.Usage{Input: input, Output: output}
	return Settlement{Outcome: outcome, Usage: usage, Digest: billing.SettleDigest(outcome, usage)}, true
}

func (h *Handler) settle(w http.ResponseWriter, r *http.Request) {
	var request settleRequest
	if !decodeBody(w, r, &request) {
		writeError(w, r, codeInvalidRequest)
		return
	}
	settlement, ok := request.settlement()
	if !ok {
		writeError(w, r, codeInvalidRequest)
		return
	}
	reservationID := r.PathValue("reservation_id")
	if len(reservationID) > maxIDBytes {
		// No reservation has an id this long, so the database is not asked.
		writeError(w, r, codeReservationNotFound)
		return
	}
	settled, err := h.store.Settle(r.Context(), reservationID, settlement)
	if err != nil {
		h.storeFailure(w, r, "settle", err)
		return
	}
	writeJSON(w, http.StatusOK, settleResponse{settled.Charged, settled.LedgerEntryID, "settled"})
}
