package admission_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/admission"
	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
	"github.com/bibi45c/gateflow/services/gateway/internal/meteringclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
)

// Canaries that must never appear in the logs.
const (
	gatewayKey         = "gk-canary-3a9f61"
	accessToken        = "access-token-canary-71c2"
	meteringToken      = "metering-token-canary-0d4e"
	upstreamCredential = "sk-upstream-canary-9b18"
	bodyCanary         = "body-canary-e27c"
)

const (
	credentialEnv = "UPSTREAM_CREDENTIAL_TEST"
	// testTimeout stands in for the 2 s timeout of each internal call.
	testTimeout = 250 * time.Millisecond
)

// messagesBody is the client's request: max_tokens is the shared fixture's
// max_output_tokens.
const messagesBody = `{"model":"mock-small","max_tokens":20,"stream":true,` +
	`"messages":[{"role":"user","content":"Hello ` + bodyCanary + `"}]}`

// testEnv stands in for the gateway's environment, so no test reads or sets a real
// variable.
func testEnv(name string) string {
	if name == credentialEnv {
		return upstreamCredential
	}
	return ""
}

// recorded is one request a stand-in received.
type recorded struct {
	Path   string
	Header http.Header
	Body   []byte
}

// answer replies to the nth request, counted from 1, of one route of a stand-in.
type answer func(w http.ResponseWriter, r *http.Request, n int)

// route records the requests of one stand-in route.
type route struct {
	mu  sync.Mutex
	got []recorded
}

func (rt *route) handler(reply answer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rt.mu.Lock()
		rt.got = append(rt.got, recorded{Path: r.URL.EscapedPath(), Header: r.Header.Clone(), Body: body})
		n := len(rt.got)
		rt.mu.Unlock()
		reply(w, r, n)
	}
}

func (rt *route) requests() []recorded {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return slices.Clone(rt.got)
}

// setup says how the stand-ins behave. A nil answer is the success of the contract
// examples: a completed stream, a verify naming the upstream, a 201 reserve and a
// 200 settle.
type setup struct {
	upstream, verify, reserve, settle answer
	accessDown, meteringDown          bool // the service's address refuses connections
	tlsUpstream                       bool // an https upstream behind credentialEnv
}

// harness is the gateway's POST /v1/messages, as main mounts it, in front of
// stand-ins for the upstream, access and metering.
type harness struct {
	t                                 *testing.T
	upstream, verify, reserve, settle *route
	gateway                           *httptest.Server
	logs                              *syncBuffer
}

func start(t *testing.T, s setup) *harness {
	t.Helper()
	h := &harness{t: t, upstream: &route{}, verify: &route{}, reserve: &route{}, settle: &route{}, logs: &syncBuffer{}}
	t.Cleanup(h.checkLogs) // runs after every server has closed

	upstreamMux := http.NewServeMux()
	upstreamMux.Handle("POST /v1/messages", h.upstream.handler(or(s.upstream, completedStream)))
	upstream := httptest.NewUnstartedServer(upstreamMux)
	channel := channelJSON{ID: "mock", Protocol: "anthropic-messages"}
	if s.tlsUpstream {
		upstream.StartTLS()
		channel.CredentialEnv = credentialEnv
	} else {
		upstream.Start()
	}
	t.Cleanup(upstream.Close)
	channel.BaseURL = upstream.URL

	accessMux := http.NewServeMux()
	accessMux.Handle("POST /internal/v1/keys/verify", h.verify.handler(or(s.verify, verifyOK(channel))))
	accessURL := serve(t, accessMux, s.accessDown)
	meteringMux := http.NewServeMux()
	meteringMux.Handle("POST /internal/v1/reservations", h.reserve.handler(or(s.reserve, reserveOK)))
	meteringMux.Handle("POST /internal/v1/reservations/{id}/settle", h.settle.handler(or(s.settle, settleOK)))
	meteringURL := serve(t, meteringMux, s.meteringDown)

	logger := slog.New(slog.NewJSONHandler(h.logs, nil))
	accessCaller := internalapi.NewCaller(accessURL, accessToken)
	accessCaller.Timeout = testTimeout
	meteringCaller := internalapi.NewCaller(meteringURL, meteringToken)
	meteringCaller.Timeout = testTimeout
	admit := admission.New(admission.Config{
		Access:   accessclient.New(accessCaller),
		Metering: meteringclient.New(meteringCaller),
		Getenv:   testEnv,
		Logger:   logger,
	})
	cfg := relay.Config{Getenv: testEnv}
	if s.tlsUpstream {
		cfg.Transport = upstream.Client().Transport
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", relay.New(cfg, admit, logger))
	h.gateway = httptest.NewServer(mux)
	t.Cleanup(h.gateway.Close)
	return h
}

// serve starts a stand-in service and returns its URL, or the URL of a closed port
// when down.
func serve(t *testing.T, handler http.Handler, down bool) string {
	if down {
		return closedPortURL(t)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func or(reply, fallback answer) answer {
	if reply != nil {
		return reply
	}
	return fallback
}

// closedPortURL returns the URL of a local port that nothing listens on.
func closedPortURL(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return "http://" + addr
}

// post sends the client's request with the gateway key in x-api-key, or with header
// instead when it is not nil.
func (h *harness) post(ctx context.Context, header http.Header) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.gateway.URL+"/v1/messages", strings.NewReader(messagesBody))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if header == nil {
		req.Header.Set("x-api-key", gatewayKey)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := h.gateway.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// finish waits until the gateway has handled every request, and so has settled.
func (h *harness) finish() {
	h.gateway.Close()
}

// checkLogs fails the test when a log line holds a canary.
func (h *harness) checkLogs() {
	h.finish()
	logs := h.logs.String()
	for _, canary := range []string{gatewayKey, accessToken, meteringToken, upstreamCredential, bodyCanary} {
		if strings.Contains(logs, canary) {
			h.t.Errorf("the logs hold the canary %q:\n%s", canary, logs)
		}
	}
}

// errorLines returns the error log lines.
func (h *harness) errorLines() []map[string]any {
	h.t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(h.logs.String()))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			h.t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if line["level"] == "ERROR" {
			lines = append(lines, line)
		}
	}
	return lines
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

type channelJSON struct {
	ID            string `json:"id"`
	Protocol      string `json:"protocol"`
	BaseURL       string `json:"base_url"`
	CredentialEnv string `json:"credential_env"`
}

// verifyOK answers verify like the contract example, with channel and the shared
// fixture's price version.
func verifyOK(channel channelJSON) answer {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		body, _ := json.Marshal(map[string]any{
			"tenant_id":        "tenant-01",
			"key_id":           "key-tenant-01",
			"price_version_id": "pv-fixture-2-5",
			"channel":          channel,
		})
		reply(w, http.StatusOK, string(body))
	}
}

func reserveOK(w http.ResponseWriter, r *http.Request, n int) {
	reply(w, http.StatusCreated, `{"reservation_id":"rsv_f1","max_cost":300,"status":"reserved"}`)
}

func settleOK(w http.ResponseWriter, r *http.Request, n int) {
	reply(w, http.StatusOK, `{"charged":170,"ledger_entry_id":"led_f1","status":"settled"}`)
}

// internalError answers with the error body of api/errors.md.
func internalError(status int, code string) answer {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		reply(w, status, `{"error":{"code":"`+code+`","message":"A fixed message.","request_id":"`+
			r.Header.Get("X-Request-Id")+`"}}`)
	}
}

