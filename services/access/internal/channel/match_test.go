package channel_test

import (
	"testing"

	"github.com/bibi45c/gateflow/services/access/internal/channel"
)

func TestMatchModel(t *testing.T) {
	tests := []struct {
		pattern, model string
		want           bool
	}{
		{"mock-fast", "mock-fast", true},
		{"mock-fast", "mock-faster", false},
		{"mock-fast", "mock-fas", false},
		{"mock-fast", "Mock-fast", false},
		{"mock-*", "mock-fast", true},
		{"mock-*", "mock-", true},
		{"mock-*", "mock", false},
		{"mock-*", "other-mock-fast", false},
		{"mock-*", "mock-*", true},
		{"mock-fast", "mock-*", false},
		{"qwen2.5-coder:7b", "qwen2.5-coder:7b", true},
		{"qwen2.5-coder:7b", "qwen2x5-coder:7b", false},
	}
	for _, tt := range tests {
		if got := channel.MatchModel(tt.pattern, tt.model); got != tt.want {
			t.Errorf("MatchModel(%q, %q) = %v, want %v", tt.pattern, tt.model, got, tt.want)
		}
	}
}
