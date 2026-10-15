package relay

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// Public errors the relay raises itself. Their messages are fixed and never quote
// the request; 5xx messages carry no internal detail.
var (
	errTooLarge = &PublicError{
		Status:  http.StatusRequestEntityTooLarge,
		Type:    anthropic.ErrorTypeRequestTooLarge,
		Message: "The request body is larger than 2 MiB.",
	}
	errMissingKey = &PublicError{
		Status:  http.StatusUnauthorized,
		Type:    anthropic.ErrorTypeAuthentication,
		Message: "A gateway key is required in the x-api-key header or as an Authorization bearer token.",
	}
	errUpstream = &PublicError{
		Status:  http.StatusBadGateway,
		Type:    anthropic.ErrorTypeAPI,
		Message: upstreamFailedMessage,
	}
	errInternal = &PublicError{
		Status:  http.StatusInternalServerError,
		Type:    anthropic.ErrorTypeAPI,
		Message: "Internal server error.",
	}
)

// upstreamFailedMessage is the message of the 502 and of the error event the
// gateway sends when the upstream fails.
const upstreamFailedMessage = "The upstream model service failed."

// msgUnreadable is the message of the 400 for a body that could not be read.
const msgUnreadable = "The request body could not be read."

// checkRequest reads and checks a request in the order of #18: body size, gateway key
// present, then the body members that the endpoint's protocol checks. It fills call as
// it goes, so that the request's observation has what was read even when a check
// fails. It returns the body to send upstream.
func (h *handler) checkRequest(w http.ResponseWriter, r *http.Request, call *Call) ([]byte, *PublicError) {
	body, perr := readBody(w, r)
	if perr != nil {
		return nil, perr
	}
	call.BodyBytes = int64(len(body))
	call.GatewayKey = gatewayKey(r.Header)
	if call.GatewayKey == "" {
		return nil, errMissingKey
	}
	return h.proto.checkBody(body, call)
}

// readBody reads the request body, refusing one over maxBodyBytes. w must be the
// server's own writer, so that the server closes the connection after a 413.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *PublicError) {
	if r.ContentLength > maxBodyBytes {
		return nil, errTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, errTooLarge
	case err != nil:
		return nil, invalidRequest(msgUnreadable)
	}
	return body, nil
}

// gatewayKey returns the key in x-api-key, or else the bearer token in
// Authorization, or "" when there is neither.
func gatewayKey(header http.Header) string {
	if key := strings.TrimSpace(header.Get("x-api-key")); key != "" {
		return key
	}
	scheme, token, found := strings.Cut(strings.TrimSpace(header.Get("Authorization")), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func invalidRequest(message string) *PublicError {
	return &PublicError{
		Status:  http.StatusBadRequest,
		Type:    anthropic.ErrorTypeInvalidRequest,
		Message: message,
	}
}

// writeError writes e as an error response, in the error body of the handler's
// protocol.
func (h *handler) writeError(w http.ResponseWriter, e *PublicError) {
	body, _ := json.Marshal(h.proto.errorBody(e)) // strings always encode
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(body)
}

// newRequestID returns "req_" and a random version 4 UUID.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("req_%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
