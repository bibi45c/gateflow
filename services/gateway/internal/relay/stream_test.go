package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

var requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// The upstream sends one event and then blocks until the test opens the gate, so the
// client can only have that event if the gateway relayed it without waiting for more.
func TestStreamIsNotBuffered(t *testing.T) {
	blocked := newGate(t)
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, messageStart(`{"input_tokens":10,"output_tokens":1}`))
		select {
		case <-blocked.ch:
		case <-r.Context().Done():
			return
		}
		sendEvents(w, messageDelta(`{"output_tokens":3}`), messageStop())
	})
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	reader := anthropic.NewReader(resp.Body)
	first := make(chan anthropic.Event, 1)
	go func() {
		if e, err := reader.Next(); err == nil {
			first <- e
		}
	}()
	select {
	case e := <-first:
		if e.Type() != anthropic.EventMessageStart {
			t.Fatalf("first event type = %q, want message_start", e.Type())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive while the upstream was blocked")
	}

	blocked.open()
	rest := eventTypes(readEvents(t, reader))
	if want := []string{"message_delta", "message_stop"}; !slices.Equal(rest, want) {
		t.Errorf("later events = %q, want %q", rest, want)
	}
	checkResult(t, gw.admission.result(t), OutcomeCompleted, count(10), count(3))
}

