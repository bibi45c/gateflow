package openai

import "encoding/json"

// Error types of the Responses error body and error event that FlowGate sends. They
// are the types of /v1/messages; which situation gets which type and status is listed
// in api/errors.md.
const (
	ErrorTypeInvalidRequest  = "invalid_request_error"
	ErrorTypeAuthentication  = "authentication_error"
	ErrorTypeBilling         = "billing_error"
	ErrorTypePermission      = "permission_error"
	ErrorTypeRequestTooLarge = "request_too_large"
	ErrorTypeAPI             = "api_error"
)

// ErrorBody is a Responses error: the body of an error response.
//
//	{"error":{"message":"...","type":"api_error","param":null,"code":null}}
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the error member of an ErrorBody. A nil Param or Code is written as
// null.
type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// NewErrorBody returns the error body with the given error type and message, and with
// param and code null.
func NewErrorBody(errorType, message string) ErrorBody {
	return ErrorBody{Error: ErrorDetail{Message: message, Type: errorType}}
}

// WithParam returns a copy of b whose param names the request member at fault.
func (b ErrorBody) WithParam(param string) ErrorBody {
	b.Error.Param = &param
	return b
}

// errorEvent is the data of an error event, with its members in the order that
// api/errors.md gives.
type errorEvent struct {
	Type           string  `json:"type"`
	Code           string  `json:"code"`
	Message        string  `json:"message"`
	Param          *string `json:"param"`
	SequenceNumber int64   `json:"sequence_number"`
}

// EncodeErrorEvent returns the data of an error event in a Responses stream, with the
// members of api/errors.md: type "error", code, message, param null and
// sequence_number. The gateway's own error event carries its error type in code.
// Write the data as an event named EventError.
//
//	{"type":"error","code":"api_error","message":"...","param":null,"sequence_number":12}
func EncodeErrorEvent(code, message string, sequenceNumber int64) []byte {
	// Strings and integers always encode, so Marshal cannot fail here.
	data, _ := json.Marshal(errorEvent{
		Type:           EventError,
		Code:           code,
		Message:        message,
		SequenceNumber: sequenceNumber,
	})
	return data
}
