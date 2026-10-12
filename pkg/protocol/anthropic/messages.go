package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// ErrMalformed reports JSON that this package cannot read: not one JSON object, a
// member read twice, or a member of the wrong JSON type. The errors that wrap it name
// the member but never quote the input.
var ErrMalformed = errors.New("anthropic: malformed JSON")

// Event types of a Messages stream. Each is the "type" member of an event's data, and
// the Anthropic API also sends it as the event: field.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// Request holds the members of a Messages request body that the gateway reads.
type Request struct {
	// Model is the model member, or "" when it is absent or null.
	Model string
	// MaxTokens is the max_tokens member, or nil when it is absent or null.
	MaxTokens *int64
	// Stream is the stream member, or false when it is absent or null.
	Stream bool
}

// ParseRequest reads model, max_tokens and stream from a Messages request body. It
// matches member names exactly and treats null as absent. The other members are only
// checked for JSON syntax: nothing is re-encoded, so callers forward body unchanged.
//
// The error wraps ErrMalformed when body is not one JSON object in UTF-8, when one of
// the three members appears twice, or when model is not a string, max_tokens not an
// integer that fits in int64, or stream not a boolean.
func ParseRequest(body []byte) (Request, error) {
	if !utf8.Valid(body) {
		return Request{}, fmt.Errorf("%w: not valid UTF-8", ErrMalformed)
	}
	var req Request
	err := decodeMembers(body, map[string]any{
		"model":      &req.Model,
		"max_tokens": &req.MaxTokens,
		"stream":     &req.Stream,
	})
	if err != nil {
		return Request{}, err
	}
	return req, nil
}

// Usage holds the token counts of a usage object. A count that was absent or null is
// nil; a count reported as 0 points to 0.
type Usage struct {
	InputTokens              *int64 `json:"input_tokens,omitempty"`
	OutputTokens             *int64 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
}

// MessageStartUsage reads message.usage from the data of a message_start event. A
// missing message or usage gives a Usage with every count nil.
func MessageStartUsage(data []byte) (Usage, error) {
	var message json.RawMessage
	if err := decodeEvent(data, EventMessageStart, "message", &message); err != nil {
		return Usage{}, err
	}
	if isAbsent(message) {
		return Usage{}, nil
	}
	return ResponseUsage(message)
}

// MessageDeltaUsage reads usage from the data of a message_delta event. A missing
// usage gives a Usage with every count nil.
func MessageDeltaUsage(data []byte) (Usage, error) {
	var usage json.RawMessage
	if err := decodeEvent(data, EventMessageDelta, "usage", &usage); err != nil {
		return Usage{}, err
	}
	return readUsage(usage)
}

// ResponseUsage reads usage from a non-streaming Messages response body, or from the
// message object of a message_start event. A missing usage gives a Usage with every
// count nil.
func ResponseUsage(body []byte) (Usage, error) {
	var usage json.RawMessage
	if err := decodeMembers(body, map[string]any{"usage": &usage}); err != nil {
		return Usage{}, err
	}
	return readUsage(usage)
}

// Type returns the "type" member of the event's data, or "" when the data is not a
// JSON object with a string type, as for a comment block.
func (e Event) Type() string {
	var eventType string
	if decodeMembers(e.Data, map[string]any{"type": &eventType}) != nil {
		return ""
	}
	return eventType
}

func readUsage(raw json.RawMessage) (Usage, error) {
	var usage Usage
	if isAbsent(raw) {
		return usage, nil
	}
	err := decodeMembers(raw, map[string]any{
		"input_tokens":                &usage.InputTokens,
		"output_tokens":               &usage.OutputTokens,
		"cache_read_input_tokens":     &usage.CacheReadInputTokens,
		"cache_creation_input_tokens": &usage.CacheCreationInputTokens,
	})
	if err != nil {
		return Usage{}, err
	}
	return usage, nil
}

// decodeEvent decodes the member called name of event data into target, and fails
// unless the data's type is want.
func decodeEvent(data []byte, want, name string, target any) error {
	var eventType string
	if err := decodeMembers(data, map[string]any{"type": &eventType, name: target}); err != nil {
		return err
	}
	if eventType != want {
		return fmt.Errorf("%w: event type is not %s", ErrMalformed, want)
	}
	return nil
}

func isAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// decodeMembers decodes each member of the JSON object in data that targets names
// into its target, matching names exactly. A null member leaves a target unchanged,
// except that a pointer target becomes nil. Other members are checked for syntax only.
// A named member that appears twice is an error, so that no other reader of the same
// bytes can see a different value.
func decodeMembers(data []byte, targets map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("%w: not a JSON object", ErrMalformed)
	}
	seen := make(map[string]bool, len(targets))
	for dec.More() {
		token, err := dec.Token()
		name, isName := token.(string)
		if err != nil || !isName {
			return fmt.Errorf("%w: invalid syntax", ErrMalformed)
		}
		target, wanted := targets[name]
		switch {
		case !wanted:
			target = new(json.RawMessage)
		case seen[name]:
			return fmt.Errorf("%w: member %q appears twice", ErrMalformed, name)
		default:
			seen[name] = true
		}
		if err := dec.Decode(target); err != nil {
			var typeErr *json.UnmarshalTypeError
			if wanted && errors.As(err, &typeErr) {
				return fmt.Errorf("%w: member %q has the wrong type", ErrMalformed, name)
			}
			return fmt.Errorf("%w: invalid syntax", ErrMalformed)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%w: invalid syntax", ErrMalformed)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the JSON object", ErrMalformed)
	}
	return nil
}
