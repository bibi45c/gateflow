package relay

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// Streams recorded on Ollama 0.40.3 arrive in 7-byte pieces and must come back byte
// for byte, with the upstream's content type and a request id. Input is input_tokens
// alone: the text stream's 10 cached tokens are already part of its 11.
func TestResponsesStreamPassesBytesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		file          string
		input, output int64
	}{
		{"ollama-tool-stream.sse", 157, 19},
		{"ollama-text-stream.sse", 11, 3},
	} {
		t.Run(tc.file, func(t *testing.T) {
			stream := fixture(t, tc.file)
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				for rest := stream; len(rest) > 0; {
					n := min(7, len(rest))
					sendEvents(w, string(rest[:n]))
					rest = rest[n:]
				}
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			if got := readAllBody(t, resp); !bytes.Equal(got, stream) {
				t.Errorf("relayed stream differs from the upstream's:\n%s", got)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
				t.Errorf("content-type = %q", ct)
			}
			if id := resp.Header.Get("request-id"); !requestIDPattern.MatchString(id) {
				t.Errorf("request-id = %q, want req_<uuid>", id)
			}
			checkResult(t, gw.admission.result(t), OutcomeCompleted, count(tc.input), count(tc.output))
		})
	}
}

// The upstream sends one event and then blocks until the test opens the gate, so the
// client can only have that event if the gateway relayed it without waiting for more.
func TestResponsesStreamIsNotBuffered(t *testing.T) {
	blocked := newGate(t)
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, responseCreated(0))
		select {
		case <-blocked.ch:
		case <-r.Context().Done():
			return
		}
		sendEvents(w, outputTextDelta(1, "Hi"),
			responseEnd(openai.EventCompleted, 2, `{"input_tokens":10,"output_tokens":3}`))
	})
	gw := startResponsesGateway(t, channel)

	resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	reader := sse.NewReader(resp.Body)
	first := make(chan sse.Event, 1)
	go func() {
		if e, err := reader.Next(); err == nil {
			first <- e
		}
	}()
	select {
	case e := <-first:
		if e.Type() != openai.EventCreated {
			t.Fatalf("first event type = %q, want response.created", e.Type())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive while the upstream was blocked")
	}

	blocked.open()
	rest := eventTypes(readEvents(t, reader))
	if want := []string{openai.EventOutputTextDelta, openai.EventCompleted}; !slices.Equal(rest, want) {
		t.Errorf("later events = %q, want %q", rest, want)
	}
	checkResult(t, gw.admission.result(t), OutcomeCompleted, count(10), count(3))
}

// Each event that ends a Responses stream settles by the outcome rule of
// api/metering.yaml, on the usage of its response, and the stream ends after it.
func TestResponsesEndEvents(t *testing.T) {
	const usage = `{"input_tokens":800,"input_tokens_details":{"cached_tokens":600},"output_tokens":50,` +
		`"output_tokens_details":{"reasoning_tokens":7},"total_tokens":850}`
	for _, tc := range []struct {
		name          string
		end           string
		outcome       string
		input, output *int64
	}{
		{"completed, cached tokens not added", responseEnd(openai.EventCompleted, 2, usage),
			OutcomeCompleted, count(800), count(50)},
		{"incomplete", responseEnd(openai.EventIncomplete, 2, usage), OutcomeCompleted, count(800), count(50)},
		{"failed, charged on its usage", responseEnd(openai.EventFailed, 2, `{"input_tokens":800,"output_tokens":20}`),
			OutcomeInterrupted, count(800), count(20)},
		{"failed without usage", responseEnd(openai.EventFailed, 2, `null`), OutcomeInterrupted, nil, nil},
		{"completed with input 0 and output 0", responseEnd(openai.EventCompleted, 2, `{"input_tokens":0,"output_tokens":0}`),
			OutcomeCompleted, nil, count(0)},
		{"completed with negative counts", responseEnd(openai.EventCompleted, 2, `{"input_tokens":-1,"output_tokens":-5}`),
			OutcomeCompleted, nil, nil},
		{"completed with unreadable usage", responseEnd(openai.EventCompleted, 2, `{"input_tokens":"800"}`),
			OutcomeCompleted, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := responseCreated(0) + outputTextDelta(1, "Hi")
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				startStream(w)
				sendEvents(w, head, tc.end, outputTextDelta(3, "late"))
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			if body := readAllBody(t, resp); string(body) != head+tc.end {
				t.Errorf("body = %q, want the events up to the end event", body)
			}
			checkResult(t, gw.admission.result(t), tc.outcome, tc.input, tc.output)
		})
	}
}

// Usage comes only from the event that ends the stream, never from the response of an
// earlier event.
func TestResponsesUsageComesFromTheEndEvent(t *testing.T) {
	var o responsesObserver
	stream := responseEvent(openai.EventCreated, 0, `{"status":"in_progress","usage":{"input_tokens":5,"output_tokens":5}}`) +
		outputTextDelta(1, "Hi")
	for _, e := range readEvents(t, sse.NewReader(strings.NewReader(stream))) {
		if role := o.event(e); role != roleOutput {
			t.Errorf("%s: role %d, want output", e.Type(), role)
		}
	}
	if input, output := o.counts(); input != nil || output != nil {
		t.Errorf("counts = %s, %s before the end event; want null", showCount(input), showCount(output))
	}
}

