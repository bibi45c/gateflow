package channel_test

import (
	"testing"

	"github.com/bibi45c/gateflow/services/access/internal/channel"
)

func TestCheckProtocol(t *testing.T) {
	tests := map[string]bool{
		"anthropic-messages":  true,
		"openai-responses":    true,
		"":                    false,
		"Anthropic-Messages":  false,
		"OPENAI-RESPONSES":    false,
		" openai-responses":   false,
		"openai-responses\n":  false,
		"openai-chat":         false,
		"anthropic-messages,": false,
	}
	for protocol, valid := range tests {
		if got := channel.CheckProtocol(protocol) == nil; got != valid {
			t.Errorf("CheckProtocol(%q) accepts it = %v, want %v", protocol, got, valid)
		}
	}
	if channel.Protocol != "anthropic-messages" || channel.ProtocolResponses != "openai-responses" {
		t.Errorf("protocols are %q and %q", channel.Protocol, channel.ProtocolResponses)
	}
}
