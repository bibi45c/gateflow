// Package relay serves POST /v1/messages (Anthropic Messages) and POST /v1/responses
// (OpenAI Responses). It checks a request in the order of issue #18, asks an
// Admission whether it may go upstream, sends the body to the admitted channel and
// relays the reply back event by event, without buffering. It reports the usage and
// outcome it saw through the Admission's report function, by the rules of
// api/metering.yaml. The public errors it writes are listed in api/errors.md, in the
// error body of the endpoint called. The endpoint's protocol gives how the relay reads
// the request and the reply; the rest is shared. It converts nothing between the two
// protocols. It reports each request's result, demand and first output to the
// observability helper, which writes the request's log line.
package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bibi45c/gateflow/pkg/observability"
)

// Bounds from issue #18. They are S1 constants, not configuration.
const (
	maxBodyBytes  = 2 << 20 // largest request body accepted
	maxEventBytes = 1 << 20 // largest upstream event, or non-streaming body, relayed
	// maxOutputTokens is the largest max_tokens or max_output_tokens accepted; the
	// smallest is 1. A Responses request without max_output_tokens is held at it, and
	// sent upstream with it.
	maxOutputTokens = 32768
	connectTimeout  = 10 * time.Second
)

// Outcomes of an admitted request, in the order of the first-match rule of #18.
const (
	OutcomeNotDispatched   = "not_dispatched"
	OutcomeCompleted       = "completed"
	OutcomeClientCancelled = "client_cancelled"
	OutcomeUpstreamError   = "upstream_error"
	OutcomeInterrupted     = "interrupted"
)

// Protocol is the wire protocol of the endpoint a request came to. Its zero value is
// Messages, the protocol that access also assumes when a verify names none.
type Protocol int

const (
	// ProtocolMessages is Anthropic Messages, POST /v1/messages.
	ProtocolMessages Protocol = iota
	// ProtocolResponses is OpenAI Responses, POST /v1/responses.
	ProtocolResponses
)

// String returns the name the internal contracts give the protocol.
func (p Protocol) String() string {
	switch p {
	case ProtocolMessages:
		return "anthropic-messages"
	case ProtocolResponses:
		return "openai-responses"
	}
	return fmt.Sprintf("Protocol(%d)", int(p))
}

// Channel is where an admitted request goes.
type Channel struct {
	BaseURL       string // scheme://host[:port], no path; the relay appends the endpoint's path
	CredentialEnv string // empty, or the name of a gateway environment variable
}

// Admission decides whether a checked request may be relayed. #39 implements it
// (key verification, channel check, cost hold); tests use a stand-in.
type Admission interface {
	// Admit runs after the request checks pass. It returns the channel and a
	// report function, or a public error that the relay writes as is.
	Admit(ctx context.Context, call Call) (Admitted, *PublicError)
}

// Call is what Admission needs to know about the request.
type Call struct {
	RequestID  string   // req_<uuid>, also sent back as the request-id header
	Protocol   Protocol // the protocol of the endpoint called
	GatewayKey string   // from x-api-key, or Authorization: Bearer
	Model      string
	// MaxTokens is the output bound to hold: max_tokens on Messages; on Responses,
	// max_output_tokens, or 32768 when the request has none.
	MaxTokens int64
	BodyBytes int64 // len(body); #39 reserves with max_input_tokens = BodyBytes
	// Observation is the request's observation, on which Admit sets the verified
	// tenant. Nil observes nothing; its methods are then no-ops.
	Observation *observability.Request
}

// Admitted is a request that may go upstream.
type Admitted struct {
	Channel Channel
	Report  func(Result) // called exactly once, after the relay finishes or fails
}

// Result is what the relay saw. Counts follow the usage source of the protocol in
// api/metering.yaml; nil means unknown. For not_dispatched and upstream_error both
// counts are 0, as #18 asks the gateway to send them.
type Result struct {
	Outcome      string // not_dispatched, completed, client_cancelled, upstream_error, interrupted
	InputTokens  *int64
	OutputTokens *int64
}

// PublicError is an error decided before dispatch. The relay writes it in the error
// body of the endpoint called.
type PublicError struct {
	Status  int
	Type    string // an error type of api/errors.md; both endpoints use the same names
	Message string // fixed string, never request input
	Param   string // the request member at fault, or ""; only the Responses body has it
}

// Config holds what the relay takes from its process. The zero Config reads
// credentials from the process environment and opens upstream connections itself.
type Config struct {
	// Getenv returns the value of the variable that a channel's CredentialEnv names.
	// Nil means os.Getenv.
	Getenv func(name string) string
	// Transport sends upstream requests. Nil means a transport of the handler's own,
	// from NewTransport.
	Transport http.RoundTripper
	// Observer times and counts every request and writes its request log line. It
	// must come from NewObserver, which knows the endpoints' routes. Nil observes
	// nothing.
	Observer *observability.Observer
}

type handler struct {
	proto    protocol
	getenv   func(string) string
	client   *http.Client
	admit    Admission
	logger   *slog.Logger
	observer *observability.Observer
}

// New returns the handler for POST /v1/messages. Every response carries a request-id
// header, and cfg.Observer observes every request. logger gets the failures.
func New(cfg Config, admit Admission, logger *slog.Logger) http.Handler {
	return newHandler(messages{}, cfg, admit, logger)
}

// NewResponses returns the handler for POST /v1/responses, which works as the one New
// returns does.
func NewResponses(cfg Config, admit Admission, logger *slog.Logger) http.Handler {
	return newHandler(responses{}, cfg, admit, logger)
}

func newHandler(proto protocol, cfg Config, admit Admission, logger *slog.Logger) *handler {
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.Transport == nil {
		cfg.Transport = NewTransport(DefaultMaxIdleConns)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{
		proto:  proto,
		getenv: cfg.Getenv,
		// A redirect is relayed as an upstream status, so the body and credential
		// never go to another address.
		client:   &http.Client{Transport: cfg.Transport, CheckRedirect: noRedirect},
		admit:    admit,
		logger:   logger,
		observer: cfg.Observer,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := Call{RequestID: newRequestID(), Protocol: h.proto.id()}
	w.Header().Set("request-id", call.RequestID)
	rec := &recorder{ResponseWriter: w}

	body, perr := h.checkRequest(w, r, &call)
	call.Observation = h.observe(r, call)
	// After a panic the call ends interrupted, which releases its admission. Every
	// other exit has ended it already, and a second Finish has no effect.
	defer func() { call.Observation.Finish(summary(rec, &call, observability.Interrupted, rec.status)) }()
	if perr != nil {
		h.refuse(rec, &call, perr)
		return
	}
	admitted, perr := h.admit.Admit(r.Context(), call)
	if perr != nil {
		h.refuse(rec, &call, perr)
		return
	}
	// The gateway has no capacity limit yet, so a call that admission accepts, with
	// its cost held, is eligible demand and admitted at once. A refused call is
	// neither, so it never reaches the two demand signals.
	call.Observation.MarkEligible()
	call.Observation.MarkAdmitted()
	outcome := h.relay(rec, r, body, call, admitted)
	finish(rec, &call, observability.Result(outcome))
}

func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
