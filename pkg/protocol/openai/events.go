package openai

import (
	"encoding/json"
	"fmt"
)

// Event types of a Responses stream. Each is the "type" member of an event's data, and
// Responses servers also send it as the event: field.
const (
	EventCreated                    = "response.created"
	EventInProgress                 = "response.in_progress"
	EventCompleted                  = "response.completed"
	EventIncomplete                 = "response.incomplete"
	EventFailed                     = "response.failed"
	EventOutputItemAdded            = "response.output_item.added"
	EventOutputItemDone             = "response.output_item.done"
	EventContentPartAdded           = "response.content_part.added"
	EventContentPartDone            = "response.content_part.done"
	EventOutputTextDelta            = "response.output_text.delta"
	EventOutputTextDone             = "response.output_text.done"
	EventFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	EventFunctionCallArgumentsDone  = "response.function_call_arguments.done"
	EventError                      = "error"
)

// EndKind tells apart the events that end a Responses stream with the final response
// object, which carries the usage. It only classifies: which outcome each one settles
// as is the gateway's rule in api/metering.yaml.
type EndKind int

const (
	// EndCompleted is response.completed.
	EndCompleted EndKind = iota + 1
	// EndIncomplete is response.incomplete, a response that stopped early.
	EndIncomplete
	// EndFailed is response.failed.
	EndFailed
)

// EndOf reports whether name, an event: field or the type of an event's data, names
// an event that ends the stream with the final response, and which one. An error event
// carries no response, so EndOf reports false for it.
func EndOf(name string) (EndKind, bool) {
	switch name {
	case EventCompleted:
		return EndCompleted, true
	case EventIncomplete:
		return EndIncomplete, true
	case EventFailed:
		return EndFailed, true
	}
	return 0, false
}

// Usage holds the token counts of a Responses usage object. A count that was absent or
// null is nil; a count reported as 0 points to 0. On the wire, input_tokens already
// counts the cached tokens, and output_tokens the reasoning tokens.
type Usage struct {
	InputTokens  *int64
	OutputTokens *int64
	TotalTokens  *int64
	// CachedTokens is input_tokens_details.cached_tokens.
	CachedTokens *int64
	// ReasoningTokens is output_tokens_details.reasoning_tokens.
	ReasoningTokens *int64
}

// EventUsage reads response.usage from the data of a response.completed,
// response.incomplete or response.failed event, and fails on any other event type. A
// missing response or usage gives a Usage with every count nil.
func EventUsage(data []byte) (Usage, error) {
	var eventType string
	var response json.RawMessage
	if err := decodeMembers(data, map[string]any{"type": &eventType, "response": &response}); err != nil {
		return Usage{}, err
	}
	if _, ok := EndOf(eventType); !ok {
		return Usage{}, fmt.Errorf("%w: event type is not %s, %s or %s",
			ErrMalformed, EventCompleted, EventIncomplete, EventFailed)
	}
	if isAbsent(response) {
		return Usage{}, nil
	}
	return ResponseUsage(response)
}

// ResponseUsage reads usage from a response object: a non-streaming Responses body, or
// the response member of an event. A missing usage gives a Usage with every count nil.
func ResponseUsage(body []byte) (Usage, error) {
	var usage json.RawMessage
	if err := decodeMembers(body, map[string]any{"usage": &usage}); err != nil {
		return Usage{}, err
	}
	return readUsage(usage)
}

func readUsage(raw json.RawMessage) (Usage, error) {
	var usage Usage
	if isAbsent(raw) {
		return usage, nil
	}
	var inputDetails, outputDetails json.RawMessage
	err := decodeMembers(raw, map[string]any{
		"input_tokens":          &usage.InputTokens,
		"output_tokens":         &usage.OutputTokens,
		"total_tokens":          &usage.TotalTokens,
		"input_tokens_details":  &inputDetails,
		"output_tokens_details": &outputDetails,
	})
	if err != nil {
		return Usage{}, err
	}
	if err := readDetail(inputDetails, "cached_tokens", &usage.CachedTokens); err != nil {
		return Usage{}, err
	}
	if err := readDetail(outputDetails, "reasoning_tokens", &usage.ReasoningTokens); err != nil {
		return Usage{}, err
	}
	return usage, nil
}

// readDetail reads the count called name from a details object, unless the object is
// absent or null.
func readDetail(raw json.RawMessage, name string, count **int64) error {
	if isAbsent(raw) {
		return nil
	}
	return decodeMembers(raw, map[string]any{name: count})
}

func isAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
