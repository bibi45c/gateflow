// Package httpapi serves the access service's internal API under /internal/v1: key
// verification and price versions, for the gateway and metering.
//
// Every route needs the internal bearer token and answers errors in the shared error
// body. Each request is logged on one line that never holds a gateway key, its HMAC,
// the Authorization header, the internal token or a request or response body, and
// is timed and counted by the observability helper under its route template.
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

	"github.com/bibi45c/gateflow/pkg/observability"
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
	observer  *observability.Observer
}

// New returns the API. pepper is ACCESS_KEY_PEPPER and internalToken is
// ACCESS_INTERNAL_TOKEN; with an empty internalToken every call is refused. observer,
// from NewObserver, times and counts every request; nil observes nothing.
func New(s Store, pepper []byte, internalToken string, logger *slog.Logger, observer *observability.Observer) *API {
	return &API{
		store:     s,
		pepper:    pepper,
		tokenHash: sha256.Sum256([]byte(internalToken)),
		hasToken:  internalToken != "",
		logger:    logger,
		observer:  observer,
	}
}

// Register adds the routes to mux. Everything under /internal/v1/ needs the internal
// token, including paths that are not routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.Handle("POST "+verifyRoute, a.route(verifyRoute, a.verifyKey))
	mux.Handle("GET "+priceVersionRoute, a.route(priceVersionRoute, a.priceVersion))
	mux.Handle(otherRoute, a.route(otherRoute, http.NotFound))
}

// requestState is what a request's log line needs and only later steps know.
type requestState struct {
	requestID string
	keyID     string
	tenantID  string
	code      string // the error code answered, if any
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

// statusWriter remembers the status a handler wrote and counts the bytes.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
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
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// route wraps a handler with what every route shares: the request id, the log line
// and observation under template, 500 on a panic, and the internal token check.
func (a *API) route(template string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s := &requestState{requestID: r.Header.Get("X-Request-Id")}
		if s.requestID == "" {
			s.requestID = newRequestID()
		}
		observed := a.observe(r, template, s.requestID)
		r = r.WithContext(context.WithValue(r.Context(), stateKey{}, s))
		body := &countingBody{ReadCloser: r.Body}
		r.Body = body // on the copy WithContext made, not the server's request
		sw := &statusWriter{ResponseWriter: w}
		// A panic that goes on to the server still ends the observation.
		defer func() { observed.Finish(summary(observability.Interrupted, sw, body)) }()

		defer func() {
			panicked := false
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				a.logger.Error("panic", "request_id", s.requestID, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeError(sw, r, CodeInternalError)
				}
				panicked = true
			}
			result := resultOf(s.code, sw.status, panicked)
			attrs := []any{
				"request_id", s.requestID,
				"method", r.Method,
				"route", r.Pattern,
				"status", sw.status,
				"result", string(result),
				"duration_ms", time.Since(start).Milliseconds(),
			}
			if s.keyID != "" {
				attrs = append(attrs, "key_id", s.keyID, "tenant_id", s.tenantID)
			}
			a.logger.Info("request", attrs...)
			observed.Finish(summary(result, sw, body))
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