func raw(status int, body string) answer {
	return func(w http.ResponseWriter, r *http.Request, n int) { reply(w, status, body) }
}

// hang answers nothing until the caller gives up.
func hang(w http.ResponseWriter, r *http.Request, n int) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

// dropConnection closes the connection without an answer.
func dropConnection(w http.ResponseWriter, r *http.Request, n int) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		conn.Close()
	}
}

// failFirst answers the first request with fail and the later ones with ok.
func failFirst(fail, ok answer) answer {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			fail(w, r, n)
			return
		}
		ok(w, r, n)
	}
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// streamReply answers with a 200 event stream of events.
func streamReply(events ...string) answer {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			_ = http.NewResponseController(w).Flush()
		}
	}
}

func event(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

func messageStart(usage string) string {
	return event("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message",`+
		`"role":"assistant","content":[],"model":"mock-small","usage":`+usage+`}}`)
}

func messageDelta(usage string) string {
	return event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":`+usage+`}`)
}

var (
	textDelta = event("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`)
	messageStop = event("message_stop", `{"type":"message_stop"}`)
)

// completedStream reports input 40 + 15 cache-read + 5 cache-creation = 60 tokens
// and 10 output tokens: the first row of the shared fixture.
var completedStream = streamReply(
	messageStart(`{"input_tokens":40,"cache_read_input_tokens":15,"cache_creation_input_tokens":5,"output_tokens":1}`),
	textDelta,
	messageDelta(`{"output_tokens":10}`),
	messageStop,
)

// readBody reads a whole response body.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// checkPublicError fails the test unless resp is an Anthropic error of status and
// type, and returns its body.
func checkPublicError(t *testing.T, resp *http.Response, status int, errorType string) string {
	t.Helper()
	body := readBody(t, resp)
	if resp.StatusCode != status {
		t.Errorf("status = %d, want %d; body %s", resp.StatusCode, status, body)
	}
	var decoded anthropic.ErrorBody
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if decoded.Type != "error" || decoded.Error.Type != errorType || decoded.Error.Message == "" {
		t.Errorf("body = %s, want an error of type %s", body, errorType)
	}
	return string(body)
}

// checkJSON fails the test unless got is the JSON value want, in any key order.
func checkJSON(t *testing.T, what string, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s %s: %v", what, got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("want %s: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// checkInternalCall fails the test unless a request to an internal service carried
// the bearer token and the gateway's request id.
func checkInternalCall(t *testing.T, got recorded, token, requestID string) {
	t.Helper()
	if auth := got.Header.Get("Authorization"); auth != "Bearer "+token {
		t.Errorf("%s: Authorization = %q, want the internal token", got.Path, auth)
	}
	if id := got.Header.Get("X-Request-Id"); id != requestID || !strings.HasPrefix(id, "req_") {
		t.Errorf("%s: X-Request-Id = %q, want %q", got.Path, id, requestID)
	}
}
