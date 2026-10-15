package relay

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// Messages of the 400 invalid_request_error answers on /v1/responses.
const (
	msgResponsesMalformed = "The request body must be a JSON object with a string model; max_output_tokens must " +
		"be an integer, previous_response_id a string, stream a boolean and tools an array of objects."
	msgMaxOutputTokens  = "max_output_tokens must be an integer from 1 to 32768, or absent."
	msgPreviousResponse = "previous_response_id is not supported: the gateway keeps no response state."
)

// errPreviousResponseID refuses a request that continues an earlier response:
// /v1/responses is stateless, so the gateway has none to continue.
var errPreviousResponseID = &PublicError{
	Status:  http.StatusBadRequest,
	Type:    openai.ErrorTypeInvalidRequest,
	Message: msgPreviousResponse,
	Param:   "previous_response_id",
}

// errNoResponseEnd reports a stream that ended without one of its end events.
var errNoResponseEnd = errors.New(
	"relay: upstream stream ended without response.completed, response.incomplete or response.failed")

// statusFailed is the status of a Responses body that is not completed.
const statusFailed = "failed"

// responsesHeaders are the client headers that /v1/responses forwards.
var responsesHeaders = []string{"Content-Type"}

// responses is OpenAI Responses, POST /v1/responses. The gateway keeps no response
// state. The body goes upstream unchanged but for a max_output_tokens that the gateway
// adds when the request has none, and the channel credential as a bearer token.
type responses struct{}

func (responses) id() Protocol               { return ProtocolResponses }
func (responses) path() string               { return "/v1/responses" }
func (responses) forwardedHeaders() []string { return responsesHeaders }
func (responses) noEnd() error               { return errNoResponseEnd }
func (responses) newObserver() observer      { return &responsesObserver{} }

func (responses) setCredential(header http.Header, credential string) {
	header.Set("Authorization", "Bearer "+credential)
}

// modelOutput is true for a response.*.delta event, such as response.output_text.delta
// or response.function_call_arguments.delta. response.created, response.in_progress,
// the added and done events of items and parts, and the end events are not model
// output.
func (responses) modelOutput(eventType string) bool {
	return strings.HasPrefix(eventType, "response.") && strings.HasSuffix(eventType, ".delta")
}

// checkBody checks the JSON, model, max_output_tokens and previous_response_id, in the
// order of api/errors.md. A request whose max_output_tokens is absent or null is held
// at maxOutputTokens and sent upstream with it, so that its charge stays bounded, as
// api/metering.yaml asks.
func (responses) checkBody(body []byte, call *Call) ([]byte, *PublicError) {
	req, err := openai.ParseRequest(body)
	if err != nil {
		return nil, invalidRequest(msgResponsesMalformed)
	}
	if req.Model == "" {
		return nil, invalidRequest(msgModel)
	}
	call.Model = req.Model
	call.Stream = req.Stream
	bound := req.MaxOutputTokens
	if bound != nil && (*bound < 1 || *bound > maxOutputTokens) {
		return nil, invalidRequest(msgMaxOutputTokens)
	}
	if req.PreviousResponseID != nil {
		return nil, errPreviousResponseID
	}
	if bound != nil {
		call.MaxTokens = *bound
		return body, nil
	}
	upstream, err := openai.SetMaxOutputTokens(body, maxOutputTokens)
	if err != nil {
		// ParseRequest has read the same JSON, so this does not happen.
		return nil, invalidRequest(msgResponsesMalformed)
	}
	call.MaxTokens = maxOutputTokens
	return upstream, nil
}

// errorBody returns the Responses error body of e, whose param names the member at
// fault when e has one.
func (responses) errorBody(e *PublicError) any {
	body := openai.NewErrorBody(e.Type, e.Message)
	if e.Param != "" {
		body = body.WithParam(e.Param)
	}
	return body
}

// responsesObserver reads a Responses reply by the rules of api/metering.yaml. The
// counts are usage.input_tokens alone, which already counts the cached tokens, and
// usage.output_tokens, from the response of the event that ends the stream, or from
// a non-streaming body. response.completed and response.incomplete complete the call;
// response.failed, charged on its usage, and an error event are upstream failures.
type responsesObserver struct {
	input, output *int64
	// last is the data of the last event read, whose sequence_number the gateway's
	// own error event follows.
	last []byte
}

func (o *responsesObserver) event(e sse.Event) eventRole {
	eventType := e.Type()
	if eventType != "" {
		o.last = e.Data
	}
	if eventType == openai.EventError {
		return roleFailure
	}
	kind, end := openai.EndOf(eventType)
	if !end {
		return roleOutput
	}
	if reported, err := openai.EventUsage(e.Data); err == nil {
		o.observe(reported)
	}
	switch kind {
	case openai.EndCompleted, openai.EndIncomplete:
		return roleEnd
	default: // openai.EndFailed
		return roleFailure
	}
}

// body reads the usage of a non-streaming reply, which completes the call once it is
// read in full, unless its status is failed.
func (o *responsesObserver) body(data []byte) bool {
	if reported, err := openai.ResponseUsage(data); err == nil {
		o.observe(reported)
	}
	return !hasFailed(data)
}

func (o *responsesObserver) observe(reported openai.Usage) {
	o.input = knownCount(reported.InputTokens)
	o.output = knownCount(reported.OutputTokens)
}

func (o *responsesObserver) counts() (input, output *int64) {
	return reportedCounts(o.input, o.output)
}

// errorEvent returns the gateway's error event, whose code is api_error and whose
// sequence_number follows that of the last event relayed.
func (o *responsesObserver) errorEvent() (string, []byte) {
	return openai.EventError, openai.EncodeErrorEvent(openai.ErrorTypeAPI, upstreamFailedMessage,
		nextSequenceNumber(o.last))
}

// nextSequenceNumber returns one more than the sequence_number of an event's data, or
// 0 when the data has none. It matches the member's name exactly.
func nextSequenceNumber(data []byte) int64 {
	var members map[string]json.RawMessage
	var number *int64
	if json.Unmarshal(data, &members) != nil || json.Unmarshal(members["sequence_number"], &number) != nil ||
		number == nil {
		return 0
	}
	if *number == math.MaxInt64 {
		return *number
	}
	return *number + 1
}

// hasFailed reports whether the status member of a Responses body is failed. It
// matches the member's name exactly.
func hasFailed(body []byte) bool {
	var members map[string]json.RawMessage
	var status string
	return json.Unmarshal(body, &members) == nil && json.Unmarshal(members["status"], &status) == nil &&
		status == statusFailed
}
