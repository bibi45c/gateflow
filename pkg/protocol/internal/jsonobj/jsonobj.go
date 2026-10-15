// Package jsonobj reads chosen members of a JSON object strictly, for the wire codecs
// under pkg/protocol.
package jsonobj

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DecodeMembers decodes each member of the JSON object in data that targets names
// into its target, matching names exactly. A null member leaves a target unchanged,
// except that a pointer target becomes nil. Other members are checked for syntax only.
// A named member that appears twice is an error, so that no other reader of the same
// bytes can see a different value. So is a member whose name is not a target's but
// equals one under Unicode case folding, such as "MAX_TOKENS" for "max_tokens":
// readers that ignore case, as Go's encoding/json does, take it for the target. Every
// error wraps malformed and may name a member, but never quotes the input.
func DecodeMembers(data []byte, targets map[string]any, malformed error) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("%w: not a JSON object", malformed)
	}
	seen := make(map[string]bool, len(targets))
	for dec.More() {
		token, err := dec.Token()
		name, isName := token.(string)
		if err != nil || !isName {
			return fmt.Errorf("%w: invalid syntax", malformed)
		}
		target, wanted := targets[name]
		switch {
		case !wanted:
			for other := range targets {
				if strings.EqualFold(name, other) {
					return caseVariant(malformed, other)
				}
			}
			target = new(json.RawMessage)
		case seen[name]:
			return fmt.Errorf("%w: member %q appears twice", malformed, name)
		default:
			seen[name] = true
		}
		if err := dec.Decode(target); err != nil {
			var typeErr *json.UnmarshalTypeError
			if wanted && errors.As(err, &typeErr) {
				return fmt.Errorf("%w: member %q has the wrong type", malformed, name)
			}
			return fmt.Errorf("%w: invalid syntax", malformed)
		}
	}
	return checkEnd(dec, malformed)
}

// Span tells where the value of one member of a JSON object lies in its data, and
// where a new member would go.
type Span struct {
	// Found reports whether the object has the member; Start and End are set only then.
	Found bool
	// Start and End bound the member's value: data[Start:End].
	Start, End int
	// Insert is the end of the last member's value, or just after the "{" of an
	// object without members.
	Insert int
	// Empty reports whether the object has no members, so that a new member at Insert
	// needs no "," before it.
	Empty bool
}

// FindMember returns where the value of the member called name lies in the JSON
// object in data. Like DecodeMembers, it matches names exactly, fails when the member
// appears twice or a name equals it only under case folding, and checks the syntax of
// the whole object, but it does not check the member's type. Only the object's own
// members count, not those of nested objects. Every error wraps malformed.
func FindMember(data []byte, name string, malformed error) (Span, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return Span{}, fmt.Errorf("%w: not a JSON object", malformed)
	}
	span := Span{Insert: int(dec.InputOffset()), Empty: true}
	for dec.More() {
		token, err := dec.Token()
		member, isName := token.(string)
		if err != nil || !isName {
			return Span{}, fmt.Errorf("%w: invalid syntax", malformed)
		}
		if member == name && span.Found {
			return Span{}, fmt.Errorf("%w: member %q appears twice", malformed, name)
		}
		if member != name && strings.EqualFold(member, name) {
			return Span{}, caseVariant(malformed, name)
		}
		// A RawMessage holds the value without the spaces around it, and the decoder
		// stops right after the value, so the value ends at the decoder's offset.
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return Span{}, fmt.Errorf("%w: invalid syntax", malformed)
		}
		end := int(dec.InputOffset())
		if member == name {
			span.Found, span.Start, span.End = true, end-len(value), end
		}
		span.Insert, span.Empty = end, false
	}
	if err := checkEnd(dec, malformed); err != nil {
		return Span{}, err
	}
	return span, nil
}

// caseVariant is the error for a member whose name equals name only under case
// folding. It names name, not the member, so that it never quotes the input.
func caseVariant(malformed error, name string) error {
	return fmt.Errorf("%w: a member name differs from %q only in case", malformed, name)
}

// checkEnd reads the "}" that closes the object and checks that nothing follows it.
func checkEnd(dec *json.Decoder, malformed error) error {
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%w: invalid syntax", malformed)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the JSON object", malformed)
	}
	return nil
}
