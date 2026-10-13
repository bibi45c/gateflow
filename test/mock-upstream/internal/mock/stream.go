package mock

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// inputParts is how many input_json_delta events carry a tool's input.
const inputParts = 3

// The JSON shapes below follow the Anthropic Messages API; struct field order fixes
// the order of members in the output.

type usageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type messageJSON struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Content      []any     `json:"content"`
	StopReason   *string   `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	Usage        usageJSON `json:"usage"`
}

type textJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseJSON struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type messageStartJSON struct {
	Type    string      `json:"type"`
	Message messageJSON `json:"message"`
}

type blockStartJSON struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type blockDeltaJSON struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type textDeltaJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputDeltaJSON struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type blockStopJSON struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type messageDeltaJSON struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type typeJSON struct {
	Type string `json:"type"`
}

// event is one server-sent event: its name, which is also its data's type.
type event struct {
	name string
	data []byte
}

func newEvent(name string, data any) event {
	return event{name: name, data: mustMarshal(data)}
}

// parts splits the block into the pieces its deltas carry: a text word by word,
// a tool's input JSON into inputParts pieces cut between characters.
func (b block) parts() []string {
	if b.toolName == "" {
		return strings.SplitAfter(b.text, " ")
	}
	input := string(b.input)
	size := (len(input) + inputParts - 1) / inputParts
	var parts []string
	for input != "" {
		end := min(size, len(input))
		for end < len(input) && !utf8.RuneStart(input[end]) {
			end++
		}
		parts = append(parts, input[:end])
		input = input[end:]
	}
	return parts
}

// outputTokens is the number of deltas the reply streams.
func (rep reply) outputTokens() int64 {
	var n int64
	for _, b := range rep.blocks {
		n += int64(len(b.parts()))
	}
	return n
}

// messageBody returns the non-streaming response body of rep.
func messageBody(rep reply, inputTokens int64) messageJSON {
	content := make([]any, 0, len(rep.blocks))
	for _, b := range rep.blocks {
		if b.toolName == "" {
			content = append(content, textJSON{"text", b.text})
		} else {
			content = append(content, toolUseJSON{"tool_use", b.toolID, b.toolName, b.input})
		}
	}
	stopReason := rep.stopReason
	return messageJSON{ID: rep.id, Type: "message", Role: "assistant", Model: rep.model,
		Content: content, StopReason: &stopReason,
		Usage: usageJSON{InputTokens: inputTokens, OutputTokens: rep.outputTokens()}}
}

// streamEvents returns every event of the stream of rep, from message_start to
// message_stop.
func streamEvents(rep reply, inputTokens int64) []event {
	start := messageJSON{ID: rep.id, Type: "message", Role: "assistant", Model: rep.model,
		Content: []any{}, Usage: usageJSON{InputTokens: inputTokens}}
	events := []event{newEvent(anthropic.EventMessageStart,
		messageStartJSON{anthropic.EventMessageStart, start})}
	for i, b := range rep.blocks {
		var opening any = textJSON{"text", ""}
		if b.toolName != "" {
			opening = toolUseJSON{"tool_use", b.toolID, b.toolName, json.RawMessage("{}")}
		}
		events = append(events, newEvent(anthropic.EventContentBlockStart,
			blockStartJSON{anthropic.EventContentBlockStart, i, opening}))
		for _, part := range b.parts() {
			var delta any = textDeltaJSON{"text_delta", part}
			if b.toolName != "" {
				delta = inputDeltaJSON{"input_json_delta", part}
			}
			events = append(events, newEvent(anthropic.EventContentBlockDelta,
				blockDeltaJSON{anthropic.EventContentBlockDelta, i, delta}))
		}
		events = append(events, newEvent(anthropic.EventContentBlockStop,
			blockStopJSON{anthropic.EventContentBlockStop, i}))
	}
	end := messageDeltaJSON{Type: anthropic.EventMessageDelta}
	end.Delta.StopReason = rep.stopReason
	end.Usage.OutputTokens = rep.outputTokens()
	return append(events, newEvent(anthropic.EventMessageDelta, end),
		newEvent(anthropic.EventMessageStop, typeJSON{anthropic.EventMessageStop}))
}

// writeStream sends events as server-sent events, flushing each one, at the pace
// timing sets. A failing scenario sends only the first K events, and never
// message_stop, then closes the connection or sends an error event.
func writeStream(w http.ResponseWriter, r *http.Request, events []event, timing Timing, mode failure) {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	_ = flusher.Flush()
	if mode != failNone {
		events = events[:min(timing.FailAfterEvents, len(events)-1)]
	}
	if mode == failErrorEvent {
		body := anthropic.NewErrorBody(errorTypeOverloaded, overloadedMessage)
		events = append(events, newEvent(anthropic.EventError, body))
	}
	sse := anthropic.NewWriter(w)
	for i, e := range events {
		wait := timing.EventInterval
		if i == 0 {
			wait = timing.FirstEventDelay
		}
		if !sleep(r.Context(), wait) {
			return
		}
		if sse.WriteEvent(e.name, e.data) != nil || flusher.Flush() != nil {
			return
		}
	}
	if mode == failDrop {
		// Abort the handler: the server closes the connection without ending the
		// chunked body, so the client sees the stream break.
		panic(http.ErrAbortHandler)
	}
}

// sleep waits for d and reports false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
