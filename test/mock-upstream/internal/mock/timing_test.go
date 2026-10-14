package mock

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

const delay = 150 * time.Millisecond

// firstEventAfter returns how long a stream request takes to deliver its first
// event, and how long it takes to end.
func firstEventAfter(t *testing.T, timing Timing, header map[string]string) (first, total time.Duration) {
	t.Helper()
	srv := newServer(t, timing)
	body := requestBody(t, "mock-text", true, nil, userMessages)
	start := time.Now()
	resp := post(t, srv, body, header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	reader := anthropic.NewReader(resp.Body)
	for i := 0; ; i++ {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return first, time.Since(start)
		}
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = time.Since(start)
			if event.Type() != anthropic.EventMessageStart {
				t.Errorf("first event %q", event.Type())
			}
		}
	}
}

func TestFirstEventDelay(t *testing.T) {
	if first, _ := firstEventAfter(t, Timing{FirstEventDelay: delay}, nil); first < delay {
		t.Errorf("first event after %v, want at least %v", first, delay)
	}
}

func TestEventInterval(t *testing.T) {
	interval := 10 * time.Millisecond
	events := len(streamEvents(textReply("mock-text", textAnswer), 0))
	_, total := firstEventAfter(t, Timing{EventInterval: interval}, nil)
	if want := time.Duration(events-1) * interval; total < want {
		t.Errorf("stream took %v, want at least %v", total, want)
	}
}

func TestHeadersOverrideTiming(t *testing.T) {
	header := map[string]string{"X-Mock-First-Event-Delay-Ms": "150"}
	if first, _ := firstEventAfter(t, Timing{}, header); first < delay {
		t.Errorf("first event after %v, want at least %v", first, delay)
	}
	slow := Timing{FirstEventDelay: 10 * time.Second, EventInterval: 10 * time.Second}
	header = map[string]string{"X-Mock-First-Event-Delay-Ms": "0", "X-Mock-Event-Interval-Ms": "0"}
	if _, total := firstEventAfter(t, slow, header); total > 5*time.Second {
		t.Errorf("stream took %v with the delays set to 0", total)
	}
}

func TestInvalidHeadersAreIgnored(t *testing.T) {
	for _, value := range []string{"-1", "abc", "1.5", "+20", ""} {
		header := map[string]string{"X-Mock-First-Event-Delay-Ms": value}
		if first, _ := firstEventAfter(t, Timing{FirstEventDelay: delay}, header); first < delay {
			t.Errorf("header %q: first event after %v, want the configured %v", value, first, delay)
		}
	}
}

func TestTimingFromEnv(t *testing.T) {
	t.Setenv("MOCK_FIRST_EVENT_DELAY_MS", "")
	t.Setenv("MOCK_EVENT_INTERVAL_MS", "")
	t.Setenv("MOCK_FAIL_AFTER_EVENTS", "")
	clearLoadEnv(t)
	if got, err := TimingFromEnv(); err != nil || got != DefaultTiming {
		t.Errorf("unset: %+v, %v; want %+v", got, err, DefaultTiming)
	}
	t.Setenv("MOCK_FIRST_EVENT_DELAY_MS", "250")
	t.Setenv("MOCK_EVENT_INTERVAL_MS", "20")
	t.Setenv("MOCK_FAIL_AFTER_EVENTS", "0")
	want := Timing{FirstEventDelay: 250 * time.Millisecond, EventInterval: 20 * time.Millisecond,
		Load: DefaultLoad}
	if got, err := TimingFromEnv(); err != nil || got != want {
		t.Errorf("set: %+v, %v; want %+v", got, err, want)
	}
	for _, value := range []string{"-5", "soon", "99999999999"} {
		t.Setenv("MOCK_EVENT_INTERVAL_MS", value)
		if _, err := TimingFromEnv(); err == nil {
			t.Errorf("MOCK_EVENT_INTERVAL_MS=%q: no error", value)
		}
	}
}
