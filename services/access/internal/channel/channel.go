// Package channel holds the rules a channel record follows, so callers can refuse a
// bad value with a clear message before the database refuses it. The patterns are
// the same as the CHECK constraints in migrations/0001_init.sql, and the protocols
// the same as in migrations/0002_channel_protocols.sql; keep them in step.
package channel

import (
	"errors"
	"regexp"
)

// The upstream protocols a channel may speak, as verify requests name them.
const (
	// Protocol is Anthropic Messages, POST /v1/messages. A verify request without a
	// protocol asks for it.
	Protocol = "anthropic-messages"
	// ProtocolResponses is OpenAI Responses, POST /v1/responses.
	ProtocolResponses = "openai-responses"
)

var (
	baseURLPattern      = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	modelPatternPattern = regexp.MustCompile(`^[^*]+\*?$`)
)

// CheckProtocol fails unless protocol is exactly one a channel may speak.
func CheckProtocol(protocol string) error {
	switch protocol {
	case Protocol, ProtocolResponses:
		return nil
	}
	return errors.New("must be anthropic-messages or openai-responses")
}

// CheckBaseURL fails unless baseURL is http or https, a host and an optional port,
// with no path, query or trailing slash.
func CheckBaseURL(baseURL string) error {
	if !baseURLPattern.MatchString(baseURL) {
		return errors.New("must be http:// or https://, a host and an optional port, with no path, query or trailing slash")
	}
	return nil
}

// CheckModelPattern fails unless pattern is an exact model name, or a prefix ending
// in *.
func CheckModelPattern(pattern string) error {
	if !modelPatternPattern.MatchString(pattern) {
		return errors.New("must be a model name, or a prefix ending in *")
	}
	return nil
}
