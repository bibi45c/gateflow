package mock

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

// loadCases are the shapes the stream tests try: the default baseline without its
// pacing, the smallest, and one whose words cycle through the alphabet.
var loadCases = []Load{
	{Events: DefaultLoad.Events, EventBytes: DefaultLoad.EventBytes},
	{Events: 1, EventBytes: 1},
	{Events: 30, EventBytes: 7},
}

// wantLoadDelta is the text of delta i of n: the i-th letter, cycling, then a space
// unless it is the last delta.
func wantLoadDelta(load Load, i int) string {
	letter := string(rune('a' + i%26))
	if i == load.Events-1 {
		return strings.Repeat(letter, load.EventBytes)
	}
	return strings.Repeat(letter, load.EventBytes-1) + " "
}

// checkLoadDeltas checks the number, size and order of the text deltas of a stream.
func checkLoadDeltas(t *testing.T, load Load, deltas []string) {
	t.Helper()
	if len(deltas) != load.Events {
		t.Fatalf("%d deltas, want %d", len(deltas), load.Events)
	}
	for i, delta := range deltas {
		if len(delta) != load.EventBytes || delta != wantLoadDelta(load, i) {
			t.Fatalf("delta %d is %q (%d bytes), want %q", i, delta, len(delta), wantLoadDelta(load, i))
		}
	}
}

func TestLoadStreamsMessages(t *testing.T) {
	for _, load := range loadCases {
		srv := newServer(t, Timing{Load: load})
		body := requestBody(t, loadModel, true, nil, userMessages)
		events, err := readEvents(t, post(t, srv, body, nil))
		if err != nil {
			t.Fatalf("%+v: stream ended with %v", load, err)
		}
		want := []string{anthropic.EventMessageStart, anthropic.EventContentBlockStart}
		want = append(want, slices.Repeat([]string{anthropic.EventContentBlockDelta}, load.Events)...)
		want = append(want, anthropic.EventContentBlockStop, anthropic.EventMessageDelta,
			anthropic.EventMessageStop)
		if got := eventTypes(events); !slices.Equal(got, want) {
			t.Fatalf("%+v: events %v", load, got)
		}
		var deltas []string
		for _, event := range events {
			var data eventData
			if json.Unmarshal(event.Data, &data) == nil && data.Delta.Type == "text_delta" {
				deltas = append(deltas, data.Delta.Text)
			}
		}
		checkLoadDeltas(t, load, deltas)
		got := assemble(t, events)
		if got.messageID != "msg_mock_load_0" || got.stopReason != stopEndTurn ||
			count(got.usageDelta.OutputTokens) != int64(load.Events) {
			t.Errorf("%+v: id %q stop %q output tokens %d", load, got.messageID, got.stopReason,
				count(got.usageDelta.OutputTokens))
		}
	}
}

func TestLoadStreamsResponses(t *testing.T) {
	for _, load := range loadCases {
		srv := newResponsesServer(t, Timing{Load: load})
		body := responsesBody(t, loadModel, true, nil, userInput)
		events, err := readEvents(t, postResponses(t, srv, body, nil))
		if err != nil {
			t.Fatalf("%+v: stream ended with %v", load, err)
		}
		if got, want := eventTypes(events), responseEvents(textItemEvents(load.Events)); !slices.Equal(got, want) {
			t.Fatalf("%+v: events %v", load, got)
		}
		var deltas []string
		for _, event := range events {
			var data responseEventData
			if json.Unmarshal(event.Data, &data) == nil && data.Type == openai.EventOutputTextDelta {
				deltas = append(deltas, data.Delta)
			}
		}
		checkLoadDeltas(t, load, deltas)
		got := assembleResponse(t, events)
		if got.completed.ID != "resp_mock_load_0" {
			t.Errorf("%+v: response id %q", load, got.completed.ID)
		}
		checkUsage(t, "stream", got.usage, body, int64(load.Events))
	}
}

