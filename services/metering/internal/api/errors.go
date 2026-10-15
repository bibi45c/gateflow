package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
)

// An errorCode is one of metering's internal error codes, as listed in api/errors.md.
// Each has one status and one fixed message, the one api/metering.yaml shows for it;
// a message never repeats anything from the request.
type errorCode string

const (
	codeInvalidRequest        errorCode = "invalid_request"
	codeUnauthorized          errorCode = "unauthorized"
	codeWalletNotFound        errorCode = "wallet_not_found"
	codeReservationNotFound   errorCode = "reservation_not_found"
	codeInsufficientBalance   errorCode = "insufficient_balance"
	codeIdempotencyConflict   errorCode = "idempotency_conflict"
	codeInternalError         errorCode = "internal_error"
	codeDependencyUnavailable errorCode = "dependency_unavailable"
)

var errorReplies = map[errorCode]struct {
	status  int
	message string
}{
	codeInvalidRequest:        {http.StatusBadRequest, "The request body is invalid."},
	codeUnauthorized:          {http.StatusUnauthorized, "The internal token is missing or wrong."},
	codeWalletNotFound:        {http.StatusNotFound, "The tenant has no wallet."},
	codeReservationNotFound:   {http.StatusNotFound, "The reservation does not exist."},
	codeInsufficientBalance:   {http.StatusConflict, "The available balance is below the reservation amount."},
	codeIdempotencyConflict:   {http.StatusUnprocessableEntity, "The request differs from an earlier request with the same key."},
	codeInternalError:         {http.StatusInternalServerError, "Internal error."},
	codeDependencyUnavailable: {http.StatusServiceUnavailable, "A dependency is unavailable."},
}

// requestIDPattern is what a request id may look like, such as "req_<uuid>".
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type requestIDKey struct{}

// identified gives a request the one id used in its error body, its log lines and its
// call to access: the caller's X-Request-Id, or a new id when that is absent or not a
// plain id, so nothing else from a header reaches a response or a log line.
func identified(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		next(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	}
}

// requestID returns the id that identified gave the request.
func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

// newRequestID returns a random id in the gateway's form, "req_<uuid>".
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])                      // never fails
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80 // a version 4 UUID
	return fmt.Sprintf("req_%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError answers with the status and body of code.
func writeError(w http.ResponseWriter, r *http.Request, code errorCode) {
	if call, ok := r.Context().Value(callKey{}).(*callState); ok {
		call.code = code
	}
	reply := errorReplies[code]
	writeJSON(w, reply.status, map[string]any{"error": map[string]string{
		"code": string(code), "message": reply.message, "request_id": requestID(r),
	}})
}

// writeFailure answers with code for a failure the caller did not cause, and logs the
// cause. The log line carries ids only, never a body or a token.
func writeFailure(w http.ResponseWriter, r *http.Request, code errorCode, operation string, cause error) {
	slog.Error("metering request failed", "operation", operation, "code", string(code),
		"request_id", requestID(r), "err", cause)
	writeError(w, r, code)
}
