package mock

import (
	"fmt"
	"strings"
	"time"

	"github.com/bibi45c/gateflow/pkg/config"
)

// loadModel is the scenario for load tests: one long text block, shaped by Load.
const loadModel = "mock-load"

// maxLoadTextBytes bounds Events × EventBytes, so a mistyped variable cannot make
// every request build a huge reply.
const maxLoadTextBytes = 32 << 20

// Load shapes the stream of mock-load. The gateway does not forward X-Mock-*
// headers, so only the environment sets it.
type Load struct {
	// Events is the number of text deltas.
	Events int
	// Interval is the wait before each event after the first, in place of
	// Timing.EventInterval; Timing.FirstEventDelay still comes first.
	Interval time.Duration
	// EventBytes is the size of the text of each delta.
	EventBytes int
}

// DefaultLoad is the baseline of proposal section 5.6: 200 deltas of 256 bytes, ten
// events a second, so a stream lasts about 20 s.
var DefaultLoad = Load{Events: 200, Interval: 100 * time.Millisecond, EventBytes: 256}

// loadSettings lists the environment variables that set Load, with the smallest
// value each accepts.
var loadSettings = []struct {
	env string
	min int
	set func(*Load, int)
}{
	{"MOCK_LOAD_EVENTS", 1, func(l *Load, n int) { l.Events = n }},
	{"MOCK_LOAD_INTERVAL_MS", 0, func(l *Load, n int) {
		l.Interval = time.Duration(n) * time.Millisecond
	}},
	{"MOCK_LOAD_EVENT_BYTES", 1, func(l *Load, n int) { l.EventBytes = n }},
}

// loadFromEnv sets load from the environment. It fails when a value is not an
// integer of at least its minimum, or when the text would exceed maxLoadTextBytes.
func loadFromEnv(load *Load) error {
	for _, setting := range loadSettings {
		value := config.Env(setting.env, "")
		if value == "" {
			continue
		}
		n, ok := parseCount(value)
		if !ok || n < setting.min {
			return fmt.Errorf("%s: %q is not an integer of at least %d", setting.env, value,
				setting.min)
		}
		setting.set(load, n)
	}
	if size := int64(load.Events) * int64(load.EventBytes); size > maxLoadTextBytes {
		return fmt.Errorf("MOCK_LOAD_EVENTS × MOCK_LOAD_EVENT_BYTES: %d bytes exceeds %d",
			size, maxLoadTextBytes)
	}
	return nil
}

// forModel returns t with mock-load's interval when model is mock-load, else t.
func (t Timing) forModel(model string) Timing {
	if model == loadModel {
		t.EventInterval = t.Load.Interval
	}
	return t
}

func loadReply(model string, load Load) reply {
	return textReply(model, loadText(load))
}

// loadText returns load.Events words of load.EventBytes bytes, so that block.parts
// splits it into one delta per event. Word i repeats the i-th letter of the alphabet,
// cycling, so a reader can check the order; every word but the last ends with a
// space.
func loadText(load Load) string {
	var text strings.Builder
	text.Grow(load.Events * load.EventBytes)
	for i := range load.Events {
		letter := string(rune('a' + i%26))
		if i == load.Events-1 {
			text.WriteString(strings.Repeat(letter, load.EventBytes))
		} else {
			text.WriteString(strings.Repeat(letter, load.EventBytes-1) + " ")
		}
	}
	return text.String()
}