// An upstream error event after the stream started is relayed unchanged, and the
// gateway closes the stream after it.
func TestResponsesUpstreamErrorEventEndsTheStream(t *testing.T) {
	head := responseCreated(0) + outputTextDelta(1, "Hel")
	upstreamError := "event: error\r\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"Overloaded\"," +
		"\"param\":null,\"sequence_number\":2}\r\n\r\n"
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, head, upstreamError, outputTextDelta(3, "late"),
			responseEnd(openai.EventCompleted, 4, `{"input_tokens":20,"output_tokens":2}`))
	})
	gw := startResponsesGateway(t, channel)

	resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
	if body := readAllBody(t, resp); string(body) != head+upstreamError {
		t.Errorf("body = %q, want %q", body, head+upstreamError)
	}
	checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)
}

// An error event or a response.failed that comes first is a failure before output:
// the client gets the gateway's 502 in the Responses error body, and the event's
// content reaches neither the client nor the logs. A response.failed is still charged
// on its usage.
func TestResponsesFailureBeforeOutputGets502(t *testing.T) {
	const canary = "failure-canary-4c1d"
	for _, tc := range []struct {
		name          string
		first         string
		input, output *int64
	}{
		{"error event", event(`{"type":"error","code":"server_error","message":"` + canary +
			`","param":null,"sequence_number":0}`), nil, nil},
		{"response.failed", responseEvent(openai.EventFailed, 0, `{"id":"resp_1","status":"failed",`+
			`"error":{"code":"server_error","message":"`+canary+`"},"usage":{"input_tokens":30,"output_tokens":0}}`),
			count(30), count(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				startStream(w)
				sendEvents(w, tc.first, responseCreated(1))
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			body := readAllBody(t, resp)
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", resp.StatusCode)
			}
			if want := responsesErrorJSON(openai.ErrorTypeAPI, upstreamFailedMessage, ""); string(body) != want {
				t.Errorf("body = %s, want %s", body, want)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			checkResult(t, gw.admission.result(t), OutcomeInterrupted, tc.input, tc.output)

			gw.Close() // waits for the handler to report and log
			if logs := gw.logs.String(); strings.Contains(logs, canary) {
				t.Errorf("the logs hold the upstream event:\n%s", logs)
			}
		})
	}
}

// Any other failure after the stream started gets the gateway's own error event,
// numbered one past the last event relayed; a comment after that event does not count.
func TestResponsesStreamFailureAfterOutputSendsErrorEvent(t *testing.T) {
	head := responseCreated(0) + outputTextDelta(1, "Hel") + outputTextDelta(2, "lo") + ": keep-alive\n\n"
	for _, tc := range []struct {
		name string
		end  func(w http.ResponseWriter)
	}{
		{"connection drops", func(http.ResponseWriter) { panic(http.ErrAbortHandler) }},
		{"stream ends without its end event", func(http.ResponseWriter) {}},
		{"connection drops inside an event", func(w http.ResponseWriter) {
			sendEvents(w, "event: response.output_text.delta\ndata: {\"type\":")
			panic(http.ErrAbortHandler)
		}},
		{"event over 1 MiB", func(w http.ResponseWriter) {
			sendEvents(w, outputTextDelta(3, strings.Repeat("a", maxEventBytes)),
				responseEnd(openai.EventCompleted, 4, `{"input_tokens":20,"output_tokens":2}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				startStream(w)
				sendEvents(w, head)
				tc.end(w)
			})
			gw := startResponsesGateway(t, channel)

			resp := gw.postResponses(t, context.Background(), responsesBody("mock-small", true), nil)
			want := head + responsesGatewayErrorEvent(3)
			if body := readAllBody(t, resp); string(body) != want {
				t.Errorf("body = %.300q, want %q", body, want)
			}
			checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)
		})
	}
}

func TestNextSequenceNumber(t *testing.T) {
	for data, want := range map[string]int64{
		`{"type":"response.output_text.delta","sequence_number":12}`: 13,
		`{"sequence_number":0,"type":"response.created"}`:            1,
		`{"type":"response.created"}`:                                0,
		`{"type":"x","sequence_number":null}`:                        0,
		`{"type":"x","sequence_number":"5"}`:                         0,
		`{"type":"x","sequence_number":1.5}`:                         0,
		`{"type":"x","Sequence_Number":5}`:                           0,
		`{"type":"x","response":{"sequence_number":5}}`:              0,
		`{"type":"x","sequence_number":9223372036854775807}`:         9223372036854775807,
		`not json`: 0,
		``:         0,
	} {
		if got := nextSequenceNumber([]byte(data)); got != want {
			t.Errorf("%s: got %d, want %d", data, got, want)
		}
	}
}

// When the client disconnects mid-stream, the gateway cancels the upstream request
// and reports client_cancelled once.
func TestResponsesClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, responseCreated(0))
		<-r.Context().Done()
		close(cancelled)
	})
	gw := startResponsesGateway(t, channel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := gw.postResponses(t, ctx, responsesBody("mock-small", true), nil)
	if _, err := sse.NewReader(resp.Body).Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
	checkResult(t, gw.admission.result(t), OutcomeClientCancelled, nil, nil)
	gw.Close() // waits for the handler to report
	if n := gw.admission.reportCount(); n != 1 {
		t.Errorf("reported %d times, want 1", n)
	}
}