// The recorded stream arrives in 7-byte pieces and must come back byte for byte, with
// the upstream's content type and a request id.
func TestStreamPassesBytesUnchanged(t *testing.T) {
	stream := fixture(t, "stream.sse")
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		for rest := stream; len(rest) > 0; {
			n := min(7, len(rest))
			sendEvents(w, string(rest[:n]))
			rest = rest[n:]
		}
	})
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	got := readAllBody(t, resp)
	if !bytes.Equal(got, stream) {
		t.Errorf("relayed stream differs from the upstream's:\n%s", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	if id := resp.Header.Get("request-id"); !requestIDPattern.MatchString(id) {
		t.Errorf("request-id = %q, want req_<uuid>", id)
	}
	// 472 input + 128 cache read + 0 cache creation; output from message_delta.
	checkResult(t, gw.admission.result(t), OutcomeCompleted, count(600), count(89))
}

// toolEvent holds the members of a content block event that a client needs to put a
// tool call together.
type toolEvent struct {
	Index        int `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

// toolCall puts together the tool call of a stream, as a client does.
func toolCall(t *testing.T, events []anthropic.Event) (id, name, input string) {
	t.Helper()
	var partial strings.Builder
	for _, e := range events {
		var block toolEvent
		switch e.Type() {
		case anthropic.EventContentBlockStart, anthropic.EventContentBlockDelta:
			if err := json.Unmarshal(e.Data, &block); err != nil {
				t.Fatal(err)
			}
		default:
			continue
		}
		if block.ContentBlock.Type == "tool_use" {
			id, name = block.ContentBlock.ID, block.ContentBlock.Name
		}
		if block.Delta.Type == "input_json_delta" {
			partial.WriteString(block.Delta.PartialJSON)
		}
	}
	return id, name, partial.String()
}

func TestToolCallRoundTrip(t *testing.T) {
	toolUse := fixture(t, "stream.sse")
	var mu sync.Mutex
	var received [][]byte
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, body)
		mu.Unlock()
		if !bytes.Contains(body, []byte(`"tool_result"`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(toolUse)
			return
		}
		startStream(w)
		sendEvents(w,
			messageStart(`{"input_tokens":80,"output_tokens":1}`),
			event(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			textDelta("It is 18 C and sunny in Paris."),
			event(`{"type":"content_block_stop","index":0}`),
			messageDelta(`{"output_tokens":11}`),
			messageStop())
	})
	gw := startGateway(t, channel)

	first := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	events := readEvents(t, anthropic.NewReader(first.Body))
	wantTypes := []string{"", "message_start", "ping", "content_block_start",
		"content_block_delta", "", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := eventTypes(events); !slices.Equal(got, wantTypes) {
		t.Fatalf("event types = %q, want %q", got, wantTypes)
	}
	id, name, input := toolCall(t, events)
	if id != "toolu_01Fixture" || name != "get_weather" || input != `{"location": "Paris, France"}` {
		t.Fatalf("tool call = %q %q %q", id, name, input)
	}

	second := fmt.Sprintf(`{"model":"mock-small","max_tokens":256,"stream":true,"messages":[`+
		`{"role":"user","content":"What is the weather in Paris?"},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"18 C, sunny"}]}]}`,
		id, name, input, id)
	resp := gw.post(t, context.Background(), second, nil)
	answer := readEvents(t, anthropic.NewReader(resp.Body))
	wantTypes = []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop"}
	if got := eventTypes(answer); !slices.Equal(got, wantTypes) {
		t.Errorf("answer event types = %q, want %q", got, wantTypes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || string(received[1]) != second {
		t.Errorf("the upstream did not get the tool result unchanged: %q", received)
	}
	checkResult(t, gw.admission.result(t), OutcomeCompleted, count(600), count(89))
	checkResult(t, gw.admission.result(t), OutcomeCompleted, count(80), count(11))
}

func oversizedDelta() string {
	return textDelta(strings.Repeat("a", maxEventBytes))
}

func TestOversizedEventBeforeOutputGets502(t *testing.T) {
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, oversizedDelta())
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
	checkResult(t, gw.admission.result(t), OutcomeInterrupted, nil, nil)
}

func TestOversizedEventAfterOutputEndsTheStream(t *testing.T) {
	start := messageStart(`{"input_tokens":15,"output_tokens":1}`)
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, start, oversizedDelta(), messageStop())
	})
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	body := readAllBody(t, resp)
	if want := start + gatewayErrorEvent(t); string(body) != want {
		t.Errorf("body = %.300q, want %q", body, want)
	}
	checkResult(t, gw.admission.result(t), OutcomeInterrupted, count(15), nil)
}

// An upstream error event after the stream started is relayed unchanged, and the
// stream ends there.
func TestUpstreamErrorEventIsRelayedUnchanged(t *testing.T) {
	start := messageStart(`{"input_tokens":20,"output_tokens":1}`)
	upstreamError := "event: error\r\ndata: {\"type\":\"error\",\"error\":" +
		"{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\r\n\r\n"
	channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		startStream(w)
		sendEvents(w, start, upstreamError, textDelta("late"), messageStop())
	})
	gw := startGateway(t, channel)

	resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
	if body := readAllBody(t, resp); string(body) != start+upstreamError {
		t.Errorf("body = %q, want %q", body, start+upstreamError)
	}
	checkResult(t, gw.admission.result(t), OutcomeInterrupted, count(20), nil)
}

// Any other failure after the stream started gets the gateway's own error event.
func TestStreamFailureAfterOutputSendsErrorEvent(t *testing.T) {
	start := messageStart(`{"input_tokens":20,"output_tokens":1}`)
	tests := []struct {
		name string
		end  func()
	}{
		{"connection drops", func() { panic(http.ErrAbortHandler) }},
		{"stream ends without message_stop", func() {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				startStream(w)
				sendEvents(w, start, textDelta("Hel"))
				tt.end()
			})
			gw := startGateway(t, channel)

			resp := gw.post(t, context.Background(), messagesBody("mock-small", true), nil)
			want := start + textDelta("Hel") + gatewayErrorEvent(t)
			if body := readAllBody(t, resp); string(body) != want {
				t.Errorf("body = %q, want %q", body, want)
			}
			checkResult(t, gw.admission.result(t), OutcomeInterrupted, count(20), nil)
		})
	}
}

func TestEventReaderCapsEachEvent(t *testing.T) {
	const block = "data: 12345\n\n" // 13 bytes
	for _, limit := range []int64{13, 12} {
		reader := newEventReader(iotest.OneByteReader(strings.NewReader(block)), limit)
		e, err := reader.Next()
		switch {
		case limit == 13 && (err != nil || string(e.Raw) != block):
			t.Errorf("limit 13: got %q, %v; want the block", e.Raw, err)
		case limit == 12 && !errors.Is(err, errEventTooLarge):
			t.Errorf("limit 12: err = %v, want errEventTooLarge", err)
		}
	}
}

// A long event stops the reading soon after the limit, instead of being held whole.
func TestEventReaderStopsReadingALongEvent(t *testing.T) {
	const limit = 20
	stream := "data: a\n\n" + "data: " + strings.Repeat("b", 1000) + "\n\n"
	reader := newEventReader(iotest.OneByteReader(strings.NewReader(stream)), limit)
	first, err := reader.Next()
	if err != nil || string(first.Raw) != "data: a\n\n" {
		t.Fatalf("first event = %q, %v", first.Raw, err)
	}
	if _, err := reader.Next(); !errors.Is(err, errEventTooLarge) {
		t.Fatalf("err = %v, want errEventTooLarge", err)
	}
	if read := reader.source.read - int64(len(first.Raw)); read > limit+1 {
		t.Errorf("read %d bytes of the long event, want at most %d", read, limit+1)
	}
}
