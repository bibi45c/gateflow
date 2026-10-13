package anthropic

// Error types of the Anthropic error body that FlowGate sends. Which situation gets
// which type and status is listed in api/errors.md.
const (
	ErrorTypeInvalidRequest  = "invalid_request_error"
	ErrorTypeAuthentication  = "authentication_error"
	ErrorTypeBilling         = "billing_error"
	ErrorTypePermission      = "permission_error"
	ErrorTypeRequestTooLarge = "request_too_large"
	ErrorTypeAPI             = "api_error"
)

// ErrorBody is an Anthropic error: the body of an error response, and the data of an
// error event in a stream.
//
//	{"type":"error","error":{"type":"api_error","message":"..."}}
type ErrorBody struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the error member of an ErrorBody.
type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// NewErrorBody returns the error body of type "error" with the given error type and
// message.
func NewErrorBody(errorType, message string) ErrorBody {
	return ErrorBody{Type: "error", Error: ErrorDetail{Type: errorType, Message: message}}
}
