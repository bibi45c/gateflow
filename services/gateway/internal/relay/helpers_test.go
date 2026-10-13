package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// Canaries that must never appear in the logs.
const (
	gatewayKeyValue    = "gk-canary-7d1e0b"
	upstreamCredential = "sk-upstream-canary-42c9"
	bodyCanary         = "body-canary-5b0f9e"
	credentialEnv      = "UPSTREAM_CREDENTIAL_TEST"
)

// testEnv stands in for the gateway's environment, so that no test reads or sets a
// real variable.
func testEnv(name string) string {
	if name == credentialEnv {
		return upstreamCredential
	}
	return ""
}

// stubAdmission stands in for #39: it admits every call to one channel, or refuses
// it with a set error, and records the calls and what the relay reports.
type stubAdmission struct {
	channel Channel
	refuse  *PublicError
	results chan Result

	mu      sync.Mutex
	calls   []Call
	reports int
}

func newStubAdmission(channel Channel) *stubAdmission {
	return &stubAdmission{channel: channel, results: make(chan Result, 16)}
}

func (s *stubAdmission) Admit(_ context.Context, call Call) (Admitted, *PublicError) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	if s.refuse != nil {
		return Admitted{}, s.refuse
	}
	return Admitted{Channel: s.channel, Report: s.report}, nil
}

func (s *stubAdmission) report(result Result) {
	s.mu.Lock()
	s.reports++
	s.mu.Unlock()
	s.results <- result
}

// result waits for the next report.
func (s *stubAdmission) result(t *testing.T) Result {
	t.Helper()
	select {
	case result := <-s.results:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("the relay reported no result")
		return Result{}
	}
}

func (s *stubAdmission) admitted() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *stubAdmission) reportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports
}

// syncBuffer is a log destination that handlers on other goroutines write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// gateway is the relay served on a test server, as the gateway mounts it.
type gateway struct {
	*httptest.Server
	admission *stubAdmission
	logs      *syncBuffer
}

// startGateway serves the relay in front of channel, with a stand-in admission and
// environment. When the test ends it checks that every admitted request was reported
// exactly once.
func startGateway(t *testing.T, channel Channel) *gateway {
	t.Helper()
	admission := newStubAdmission(channel)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", New(Config{Getenv: testEnv}, admission, logger))
	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.Close() // waits for the handlers, and so for their reports
		if calls, reports := len(admission.admitted()), admission.reportCount(); reports != calls {
			t.Errorf("%d admitted requests were reported %d times", calls, reports)
		}
	})
	return &gateway{Server: server, admission: admission, logs: logs}
}

// startUpstream serves a stand-in upstream and returns the channel that reaches it.
func startUpstream(t *testing.T, handler http.HandlerFunc) Channel {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return Channel{BaseURL: server.URL}
}

// gate blocks a stand-in upstream until the test opens it, or the test ends.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{ch: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() {
	g.once.Do(func() { close(g.ch) })
}

// post sends a Messages request with a gateway key to the gateway.
func (g *gateway) post(t *testing.T, ctx context.Context, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", gatewayKeyValue)
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// messagesBody returns a Messages request body that holds bodyCanary.
func messagesBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":256,"stream":%t,`+
		`"messages":[{"role":"user","content":"What is the weather in Paris? %s"}]}`,
		model, stream, bodyCanary)
}

// fixture returns a file from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// event returns one event block in the Anthropic framing, named by its data's type.
func event(data string) string {
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &typed); err != nil {
		panic(err)
	}
	return "event: " + typed.Type + "\ndata: " + data + "\n\n"
}

func messageStart(usage string) string {
	return event(`{"type":"message_start","message":{"id":"msg_1","type":"message",` +
		`"role":"assistant","content":[],"model":"mock-small","usage":` + usage + `}}`)
}

func textDelta(text string) string {
	return event(`{"type":"content_block_delta","index":0,` +
		`"delta":{"type":"text_delta","text":"` + text + `"}}`)
}

func messageDelta(usage string) string {
	return event(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + usage + `}`)
}

func messageStop() string {
	return event(`{"type":"message_stop"}`)
}

// startStream starts a 200 event stream.
func startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
}

// sendEvents writes each event and flushes it.
func sendEvents(w http.ResponseWriter, events ...string) {
	for _, e := range events {
		_, _ = io.WriteString(w, e)
		_ = http.NewResponseController(w).Flush()
	}
}

// readEvents reads every block of a stream, failing the test on any error but EOF.
func readEvents(t *testing.T, reader *anthropic.Reader) []anthropic.Event {
	t.Helper()
	var events []anthropic.Event
	for {
		e, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("after %d events: %v", len(events), err)
		}
		events = append(events, e)
	}
}

func eventTypes(events []anthropic.Event) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type()
	}
	return types
}

// readAllBody reads a whole response body.
func readAllBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// errorJSON returns the Anthropic error body the relay writes.
func errorJSON(t *testing.T, errorType, message string) string {
	t.Helper()
	data, err := json.Marshal(anthropic.NewErrorBody(errorType, message))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// gatewayErrorEvent is the event the gateway sends when a started stream fails.
func gatewayErrorEvent(t *testing.T) string {
	return "event: error\ndata: " + errorJSON(t, anthropic.ErrorTypeAPI, upstreamFailedMessage) + "\n\n"
}

func count(n int64) *int64 {
	return &n
}

// checkResult fails the test unless got has the outcome and counts; nil is unknown.
func checkResult(t *testing.T, got Result, outcome string, input, output *int64) {
	t.Helper()
	if got.Outcome != outcome {
		t.Errorf("outcome = %q, want %q", got.Outcome, outcome)
	}
	if !sameCount(got.InputTokens, input) {
		t.Errorf("input tokens = %s, want %s", showCount(got.InputTokens), showCount(input))
	}
	if !sameCount(got.OutputTokens, output) {
		t.Errorf("output tokens = %s, want %s", showCount(got.OutputTokens), showCount(output))
	}
}

func sameCount(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func showCount(c *int64) string {
	if c == nil {
		return "null"
	}
	return fmt.Sprint(*c)
}
