package api

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Wallet is a tenant's balance and what open reservations hold of it.
type Wallet struct {
	TenantID  string    `json:"tenant_id"`
	Balance   int64     `json:"balance"`
	Held      int64     `json:"held"`
	Available int64     `json:"available"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UsageSummary adds up the calls reserved since a time.
type UsageSummary struct {
	Since        time.Time `json:"since"`
	Requests     int64     `json:"requests"`
	Charged      int64     `json:"charged"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// UsageItem is one call attempt as the usage log shows it.
type UsageItem struct {
	ReservationID string     `json:"reservation_id"`
	CallID        string     `json:"call_id"`
	Model         *string    `json:"model"`
	Status        string     `json:"status"`
	Outcome       *string    `json:"outcome"`
	InputTokens   *int64     `json:"input_tokens"`
	OutputTokens  *int64     `json:"output_tokens"`
	MaxCost       int64      `json:"max_cost"`
	Charged       *int64     `json:"charged"`
	CreatedAt     time.Time  `json:"created_at"`
	SettledAt     *time.Time `json:"settled_at"`
}

// Usage is a tenant's wallet, the summary since a time and its latest calls.
type Usage struct {
	Wallet  Wallet       `json:"wallet"`
	Summary UsageSummary `json:"summary"`
	Items   []UsageItem  `json:"items"`
}

// Adjustment changes a wallet's balance by Amount, which is never 0.
type Adjustment struct {
	ID           string    `json:"adjustment_id"`
	TenantID     string    `json:"tenant_id"`
	Amount       int64     `json:"amount"`
	Note         string    `json:"note"`
	BalanceAfter int64     `json:"balance_after"`
	CreatedAt    time.Time `json:"created_at"`
}

// ConsoleStore is what the console routes need from the database.
type ConsoleStore interface {
	// Wallets returns every wallet, by tenant.
	Wallets(ctx context.Context) ([]Wallet, error)
	// Usage returns the tenant's wallet, its summary since since and its latest
	// limit calls, or ErrWalletNotFound.
	Usage(ctx context.Context, tenantID string, since time.Time, limit int) (Usage, error)
	// Adjust applies a once, creating a missing wallet for a top-up. A repeat of the
	// same id and content returns the recorded adjustment with created false; other
	// content is ErrConflict. It returns ErrInsufficientBalance when the available
	// balance would fall below 0.
	Adjust(ctx context.Context, a Adjustment) (stored Adjustment, created bool, err error)
}

// Console route templates.
const (
	walletsRoute     = "/internal/v1/wallets"
	usageRoute       = "/internal/v1/tenants/{tenant_id}/usage"
	adjustmentsRoute = "/internal/v1/wallets/{tenant_id}/adjustments"
)

// Usage query limits.
const (
	defaultUsageLimit = 50
	maxUsageLimit     = 200
	maxNoteBytes      = 200
)

// WithConsole adds the console routes, served from cs, and returns h.
func (h *Handler) WithConsole(cs ConsoleStore) *Handler {
	h.console = cs
	return h
}

func (h *Handler) registerConsole(mux *http.ServeMux) {
	if h.console == nil {
		return
	}
	mux.HandleFunc("GET "+walletsRoute, identified(h.observed(walletsRoute, h.authorized(h.wallets))))
	mux.HandleFunc("GET "+usageRoute, identified(h.observed(usageRoute, h.authorized(h.usage))))
	mux.HandleFunc("POST "+adjustmentsRoute, identified(h.observed(adjustmentsRoute, h.authorized(h.adjust))))
}

func (h *Handler) wallets(w http.ResponseWriter, r *http.Request) {
	wallets, err := h.console.Wallets(r.Context())
	if err != nil {
		h.storeFailure(w, r, "wallets", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": wallets})
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	if !validPathID(tenantID) {
		writeError(w, r, codeWalletNotFound)
		return
	}
	query := r.URL.Query()
	since := time.Now().UTC().Truncate(24 * time.Hour)
	if raw := query.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, r, codeInvalidRequest)
			return
		}
		since = parsed
	}
	limit := defaultUsageLimit
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxUsageLimit {
			writeError(w, r, codeInvalidRequest)
			return
		}
		limit = n
	}
	usage, err := h.console.Usage(r.Context(), tenantID, since, limit)
	if err != nil {
		h.storeFailure(w, r, "usage", err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

type adjustRequest struct {
	AdjustmentID *string `json:"adjustment_id"`
	Amount       *int64  `json:"amount"`
	Note         *string `json:"note"`
}

func (h *Handler) adjust(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	var request adjustRequest
	if !validPathID(tenantID) || !decodeBody(w, r, &request) || !validID(request.AdjustmentID) ||
		request.Amount == nil || *request.Amount == 0 || *request.Amount == math.MinInt64 {
		writeError(w, r, codeInvalidRequest)
		return
	}
	note := ""
	if request.Note != nil {
		note = *request.Note
	}
	if len(note) > maxNoteBytes || !utf8.ValidString(note) || strings.ContainsRune(note, 0) {
		writeError(w, r, codeInvalidRequest)
		return
	}
	stored, created, err := h.console.Adjust(r.Context(), Adjustment{
		ID: *request.AdjustmentID, TenantID: tenantID, Amount: *request.Amount, Note: note})
	switch {
	case err != nil:
		h.storeFailure(w, r, "adjust", err)
	case created:
		writeJSON(w, http.StatusCreated, stored)
	default:
		writeJSON(w, http.StatusOK, stored)
	}
}

// validPathID reports whether a path id can be stored: non-empty, short, UTF-8
// without NUL.
func validPathID(id string) bool {
	return id != "" && len(id) <= maxIDBytes && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}