func TestLoadAnswersWithoutStreaming(t *testing.T) {
	load := Load{Events: 12, EventBytes: 5}
	srv := newServer(t, Timing{Load: load})
	resp := post(t, srv, requestBody(t, loadModel, false, nil, userMessages), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var message struct {
		Content []gotBlock `json:"content"`
		Usage   anthropic.Usage
	}
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	if len(message.Content) != 1 || message.Content[0].Text != loadText(load) ||
		count(message.Usage.OutputTokens) != int64(load.Events) {
		t.Errorf("content %+v output tokens %d", message.Content, count(message.Usage.OutputTokens))
	}
}

// loadStreamTime returns how long a mock-load stream takes over Messages, and its
// number of events.
func loadStreamTime(t *testing.T, timing Timing) (time.Duration, int) {
	t.Helper()
	srv := newServer(t, timing)
	start := time.Now()
	events, err := readEvents(t, post(t, srv, requestBody(t, loadModel, true, nil, userMessages), nil))
	if err != nil {
		t.Fatal(err)
	}
	return time.Since(start), len(events)
}

func TestLoadPacesEveryEventByItsInterval(t *testing.T) {
	interval := 40 * time.Millisecond
	// The slow EventInterval must not apply to mock-load.
	timing := Timing{EventInterval: 10 * time.Second, Load: Load{Events: 5, Interval: interval, EventBytes: 8}}
	total, events := loadStreamTime(t, timing)
	if want := time.Duration(events-1) * interval; total < want || total > 5*time.Second {
		t.Errorf("stream of %d events took %v, want at least %v", events, total, want)
	}
	timing.FirstEventDelay = delay
	if total, _ := loadStreamTime(t, timing); total < delay+time.Duration(events-1)*interval {
		t.Errorf("stream with a first event delay took %v", total)
	}
}

func TestLoadIntervalLeavesOtherScenariosAlone(t *testing.T) {
	slowLoad := Timing{Load: Load{Events: 1, Interval: 10 * time.Second, EventBytes: 1}}
	if _, total := firstEventAfter(t, slowLoad, nil); total > 5*time.Second {
		t.Errorf("mock-text took %v with only the load interval set", total)
	}
}

func clearLoadEnv(t *testing.T) {
	t.Helper()
	for _, setting := range loadSettings {
		t.Setenv(setting.env, "")
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearLoadEnv(t)
	if got, err := TimingFromEnv(); err != nil || got.Load != DefaultLoad {
		t.Errorf("unset: %+v, %v; want %+v", got.Load, err, DefaultLoad)
	}
	t.Setenv("MOCK_LOAD_EVENTS", "50")
	t.Setenv("MOCK_LOAD_INTERVAL_MS", "0")
	t.Setenv("MOCK_LOAD_EVENT_BYTES", "1024")
	want := Load{Events: 50, EventBytes: 1024}
	if got, err := TimingFromEnv(); err != nil || got.Load != want {
		t.Errorf("set: %+v, %v; want %+v", got.Load, err, want)
	}
	bad := map[string][]string{
		"MOCK_LOAD_EVENTS":      {"0", "-1", "many", "1.5"},
		"MOCK_LOAD_INTERVAL_MS": {"-1", "soon", "99999999999"},
		"MOCK_LOAD_EVENT_BYTES": {"0", "+256", "big"},
	}
	for env, values := range bad {
		for _, value := range values {
			clearLoadEnv(t)
			t.Setenv(env, value)
			if _, err := TimingFromEnv(); err == nil {
				t.Errorf("%s=%q: no error", env, value)
			}
		}
	}
	clearLoadEnv(t)
	t.Setenv("MOCK_LOAD_EVENTS", "1000000")
	t.Setenv("MOCK_LOAD_EVENT_BYTES", "1000")
	if _, err := TimingFromEnv(); err == nil {
		t.Error("a 1 GB text: no error")
	}
}
