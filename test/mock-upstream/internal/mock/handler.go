// Package mock serves a fake Anthropic Messages endpoint. The request's model name
// picks a fixed scenario, so the same request always gets the same bytes back:
//
//	mock-text         one text block
//	mock-tool         one tool_use of the first tool in the request's tools
//	mock-coding       a coding script that reads, edits and tests calc.py, then answers
//	mock-429          HTTP 429 rate_limit_error, with retry-after: 1
//	mock-500          HTTP 500 api_error
//	mock-drop         a long text stream whose connection closes after K events
//	mock-error-event  a long text stream that sends event: error after K events
//
// Usage follows two formulas: input_tokens = ceil(request body bytes / 4), and
// output_tokens = the number of content_block_delta events, which a non-streaming
// reply counts as if it had streamed. The cache counts are always 0.
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
}

// DefaultTiming sends every event at once and fails after three events.
var DefaultTiming = Timing{FailAfterEvents: 3}

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

// TimingFromEnv returns DefaultTiming with the values set in the environment. It
// fails when a value is not a non-negative integer.
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

// Handler serves POST /v1/messages.
type Handler struct {
	timing Timing
}

// NewHandler returns a Handler that paces streams by timing, unless a request's
// X-Mock-* headers override it.
func NewHandler(timing Timing) *Handler {
	return &Handler{timing: timing}
}

// apiError is an error the mock answers with an Anthropic error body.
type apiError struct {
	status    int
	errorType string
	message   string
}

func invalidRequest(message string) *apiError {
	return &apiError{http.StatusBadRequest, anthropic.ErrorTypeInvalidRequest, message}
}

// ServeHTTP answers a Messages request with the scenario its model names.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, failure := readRequest(w, r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	rep, failure := replyFor(req)
	if failure != nil {
		if failure.status == http.StatusTooManyRequests {
			w.Header().Set("retry-after", "1")
		}
		writeError(w, failure)
		return
	}
	failMode := failureModes[req.model]
	if req.stream {
		events := streamEvents(rep, req.inputTokens())
		writeStream(w, r, events, h.timing.withHeaders(r.Header), failMode)
		return
	}
	switch failMode {
	case failDrop:
		// Close the connection before any response, as a dropped upstream would.
		panic(http.ErrAbortHandler)
	case failErrorEvent:
		writeError(w, &apiError{statusOverloaded, errorTypeOverloaded, overloadedMessage})
	default:
		writeJSON(w, http.StatusOK, messageBody(rep, req.inputTokens()))
	}
}

// request holds what the mock reads from a Messages request.
type request struct {
	model       string
	stream      bool
	bodyBytes   int
	toolResults int
	tools       []tool
}

// inputTokens is ceil(request body bytes / 4).
func (r *request) inputTokens() int64 {
	return int64(r.bodyBytes+3) / 4
}

// readRequest reads and checks the request body.
func readRequest(w http.ResponseWriter, r *http.Request) (*request, *apiError) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, &apiError{http.StatusRequestEntityTooLarge, anthropic.ErrorTypeRequestTooLarge,
			"request body is too large"}
	case err != nil:
		return nil, invalidRequest("could not read the request body")
	}
	parsed, err := anthropic.ParseRequest(body)
	if err != nil {
		return nil, invalidRequest("request body is not valid JSON: " + err.Error())
	}
	if parsed.MaxTokens == nil {
		return nil, invalidRequest("max_tokens: field required")
	}
	var members struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, invalidRequest("messages or tools have the wrong shape")
	}
	req := &request{model: parsed.Model, stream: parsed.Stream, bodyBytes: len(body),
		tools: members.Tools}
	for _, message := range members.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		// A content string holds no blocks, so a failed decode counts none.
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "tool_result" {
					req.toolResults++
				}
			}
		}
	}
	return req, nil
}

func writeError(w http.ResponseWriter, failure *apiError) {
	writeJSON(w, failure.status, anthropic.NewErrorBody(failure.errorType, failure.message))
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
