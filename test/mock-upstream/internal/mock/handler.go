// Package mock serves a fake Anthropic Messages endpoint and a fake OpenAI Responses
// endpoint. The request's model name picks a fixed scenario, so the same request
// always gets the same bytes back, in the protocol of the endpoint:
//
//	mock-text         one text block
//	mock-tool         one call of the first tool in the request's tools
//	mock-coding       a coding script that reads, edits and tests calc.py, then answers
//	mock-429          HTTP 429 rate_limit_error, with retry-after: 1
//	mock-500          HTTP 500 api_error
//	mock-drop         a long text stream whose connection closes after K events
//	mock-error-event  a long text stream that sends an error event after K events
//	mock-load         a text stream for load tests, shaped by Load: MOCK_LOAD_EVENTS
//	                  deltas of MOCK_LOAD_EVENT_BYTES bytes, one every
//	                  MOCK_LOAD_INTERVAL_MS
//
// Usage follows two formulas: input_tokens = ceil(request body bytes / 4), and
// output_tokens = the number of text and tool input deltas the reply streams, which a
// non-streaming reply counts as if it had streamed. The cache counts are always 0; on
// Responses, so is reasoning_tokens, and total_tokens is the sum of the two counts.
// A Responses request offers only its function tools to the scenarios.
package mock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/bibi45c/gateflow/pkg/config"
	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// maxBodyBytes bounds a request body, as the Anthropic API does.
const maxBodyBytes = 32 << 20

// Error types the mock sends that FlowGate itself never does, so
// pkg/protocol/anthropic has no constants for them.
const (
	errorTypeNotFound   = "not_found_error"
	errorTypeRateLimit  = "rate_limit_error"
	errorTypeOverloaded = "overloaded_error"
)

// statusOverloaded is the status the Anthropic API answers with when it is
// overloaded.
const statusOverloaded = 529

// Timing sets the pace of a stream and where the failure scenarios cut it.
type Timing struct {
	// FirstEventDelay is the wait before the first event of a stream.
	FirstEventDelay time.Duration
	// EventInterval is the wait before each later event.
	EventInterval time.Duration
	// FailAfterEvents is K, the number of events mock-drop and mock-error-event
	// send before they fail.
	FailAfterEvents int
	// Load shapes the stream of mock-load, which paces its events by Load.Interval
	// instead of EventInterval.
	Load Load
	// fixedRate times each event from the start of the stream; forModel sets it
	// for mock-load only.
	fixedRate bool
}

// DefaultTiming sends every event at once, fails after three events, and streams
// DefaultLoad for mock-load.
var DefaultTiming = Timing{FailAfterEvents: 3, Load: DefaultLoad}

// timingSettings lists the environment variables that set Timing, each with the
// request header that overrides it for one request. Compose sets the first two;
// MOCK_FAIL_AFTER_EVENTS is left to its default of 3. The gateway does not forward
// X-Mock-* headers, so they only take effect when the mock is called directly.
var timingSettings = []struct {
	env, header string
	set         func(*Timing, int)
}{
	{"MOCK_FIRST_EVENT_DELAY_MS", "X-Mock-First-Event-Delay-Ms", func(t *Timing, n int) {
		t.FirstEventDelay = time.Duration(n) * time.Millisecond
	}},
	{"MOCK_EVENT_INTERVAL_MS", "X-Mock-Event-Interval-Ms", func(t *Timing, n int) {
		t.EventInterval = time.Duration(n) * time.Millisecond
	}},
	{"MOCK_FAIL_AFTER_EVENTS", "X-Mock-Fail-After-Events", func(t *Timing, n int) {
		t.FailAfterEvents = n
	}},
}

// TimingFromEnv returns DefaultTiming with the values set in the environment,
// including the MOCK_LOAD_* variables of loadSettings. It fails when a value is not
// a non-negative integer, or is out of range for Load.
func TimingFromEnv() (Timing, error) {
	timing := DefaultTiming
	for _, setting := range timingSettings {
		value := config.Env(setting.env, "")
		if value == "" {
			continue
		}
		n, ok := parseCount(value)
		if !ok {
			return Timing{}, fmt.Errorf("%s: %q is not a non-negative integer", setting.env, value)
		}
		setting.set(&timing, n)
	}
	if err := loadFromEnv(&timing.Load); err != nil {
		return Timing{}, err
	}
	return timing, nil
}

