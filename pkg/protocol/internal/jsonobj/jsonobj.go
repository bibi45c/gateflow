// Package jsonobj reads chosen members of a JSON object strictly, for the wire codecs
// under pkg/protocol.
package jsonobj

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeMembers decodes each member of the JSON object in data that targets names
// into its target, matching names exactly. A null member leaves a target unchanged,
// except that a pointer target becomes nil. Other members are checked for syntax only.
// A named member that appears twice is an error, so that no other reader of the same
// bytes can see a different value. Every error wraps malformed and may name a member,
// but never quotes the input.
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
