package relay

import (
	"net/http"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// protocol is the wire format of an endpoint: how the relay checks a request, calls
// the upstream, writes its own errors and reads the reply. Everything else is shared:
// the size check, the gateway key, admission, the event cap, the failure handling, the
// outcome rule and the access log.
type protocol interface {
	// id names the protocol in a Call.
	id() Protocol
	// path is the endpoint's path, which the relay also appends to a channel's base
	// URL.
	path() string
	// checkBody checks the members of a request body, after the size and key checks,
	// and fills call.Model and call.MaxTokens. It returns the body to send upstream.
	checkBody(body []byte, call *Call) ([]byte, *PublicError)
	// forwardedHeaders are the only client headers sent upstream. The client's
	// x-api-key and Authorization, and any internal token, are never among them.
	forwardedHeaders() []string
	// setCredential puts a channel credential in the protocol's credential header.
	setCredential(header http.Header, credential string)
	// errorBody returns the body of an error response.
	errorBody(e *PublicError) any
	// noEnd is the error of a stream that ended before its end event.
	noEnd() error
	// newObserver returns what reads one reply.
	newObserver() observer
}

// observer reads one upstream reply by the usage source and outcome rule of its
// protocol in api/metering.yaml.
type observer interface {
	// event reads one stream event, before the relay writes it, and says what the
	// relay does with it.
	event(e sse.Event) eventRole
	// body reads a 2xx non-streaming reply and reports whether it completes the call.
	body(data []byte) bool
	// counts returns the counts to report; nil is unknown.
	counts() (input, output *int64)
	// errorEvent returns the name and data of the gateway's own error event, which
	// follows the events read so far.
	errorEvent() (name string, data []byte)
}

// eventRole is what one stream event means to the relay.
type eventRole int

const (
	// roleOutput is an event to relay before reading on.
	roleOutput eventRole = iota
	// roleEnd is the end event, which completes the call: the relay relays it and
	// stops.
	roleEnd
	// roleFailure is an upstream error event: the relay relays it unchanged and stops,
	// or answers 502 when it comes before any other event.
	roleFailure
)
