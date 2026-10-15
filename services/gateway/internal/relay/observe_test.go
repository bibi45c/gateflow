package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
	"github.com/bibi45c/gateflow/pkg/protocol/openai"
	"github.com/bibi45c/gateflow/pkg/protocol/sse"
	"github.com/prometheus/client_golang/prometheus"
)

// endpoint is one of the two endpoints, with what its tests send and stream.
type endpoint struct {
	route      string
	start      func(*testing.T, Channel) *gateway
	newHandler func(Config, Admission, *slog.Logger) http.Handler
	body       func(model string, stream bool) string
	// metadata is an event that is not model output, output one that is, and end the
	// event that completes the call.
	metadata, output, end string
}

var endpoints = []endpoint{
	{
		route: "/v1/messages", start: startGateway, newHandler: New, body: messagesBody,
		metadata: messageStart(`{"input_tokens":5}`), output: textDelta("Hi"), end: messageStop(),
	},
	{
		route: "/v1/responses", start: startResponsesGateway, newHandler: NewResponses, body: responsesBody,
		metadata: responseCreated(0), output: outputTextDelta(1, "Hi"),
		end: responseEnd(openai.EventCompleted, 2, `{"input_tokens":5,"output_tokens":1}`),
	},
}

// send posts body to the endpoint with the gateway key, or with header over it.
func (e endpoint) send(t *testing.T, gw *gateway, ctx context.Context, body string, header http.Header) *http.Response {
	t.Helper()
	if e.route == "/v1/messages" {
		return gw.post(t, ctx, body, header)
	}
	return gw.postResponses(t, ctx, body, header)
}

// total returns the sum of the series of name whose labels include match: counter
// and gauge values, and histogram sample counts.
func total(t *testing.T, registry *prometheus.Registry, name string, match map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := 0
			for _, pair := range metric.GetLabel() {
				if value, ok := match[pair.GetName()]; ok && value == pair.GetValue() {
					matched++
				}
			}
			if matched != len(match) {
				continue
			}
			sum += metric.GetCounter().GetValue() + metric.GetGauge().GetValue() +
				float64(metric.GetHistogram().GetSampleCount())
		}
	}
	return sum
}

// checkObserved fails the test unless the route has one request, ended with result,
// and the demand signals and first-output samples are as given. Every request must
// have finished.
func checkObserved(t *testing.T, registry *prometheus.Registry, route, result string, eligible, firstOutput float64) {
	t.Helper()
	routeOnly := map[string]string{"route": route}
	if got := total(t, registry, "gateflow_http_requests_total", map[string]string{"route": route, "result": result}); got != 1 {
		t.Errorf("requests{result=%q} = %v, want 1", result, got)
	}
	if got := total(t, registry, "gateflow_http_requests_total", routeOnly); got != 1 {
		t.Errorf("requests of every result = %v, want 1", got)
	}
	if got := total(t, registry, "gateflow_gateway_active_admitted_requests", nil); got != 0 {
		t.Errorf("active admitted = %v after the request ended, want 0", got)
	}
	if got := total(t, registry, "gateflow_gateway_eligible_arrivals_total", nil); got != eligible {
		t.Errorf("eligible arrivals = %v, want %v", got, eligible)
	}
	if got := total(t, registry, "gateflow_gateway_first_output_seconds", routeOnly); got != firstOutput {
		t.Errorf("first output samples = %v, want %v", got, firstOutput)
	}
}

