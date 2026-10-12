package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
)

// An errorCode is one of metering's internal error codes. Each has one status and
// one fixed message; a message never repeats anything from the request.
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
	codeInvalidRequest:        {http.StatusBadRequest, "The request body or path is not valid."},
	codeUnauthorized:          {http.StatusUnauthorized, "The internal token is missing or wrong."},
	codeWalletNotFound:        {http.StatusNotFound, "The tenant has no wallet."},
	codeReservationNotFound:   {http.StatusNotFound, "The reservation does not exist."},
	codeInsufficientBalance:   {http.StatusConflict, "The available balance is below the maximum cost."},
	codeIdempotencyConflict:   {http.StatusUnprocessableEntity, "The same key was already used with different content."},
	codeInternalError:         {http.StatusInternalServerError, "The request could not be processed."},
	codeDependencyUnavailable: {http.StatusServiceUnavailable, "A dependency is unavailable; try again."},
}

// requestIDPattern is what a request id may look like, such as "req_<uuid>".
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// requestID returns the caller's X-Request-Id, or "" when it is absent or not a plain
// id, so nothing else from a header reaches a response or a log line.
func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); requestIDPattern.MatchString(id) {
		return id
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError answers with the status and body of code.
func writeError(w http.ResponseWriter, r *http.Request, code errorCode) {
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
