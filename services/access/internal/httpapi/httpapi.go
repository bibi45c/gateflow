// Package httpapi serves the access service's internal API under /internal/v1: key
// verification and price versions, for the gateway and metering.
//
// Every route needs the internal bearer token and answers errors in the shared error
// body. Each request is logged on one line that never holds a gateway key, its HMAC,
// the Authorization header, the internal token or a request or response body.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/bibi45c/gateflow/services/access/internal/store"
)

// MaxBodyBytes is the largest request body the API reads.
const MaxBodyBytes = 64 << 10

// QueryTimeout bounds the database work of one request. It is below the 1 s that
// metering waits for access, so a slow database is answered with 503 in time.
const QueryTimeout = 800 * time.Millisecond

// Store is what the handlers need from the database; *store.Store implements it.
type Store interface {
	VerifyKey(ctx context.Context, keyHMAC []byte, model, protocol string) (store.VerifiedKey, error)
	PriceVersion(ctx context.Context, id string) (store.PriceVersion, error)
}

// API holds what the handlers share.
type API struct {
	store     Store
	pepper    []byte
	tokenHash [sha256.Size]byte
	hasToken  bool
	logger    *slog.Logger
}

// New returns the API. pepper is ACCESS_KEY_PEPPER and internalToken is
// ACCESS_INTERNAL_TOKEN; with an empty internalToken every call is refused.
func New(s Store, pepper []byte, internalToken string, logger *slog.Logger) *API {
	return &API{
		store:     s,
		pepper:    pepper,
		tokenHash: sha256.Sum256([]byte(internalToken)),
		hasToken:  internalToken != "",
		logger:    logger,
	}
}

// Register adds the routes to mux. Everything under /internal/v1/ needs the internal
// token, including paths that are not routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.Handle("POST /internal/v1/keys/verify", a.route(a.verifyKey))
	mux.Handle("GET /internal/v1/price-versions/{id}", a.route(a.priceVersion))
	mux.Handle("/internal/v1/", a.route(http.NotFound))
}

// requestState is what a request's log line needs and only later steps know.
type requestState struct {
	requestID string
	keyID     string
	tenantID  string
}

type stateKey struct{}

func state(r *http.Request) *requestState {
	if s, ok := r.Context().Value(stateKey{}).(*requestState); ok {
		return s
	}
	return &requestState{}
}

// requestID is the caller's X-Request-Id, or the id generated for the request.
func requestID(r *http.Request) string {
	return state(r).requestID
}

// newRequestID returns req_<uuid>, with a random (version 4) UUID.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("req_%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// route wraps a handler with what every route shares: the request id, the log line,
// 500 on a panic, and the internal token check.
func (a *API) route(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s := &requestState{requestID: r.Header.Get("X-Request-Id")}
		if s.requestID == "" {
			s.requestID = newRequestID()
		}
		r = r.WithContext(context.WithValue(r.Context(), stateKey{}, s))
		sw := &statusWriter{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				a.logger.Error("panic", "request_id", s.requestID, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeError(sw, r, CodeInternalError)
				}
			}
			attrs := []any{
				"request_id", s.requestID,
				"method", r.Method,
				"route", r.Pattern,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if s.keyID != "" {
				attrs = append(attrs, "key_id", s.keyID, "tenant_id", s.tenantID)
			}
			a.logger.Info("request", attrs...)
		}()

		if !a.authorized(r) {
			writeError(sw, r, CodeUnauthorized)
			return
		}
		next(sw, r)
	})
}

// authorized reports whether r carries "Authorization: Bearer <internal token>". It
// compares digests, so the time taken depends on neither the token nor its length.
func (a *API) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || !a.hasToken {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], a.tokenHash[:]) == 1
}