// Every way an admitted call ends gives its outcome as the result, counts it as
// eligible, and leaves no active request once it has finished. Only a call whose
// model output reached the client has a first-output sample.
func TestObservedAdmittedCalls(t *testing.T) {
	for _, e := range endpoints {
		cases := []struct {
			name        string
			upstream    http.HandlerFunc // nil: a closed port
			cancel      bool             // the client leaves after the first event
			result      string
			firstOutput float64
		}{
			{"completed", streamOf(e.metadata, e.output, e.end), false, OutcomeCompleted, 1},
			{"stream cut before its end", streamOf(e.metadata, e.output), false, OutcomeInterrupted, 1},
			{"upstream status", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}, false, OutcomeUpstreamError, 0},
			{"not dispatched", nil, false, OutcomeNotDispatched, 0},
			{"client cancelled", func(w http.ResponseWriter, r *http.Request) {
				startStream(w)
				sendEvents(w, e.metadata)
				<-r.Context().Done()
			}, true, OutcomeClientCancelled, 0},
		}
		for _, c := range cases {
			t.Run(e.route+"/"+c.name, func(t *testing.T) {
				channel := Channel{BaseURL: closedPortURL(t)}
				if c.upstream != nil {
					channel = startUpstream(t, c.upstream)
				}
				gw := e.start(t, channel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				resp := e.send(t, gw, ctx, e.body(testModelAlias, true), nil)
				if c.cancel {
					if _, err := sse.NewReader(resp.Body).Next(); err != nil {
						t.Fatal(err)
					}
					cancel()
				} else {
					readAllBody(t, resp)
				}
				gw.admission.result(t)
				gw.Close() // waits for the handler to finish
				checkObserved(t, gw.metrics, e.route, c.result, 1, c.firstOutput)
			})
		}
	}
}

// streamOf returns an upstream that streams events and then ends the response.
func streamOf(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, events...)
	}
}

// An admitted call is active while it waits for its first output and while it
// streams, and its first output is observed once the output event is flushed, not
// for the metadata before it.
func TestObservedActiveAndFirstOutput(t *testing.T) {
	for _, e := range endpoints {
		t.Run(e.route, func(t *testing.T) {
			toOutput, toEnd := newGate(t), newGate(t)
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				// A test that fails early disconnects, which ends the wait.
				passed := func(g *gate) bool {
					select {
					case <-g.ch:
						return true
					case <-r.Context().Done():
						return false
					}
				}
				startStream(w)
				sendEvents(w, e.metadata)
				if passed(toOutput) {
					sendEvents(w, e.output)
				}
				if passed(toEnd) {
					sendEvents(w, e.end)
				}
			})
			gw := e.start(t, channel)
			resp := e.send(t, gw, context.Background(), e.body(testModelAlias, true), nil)
			events := sse.NewReader(resp.Body)
			firstOutput := func() float64 {
				return total(t, gw.metrics, "gateflow_gateway_first_output_seconds", map[string]string{"route": e.route})
			}

			if _, err := events.Next(); err != nil {
				t.Fatal(err)
			}
			if got := total(t, gw.metrics, "gateflow_gateway_active_admitted_requests", nil); got != 1 {
				t.Errorf("active admitted = %v while waiting for output, want 1", got)
			}
			if got := firstOutput(); got != 0 {
				t.Errorf("first output samples = %v after metadata only, want 0", got)
			}
			toOutput.open()
			if _, err := events.Next(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "a first-output sample", func() bool { return firstOutput() == 1 })
			toEnd.open()
			readAllBody(t, resp)
			gw.Close()
			checkObserved(t, gw.metrics, e.route, OutcomeCompleted, 1, 1)
		})
	}
}

// waitFor polls cond, which the handler makes true right after the client has read
// what it waits on.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("no %s within 5 s", what)
}

