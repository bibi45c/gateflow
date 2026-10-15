package relay

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// Messages of the 400 invalid_request_error answers on /v1/messages.
const (
	msgMalformed = "The request body must be a JSON object with a string model, an integer max_tokens and a boolean stream."
	msgModel     = "model must be a non-empty string."
	msgMaxTokens = "max_tokens must be an integer from 1 to 32768."
)

// errNoMessageStop reports a stream that ended without message_stop.
var errNoMessageStop = errors.New("relay: upstream stream ended without message_stop")

// messagesHeaders are the client headers that /v1/messages forwards.
var messagesHeaders = []string{"Content-Type", "Anthropic-Version", "Anthropic-Beta"}

// messages is Anthropic Messages, POST /v1/messages. The body goes upstream
// unchanged, and the channel credential in x-api-key.
type messages struct{}

func (messages) id() Protocol               { return ProtocolMessages }
func (messages) path() string               { return "/v1/messages" }
func (messages) forwardedHeaders() []string { return messagesHeaders }
func (messages) noEnd() error               { return errNoMessageStop }
func (messages) newObserver() observer      { return &messagesObserver{} }

func (messages) setCredential(header http.Header, credential string) {
	header.Set("X-Api-Key", credential)
}

// checkBody checks the JSON, model and max_tokens, in the order of #18.
func (messages) checkBody(body []byte, call *Call) ([]byte, *PublicError) {
	req, err := anthropic.ParseRequest(body)
	if err != nil {
		return nil, invalidRequest(msgMalformed)
	}
	if req.Model == "" {
		return nil, invalidRequest(msgModel)
	}
	call.Model = req.Model
	if req.MaxTokens == nil || *req.MaxTokens < 1 || *req.MaxTokens > maxOutputTokens {
		return nil, invalidRequest(msgMaxTokens)
	}
	call.MaxTokens = *req.MaxTokens
	return body, nil
}

// errorBody returns the Anthropic error body of e.
func (messages) errorBody(e *PublicError) any {
	return anthropic.NewErrorBody(e.Type, e.Message)
}

// messagesObserver reads a Messages reply: its usage by the usage source of #18,
// message_stop as its end event and event: error as an upstream failure.
type messagesObserver struct {
	usage usage
}

func (o *messagesObserver) event(e sse.Event) eventRole {
	eventType := e.Type()
	o.usage.observe(eventType, e.Data)
	switch eventType {
	case anthropic.EventMessageStop:
		return roleEnd
	case anthropic.EventError:
		return roleFailure
	}
	return roleOutput
}

// body reads the usage of a non-streaming reply, which completes the call once it is
// read in full.
func (o *messagesObserver) body(data []byte) bool {
	o.usage.observeBody(data)
	return true
}

func (o *messagesObserver) counts() (input, output *int64) {
	return o.usage.counts()
}

// errorEvent returns the gateway's event: error, of type api_error.
func (o *messagesObserver) errorEvent() (string, []byte) {
	data, _ := json.Marshal(anthropic.NewErrorBody(anthropic.ErrorTypeAPI, upstreamFailedMessage)) // strings always encode
	return anthropic.EventError, data
}
