// Package api serves metering's internal HTTP API: reserving the maximum cost of a
// model call before it is sent, and settling the real cost afterwards.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// Errors a Store returns for the cases the API reports to its caller. Any other
// error is an internal error.
var (
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrReservationNotFound = errors.New("reservation not found")
	ErrConflict            = errors.New("same key, different content")
	ErrUnavailable         = errors.New("database unavailable")
)

// Reservation is a cost hold for one attempt of one call.
type Reservation struct {
	ID             string
	CallID         string
	AttemptID      int32
	TenantID       string
	PriceVersionID string
	// Model is the model the caller named, or empty; it is not part of Digest.
	Model   string
	Prices  billing.Prices
	Bounds  billing.Bounds
	MaxCost int64
	Digest  []byte
}

// Settlement is what the gateway reports when a call ends.
type Settlement struct {
	Outcome billing.Outcome
	Usage   billing.Usage
	Digest  []byte
}

// Settled is the recorded result of a settlement.
type Settled struct {
	Charged       int64
	LedgerEntryID string
}

// Store is the database behind the API. Each method is one short transaction, and
// none of them keeps a lock or a connection after it returns.
type Store interface {
	// FindReservation returns the reservation recorded for the key, if there is one.
	FindReservation(ctx context.Context, callID string, attemptID int32) (Reservation, bool, error)
	// Reserve records r, assigns its ID and holds r.MaxCost on the tenant's wallet.
	// When the key is already recorded, it changes nothing and returns that
	// reservation with created false. It returns ErrWalletNotFound or
	// ErrInsufficientBalance when it records nothing.
	Reserve(ctx context.Context, r Reservation) (stored Reservation, created bool, err error)
	// Settle charges the reservation once and releases the rest of its hold. A repeat
	// with the same digest returns the recorded result; another digest is
	// ErrConflict. An unknown reservation is ErrReservationNotFound.
	Settle(ctx context.Context, reservationID string, s Settlement) (Settled, error)
}

// PriceSource looks up the prices of a price version, as accessclient.Client does.
type PriceSource interface {
	PriceVersion(ctx context.Context, id, requestID string) (billing.Prices, error)
}

// Handler serves the reserve and settle operations.
type Handler struct {
	store    Store
	prices   PriceSource
	token    [sha256.Size]byte
	open     bool
	observer *observability.Observer
	console  ConsoleStore
}

// New returns a handler that accepts calls carrying token as their bearer token. With
// an empty token it refuses every call, so a missing setting never opens the API.
// observer, from NewObserver, times and counts every call and writes its request
// line; nil observes nothing.
func New(store Store, prices PriceSource, token string, observer *observability.Observer) *Handler {
	return &Handler{store: store, prices: prices, token: sha256.Sum256([]byte(token)), open: token != "",
		observer: observer}
}

// Register adds the two operations, and the console routes when set, to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+reserveRoute, identified(h.observed(reserveRoute, h.authorized(h.reserve))))
	mux.HandleFunc("POST "+settleRoute, identified(h.observed(settleRoute, h.authorized(h.settle))))
	h.registerConsole(mux)
}

// authorized runs next only for a request with the internal bearer token. Both sides
// are hashed first, so the comparison takes the same time for any token length.
func (h *Handler) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, isBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sent := sha256.Sum256([]byte(token))
		if !h.open || !isBearer || subtle.ConstantTimeCompare(sent[:], h.token[:]) != 1 {
			writeError(w, r, codeUnauthorized)
			return
		}
		next(w, r)
	}
}
