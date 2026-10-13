package relay

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// upstreamCall is what a stand-in upstream received.
type upstreamCall struct {
	method, path string
	header       http.Header
	body         []byte
}

// recordingUpstream answers every request with the non-streaming fixture and keeps
// what it received.
func recordingUpstream(t *testing.T, reply []byte) (Channel, func() upstreamCall) {
	var mu sync.Mutex
	var got upstreamCall
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = upstreamCall{r.Method, r.URL.Path, r.Header.Clone(), body}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	})
	return channel, func() upstreamCall {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestForwardsOnlyAllowedHeaders(t *testing.T) {
	channel, received := recordingUpstream(t, fixture(t, "message.json"))
	channel.CredentialEnv = credentialEnv
	gw := startGateway(t, channel)

	body := messagesBody("mock-small", false)
	resp := gw.post(t, context.Background(), body, http.Header{
		"Anthropic-Beta":  {"tools-2024-04-04", "prompt-caching-2024-07-31"},
		"Authorization":   {"Bearer " + gatewayKeyValue},
		"Cookie":          {"session=secret"},
		"X-Custom":        {"value"},
		"X-Request-Id":    {"req_from_client"},
		"X-Forwarded-For": {"203.0.113.7"},
	})
	readAllBody(t, resp)
	got := received()

	if got.method != http.MethodPost || got.path != "/v1/messages" {
		t.Errorf("upstream got %s %s, want POST /v1/messages", got.method, got.path)
	}
	if string(got.body) != body {
		t.Errorf("upstream body = %q, want the client's bytes", got.body)
	}
	if key := got.header.Values("X-Api-Key"); !slices.Equal(key, []string{upstreamCredential}) {
		t.Errorf("x-api-key = %q, want the channel credential only", key)
	}
	want := map[string][]string{
		"Content-Type":      {"application/json"},
		"Anthropic-Version": {"2023-06-01"},
		"Anthropic-Beta":    {"tools-2024-04-04", "prompt-caching-2024-07-31"},
	}
	for name, values := range want {
		if !slices.Equal(got.header.Values(name), values) {
			t.Errorf("%s = %q, want %q", name, got.header.Values(name), values)
		}
	}
	// The transport adds User-Agent and Content-Length itself.
	allowed := []string{"Content-Type", "Anthropic-Version", "Anthropic-Beta", "X-Api-Key",
		"User-Agent", "Content-Length"}
	for name := range got.header {
		if !slices.Contains(allowed, name) {
			t.Errorf("header %s was forwarded", name)
		}
	}
}

func TestSendsNoAuthWithoutCredentialEnv(t *testing.T) {
	channel, received := recordingUpstream(t, fixture(t, "message.json"))
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", false), http.Header{
		"Authorization": {"Bearer " + gatewayKeyValue},
	})
	readAllBody(t, resp)
	got := received()
	if got.header.Get("X-Api-Key") != "" || got.header.Get("Authorization") != "" {
		t.Errorf("upstream got auth headers: %v", got.header)
	}
}

func TestNonStreamingReplyPassesThrough(t *testing.T) {
	reply := fixture(t, "message.json")
	channel, _ := recordingUpstream(t, reply)
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", false), nil)
	body := readAllBody(t, resp)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, reply) {
		t.Errorf("got %d %q, want 200 and the upstream body", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	// input_tokens 0 + cache_creation_input_tokens 12; output_tokens 0 is a known 0.
	checkResult(t, gw.admission.result(t), OutcomeCompleted, count(12), count(0))
}

func TestUpstream400And429PassThrough(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			reply := `{"type":"error","error":{"type":"rate_limit_error","message":"Slow down."}}`
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Retry-After", "17")
				w.Header().Set("Request-Id", "req_upstream")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, reply)
			})
			gw := startGateway(t, channel)

			resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
			body := readAllBody(t, resp)
			if resp.StatusCode != status || string(body) != reply {
				t.Errorf("got %d %s, want %d and the upstream body", resp.StatusCode, body, status)
			}
			if got := resp.Header.Get("Retry-After"); got != "17" {
				t.Errorf("retry-after = %q, want 17", got)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("content-type = %q", got)
			}
			if got := resp.Header.Get("Request-Id"); !requestIDPattern.MatchString(got) {
				t.Errorf("request-id = %q, want the gateway's own", got)
			}
			checkResult(t, gw.admission.result(t), OutcomeUpstreamError, count(0), count(0))
		})
	}
}

func TestOtherUpstreamStatusesGet502(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the gateway followed a redirect")
	}))
	t.Cleanup(elsewhere.Close)
	const canary = "upstream-error-canary-91aa"
	for _, status := range []int{401, 403, 404, 307, 500, 503, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", elsewhere.URL+"/v1/messages")
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"x","message":"`+canary+`"}}`)
			})
			gw := startGateway(t, channel)

			resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
			body := readAllBody(t, resp)
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", resp.StatusCode)
			}
			if want := errorJSON(t, anthropic.ErrorTypeAPI, upstreamFailedMessage); string(body) != want {
				t.Errorf("body = %s, want %s", body, want)
			}
			if resp.Header.Get("Retry-After") != "30" {
				t.Errorf("retry-after = %q, want the upstream's", resp.Header.Get("Retry-After"))
			}
			if strings.Contains(gw.logs.String(), canary) {
				t.Error("the logs hold the upstream error body")
			}
			checkResult(t, gw.admission.result(t), OutcomeUpstreamError, count(0), count(0))
		})
	}
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

func TestUnreachableUpstreamIsNotDispatched(t *testing.T) {
	gw := startGateway(t, Channel{BaseURL: closedPortURL(t)})

	resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	body := readAllBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if want := errorJSON(t, anthropic.ErrorTypeAPI, upstreamFailedMessage); string(body) != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	checkResult(t, gw.admission.result(t), OutcomeNotDispatched, count(0), count(0))
}

// A request that was sent but got no reply is interrupted, not not_dispatched.
func TestSentRequestWithoutReplyIsInterrupted(t *testing.T) {
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	readAllBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)
}

// When the client disconnects mid-stream, the gateway cancels the upstream request
// and reports client_cancelled with the usage seen so far.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, messageStart(`{"input_tokens":30,"output_tokens":1}`))
		<-r.Context().Done()
		close(cancelled)
	})
	gw := startGateway(t, channel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := gw.post(t, ctx, messagesBody("mock-small", true), nil)
	if _, err := anthropic.NewReader(resp.Body).Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
	checkResult(t, gw.admission.result(t), OutcomeClientCancelled, count(30), nil)
}

// Report runs exactly once even when the relay panics.
func TestReportedOnceWhenTheRelayPanics(t *testing.T) {
	admission := newStubAdmission(Channel{BaseURL: closedPortURL(t), CredentialEnv: credentialEnv})
	getenv := func(string) string { panic("getenv failed") }
	handler := New(Config{Getenv: getenv}, admission, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(messagesBody("mock-small", false)))
	req.Header.Set("x-api-key", gatewayKeyValue)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not reach the server")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	checkResult(t, admission.result(t), OutcomeNotDispatched, count(0), count(0))
	if n := admission.reportCount(); n != 1 {
		t.Errorf("reported %d times, want 1", n)
	}
}
