package channel

import "strings"

// MatchModel reports whether model is served under pattern: an exact name matches
// only itself, and a pattern ending in * matches every name starting with the part
// before it.
func MatchModel(pattern, model string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(model, prefix)
	}
	return pattern == model
}