// withHeaders returns t with the values of the request's X-Mock-* headers. A
// header that is not a non-negative integer is ignored.
func (t Timing) withHeaders(header http.Header) Timing {
	for _, setting := range timingSettings {
		if n, ok := parseCount(header.Get(setting.header)); ok {
			setting.set(&t, n)
		}
	}
	return t
}

// parseCount reads a non-negative decimal integer small enough that its value in
// milliseconds fits in a time.Duration.
func parseCount(value string) (int, bool) {
	n, err := strconv.ParseUint(value, 10, 31)
	return int(n), err == nil
}

// Handler serves one endpoint of the mock: POST /v1/messages or POST /v1/responses.
type Handler struct {
	timing Timing
	proto  protocol
}

// protocol is the wire format of an endpoint: how it reads a request and writes the
// replies and errors of the scenarios.
type protocol interface {
	// readRequest reads and checks a request body.
	readRequest(body []byte) (*request, *apiError)
	// errorBody returns the body of an error response.
	errorBody(failure *apiError) any
	// responseBody returns the non-streaming response body of rep.
	responseBody(rep reply, inputTokens int64) any
	// streamEvents returns every event of the stream of rep; the last one ends it.
	streamEvents(rep reply, inputTokens int64) []event
	// errorEvent returns the error event that mock-error-event sends after the first
	// sent events.
	errorEvent(sent int) event
}

// NewHandler returns a Handler for POST /v1/messages that paces streams by timing,
// unless a request's X-Mock-* headers override it.
func NewHandler(timing Timing) *Handler {
	return &Handler{timing: timing, proto: messages{}}
}

// NewResponsesHandler returns a Handler for POST /v1/responses that paces streams
// like NewHandler.
func NewResponsesHandler(timing Timing) *Handler {
	return &Handler{timing: timing, proto: responses{}}
}

// apiError is an error the mock answers with an error body. The error types of the
// two protocols have the same names.
type apiError struct {
	status    int
	errorType string
	message   string
}

func invalidRequest(message string) *apiError {
	return &apiError{http.StatusBadRequest, anthropic.ErrorTypeInvalidRequest, message}
}

// ServeHTTP answers a request with the scenario its model names.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, failure := h.readRequest(w, r)
	if failure == nil {
		var rep reply
		if rep, failure = replyFor(req, h.timing.Load); failure == nil {
			h.answer(w, r, req, rep)
			return
		}
	}
	if failure.status == http.StatusTooManyRequests {
		w.Header().Set("retry-after", "1")
	}
	h.writeError(w, failure)
}

// answer sends rep, or fails as the scenario of req says.
func (h *Handler) answer(w http.ResponseWriter, r *http.Request, req *request, rep reply) {
	failMode := failureModes[req.model]
	if req.stream {
		events := h.proto.streamEvents(rep, req.inputTokens())
		h.writeStream(w, r, events, h.timing.withHeaders(r.Header).forModel(req.model), failMode)
		return
	}
	switch failMode {
	case failDrop:
		// Close the connection before any response, as a dropped upstream would.
		panic(http.ErrAbortHandler)
	case failErrorEvent:
		h.writeError(w, &apiError{statusOverloaded, errorTypeOverloaded, overloadedMessage})
	default:
		writeJSON(w, http.StatusOK, h.proto.responseBody(rep, req.inputTokens()))
	}
}

// request holds what the mock reads from a request.
type request struct {
	model       string
	stream      bool
	bodyBytes   int
	toolResults int // tool_result blocks or function_call_output items
	tools       []tool
}

// inputTokens is ceil(request body bytes / 4).
func (r *request) inputTokens() int64 {
	return int64(r.bodyBytes+3) / 4
}

// readRequest reads the request body and lets the protocol check it.
func (h *Handler) readRequest(w http.ResponseWriter, r *http.Request) (*request, *apiError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, &apiError{http.StatusRequestEntityTooLarge, anthropic.ErrorTypeRequestTooLarge,
			"request body is too large"}
	case err != nil:
		return nil, invalidRequest("could not read the request body")
	}
	return h.proto.readRequest(body)
}

func (h *Handler) writeError(w http.ResponseWriter, failure *apiError) {
	writeJSON(w, failure.status, h.proto.errorBody(failure))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(mustMarshal(body))
}

// mustMarshal encodes values built by this package, which always encode.
func mustMarshal(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("mock: encode %T: %v", value, err))
	}
	return data
}
