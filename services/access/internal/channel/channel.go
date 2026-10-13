// Package channel holds the rules a channel record follows, so callers can refuse a
// bad value with a clear message before the database refuses it. The patterns are
// the same as the CHECK constraints in migrations/0001_init.sql; keep them in step.
package channel

import (
	"errors"
	"regexp"
)

// Protocol is the only upstream protocol a channel may speak.
const Protocol = "anthropic-messages"

var (
	baseURLPattern      = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	modelPatternPattern = regexp.MustCompile(`^[^*]+\*?$`)
)

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
