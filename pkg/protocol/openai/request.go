package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/bibi45c/gateflow/pkg/protocol/internal/jsonobj"
)

// ErrMalformed reports JSON that this package cannot read: not one JSON object, a
// member read twice, or a member of the wrong JSON type. The errors that wrap it name
// the member but never quote the input.
var ErrMalformed = errors.New("openai: malformed JSON")

// Request holds the members of a Responses request body that the gateway reads.
type Request struct {
	// Model is the model member, or "" when it is absent or null.
	Model string
	// Stream is the stream member, or false when it is absent or null.
	Stream bool
	// MaxOutputTokens is the max_output_tokens member, or nil when it is absent or
	// null.
	MaxOutputTokens *int64
	// PreviousResponseID is the previous_response_id member, or nil when it is absent
	// or null. An empty string is present, so it is not nil.
	PreviousResponseID *string
	// Tools holds the elements of the tools member in order, or nil when it is absent
	// or null. An empty array gives an empty slice that is not nil.
	Tools []Tool
}

// Tool holds the members of one element of tools that the gateway reads.
type Tool struct {
	// Type is the type member, such as "function", or "" when it is absent or null.
	Type string
	// Name is the name member, or "" when it is absent or null, as for a built-in tool.
	Name string
	// Raw is the element exactly as it appears in the body.
	Raw json.RawMessage
}

// ParseRequest reads model, stream, max_output_tokens, previous_response_id and tools
// from a Responses request body. It matches member names exactly and treats null as
// absent. The other members are only checked for JSON syntax: nothing is re-encoded,
// so callers forward body unchanged, or change only max_output_tokens with
// SetMaxOutputTokens. Whether a value is allowed is for the gateway to decide.
//
// The error wraps ErrMalformed when body is not one JSON object in UTF-8, when one of
// the five members appears twice, or when model is not a string, stream not a boolean,
// max_output_tokens not an integer that fits in int64, previous_response_id not a
// string, or tools not an array of objects whose type and name are strings.
func ParseRequest(body []byte) (Request, error) {
	if !utf8.Valid(body) {
		return Request{}, fmt.Errorf("%w: not valid UTF-8", ErrMalformed)
	}
	var req Request
	var tools []json.RawMessage
	err := decodeMembers(body, map[string]any{
		"model":                &req.Model,
		"stream":               &req.Stream,
		"max_output_tokens":    &req.MaxOutputTokens,
		"previous_response_id": &req.PreviousResponseID,
		"tools":                &tools,
	})
	if err != nil {
		return Request{}, err
	}
	if req.Tools, err = readTools(tools); err != nil {
		return Request{}, err
	}
	return req, nil
}

// readTools reads type and name from each element of tools.
func readTools(elements []json.RawMessage) ([]Tool, error) {
	if elements == nil {
		return nil, nil
	}
	tools := make([]Tool, len(elements))
	for i, raw := range elements {
		tools[i].Raw = raw
		err := decodeMembers(raw, map[string]any{"type": &tools[i].Type, "name": &tools[i].Name})
		if err != nil {
			return nil, fmt.Errorf("%w, in element %d of member \"tools\"", err, i)
		}
	}
	return tools, nil
}

// SetMaxOutputTokens returns a copy of a Responses request body with its
// max_output_tokens member set to n. When the member holds null or any other value,
// only that value is replaced; when the body has no such member, it is added after the
// last member. Every other byte stays as it was. Which n to set is for the gateway to
// decide.
//
// SetMaxOutputTokens checks the body's JSON as ParseRequest does, but not the types of
// the other members. The error wraps ErrMalformed when body is not one JSON object in
// UTF-8 or holds max_output_tokens twice.
func SetMaxOutputTokens(body []byte, n int64) ([]byte, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("%w: not valid UTF-8", ErrMalformed)
	}
	span, err := jsonobj.FindMember(body, "max_output_tokens", ErrMalformed)
	if err != nil {
		return nil, err
	}
	value := strconv.FormatInt(n, 10)
	if span.Found {
		return splice(body, span.Start, span.End, value), nil
	}
	member := `"max_output_tokens":` + value
	if !span.Empty {
		member = "," + member
	}
	return splice(body, span.Insert, span.Insert, member), nil
}

// splice returns a new slice holding body with body[start:end] replaced by s.
func splice(body []byte, start, end int, s string) []byte {
	out := make([]byte, 0, len(body)-(end-start)+len(s))
	out = append(out, body[:start]...)
	out = append(out, s...)
	return append(out, body[end:]...)
}

// decodeMembers decodes the members of the JSON object in data that targets names,
// strictly; see jsonobj.DecodeMembers. Its errors wrap ErrMalformed.
func decodeMembers(data []byte, targets map[string]any) error {
	return jsonobj.DecodeMembers(data, targets, ErrMalformed)
}
