package httpapi

import (
	"encoding/json"
	"net/http"
)

// The error codes the access service returns.
const (
	CodeInvalidRequest        = "invalid_request"
	CodeUnauthorized          = "unauthorized"
	CodeInvalidKey            = "invalid_key"
	CodeModelNotAllowed       = "model_not_allowed"
	CodePriceVersionNotFound  = "price_version_not_found"
	CodeDependencyUnavailable = "dependency_unavailable"
	CodeInternalError         = "internal_error"
)

// errorKinds gives each code its status and its one message. A message is a fixed
// string and never includes anything from the request.
var errorKinds = map[string]struct {
	status  int
	message string
}{
	CodeInvalidRequest:        {http.StatusBadRequest, "The request is malformed or a required field is missing."},
	CodeUnauthorized:          {http.StatusUnauthorized, "The internal token is missing or wrong."},
	CodeInvalidKey:            {http.StatusUnauthorized, "The key is unknown or revoked."},
	CodeModelNotAllowed:       {http.StatusForbidden, "The key may not use this model."},
	CodePriceVersionNotFound:  {http.StatusNotFound, "The price version does not exist."},
	CodeDependencyUnavailable: {http.StatusServiceUnavailable, "A dependency is unavailable. Try again later."},
	CodeInternalError:         {http.StatusInternalServerError, "An internal error occurred."},
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// writeError answers with the status and fixed message of code, in the error body
// every internal API shares: {"error":{"code","message","request_id"}}.
func writeError(w http.ResponseWriter, r *http.Request, code string) {
	kind, ok := errorKinds[code]
	if !ok {
		code, kind = CodeInternalError, errorKinds[CodeInternalError]
	}
	state(r).code = code
	writeJSON(w, kind.status, errorBody{Error: errorDetail{
		Code:      code,
		Message:   kind.message,
		RequestID: requestID(r),
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