// A panic in the relay ends the call interrupted and releases its admission.
func TestObservedPanicReleasesTheAdmission(t *testing.T) {
	for _, e := range endpoints {
		t.Run(e.route, func(t *testing.T) {
			admission := newStubAdmission(Channel{BaseURL: closedPortURL(t), CredentialEnv: credentialEnv})
			observer, registry := newTestObserver(t, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			getenv := func(string) string { panic("getenv failed") }
			handler := e.newHandler(Config{Getenv: getenv, Observer: observer}, admission,
				slog.New(slog.NewJSONHandler(io.Discard, nil)))
			req := httptest.NewRequest(http.MethodPost, e.route, strings.NewReader(e.body(testModelAlias, false)))
			req.Header.Set("x-api-key", gatewayKeyValue)

			func() {
				defer func() {
					if recover() == nil {
						t.Error("the panic did not reach the server")
					}
				}()
				handler.ServeHTTP(httptest.NewRecorder(), req)
			}()
			checkObserved(t, registry, e.route, OutcomeInterrupted, 1, 0)
		})
	}
}

// A refused call, by the checks or by admission, ends with the result of its status,
// and is neither eligible nor admitted.
func TestObservedRefusals(t *testing.T) {
	refusals := []struct {
		name   string
		body   string
		noKey  bool
		refuse *PublicError
		result string
	}{
		{"body over 2 MiB", padded(maxBodyBytes + 1), false, nil, "invalid_request"},
		{"malformed body", `{"model":`, false, nil, "invalid_request"},
		{"no key", "", true, nil, "authentication_denied"},
		{"invalid key", "", false, refusal(http.StatusUnauthorized, anthropic.ErrorTypeAuthentication), "authentication_denied"},
		{"model not allowed", "", false, refusal(http.StatusForbidden, anthropic.ErrorTypePermission), "permission_denied"},
		{"balance too low", "", false, errBilling, "insufficient_balance"},
		{"dependency unavailable", "", false, refusal(http.StatusServiceUnavailable, anthropic.ErrorTypeAPI), "dependency_error"},
		{"channel misconfigured", "", false, refusal(http.StatusInternalServerError, anthropic.ErrorTypeAPI), "internal_error"},
	}
	for _, e := range endpoints {
		for _, r := range refusals {
			t.Run(e.route+"/"+r.name, func(t *testing.T) {
				admission := newStubAdmission(Channel{BaseURL: closedPortURL(t)})
				admission.refuse = r.refuse
				observer, registry := newTestObserver(t, slog.New(slog.NewJSONHandler(io.Discard, nil)))
				handler := e.newHandler(Config{Getenv: testEnv, Observer: observer}, admission,
					slog.New(slog.NewJSONHandler(io.Discard, nil)))
				body := r.body
				if body == "" {
					body = e.body(testModelAlias, true)
				}
				req := httptest.NewRequest(http.MethodPost, e.route, strings.NewReader(body))
				if !r.noKey {
					req.Header.Set("Authorization", "Bearer "+gatewayKeyValue)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code < 400 {
					t.Fatalf("status %d, want a refusal", rec.Code)
				}
				checkObserved(t, registry, e.route, r.result, 0, 0)
			})
		}
	}
}

func refusal(status int, errorType string) *PublicError {
	return &PublicError{Status: status, Type: errorType, Message: "A fixed message."}
}

// The request line of each call holds its result, status, model alias and first
// output, and never the model a client named that is not an alias.
func TestRequestLineFields(t *testing.T) {
	const unlisted = "unlisted-model-canary-6e21"
	for _, e := range endpoints {
		t.Run(e.route, func(t *testing.T) {
			gw := e.start(t, startUpstream(t, streamOf(e.metadata, e.output, e.end)))
			listed := e.send(t, gw, context.Background(), e.body(testModelAlias, true), nil)
			readAllBody(t, listed)
			other := e.send(t, gw, context.Background(), e.body(unlisted, true), nil)
			readAllBody(t, other)
			gw.Close()

			lines := requestLines(t, gw.logs.String())
			want := map[string]map[string]any{
				listed.Header.Get("request-id"): {"model": testModelAlias},
				other.Header.Get("request-id"):  {"model": "other"},
			}
			for id, fields := range want {
				line := lines[id]
				fields["result"], fields["status"], fields["route"] = "completed", float64(200), e.route
				fields["tenant_slot"] = "unverified" // the stand-in admission verifies no key
				for name, value := range fields {
					if line[name] != value {
						t.Errorf("request %s: %s = %v, want %v", id, name, line[name], value)
					}
				}
				if _, ok := line["first_output_ms"].(float64); !ok {
					t.Errorf("request %s has no first_output_ms: %v", id, line)
				}
			}
			if strings.Contains(gw.logs.String(), unlisted) {
				t.Errorf("the logs hold the client's model name:\n%s", gw.logs)
			}
		})
	}
}

// requestLines returns the request log lines by request id.
func requestLines(t *testing.T, logs string) map[string]map[string]any {
	t.Helper()
	lines := map[string]map[string]any{}
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if line["msg"] == "request" {
			id, _ := line["request_id"].(string)
			lines[id] = line
		}
	}
	return lines
}
