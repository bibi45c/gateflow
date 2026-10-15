package admission

import (
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
)

func TestCheckChannel(t *testing.T) {
	getenv := func(name string) string {
		if name == "UPSTREAM_CREDENTIAL_HOSTED" {
			return "sk-value"
		}
		return ""
	}
	for _, tc := range []struct {
		baseURL, credentialEnv string
		ok                     bool
	}{
		{"http://mock-upstream:8090", "", true},
		{"http://host.docker.internal:11434", "", true},
		{"https://api.example.com", "UPSTREAM_CREDENTIAL_HOSTED", true},
		{"https://api.example.com:443", "", true},
		{"http://mock-upstream:8090/", "", false},
		{"http://mock-upstream:8090/v1", "", false},
		{"http://mock-upstream:8090?x=1", "", false},
		{"http://user:pass@mock-upstream:8090", "", false},
		{"http://mock-upstream:123456", "", false},
		{"mock-upstream:8090", "", false},
		{"", "", false},
		{"http://api.example.com", "UPSTREAM_CREDENTIAL_HOSTED", false},
		{"https://api.example.com", "UPSTREAM_CREDENTIAL_", false},
		{"https://api.example.com", "UPSTREAM_CREDENTIAL_hosted", false},
		{"https://api.example.com", "PATH", false},
		{"https://api.example.com", "UPSTREAM_CREDENTIAL_UNSET", false},
	} {
		channel := accessclient.Channel{
			ID:            "c",
			Protocol:      "anthropic-messages",
			BaseURL:       tc.baseURL,
			CredentialEnv: tc.credentialEnv,
		}
		if err := checkChannel(channel, "anthropic-messages", getenv); (err == nil) != tc.ok {
			t.Errorf("%q, %q: err = %v, want ok %v", tc.baseURL, tc.credentialEnv, err, tc.ok)
		}
	}
}

func TestCheckChannelNeedsTheAnthropicProtocol(t *testing.T) {
	channel := accessclient.Channel{ID: "c", Protocol: "openai-responses", BaseURL: "http://mock-upstream:8090"}
	if checkChannel(channel, "anthropic-messages", func(string) string { return "" }) == nil {
		t.Error("a channel of another protocol passed")
	}
}

// A channel must speak the protocol of the endpoint called, whichever that is.
func TestCheckChannelNeedsTheEndpointProtocol(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, tc := range []struct {
		channel, endpoint string
		ok                bool
	}{
		{"anthropic-messages", relay.ProtocolMessages.String(), true},
		{"openai-responses", relay.ProtocolResponses.String(), true},
		{"anthropic-messages", relay.ProtocolResponses.String(), false},
		{"openai-responses", relay.ProtocolMessages.String(), false},
		{"", relay.ProtocolResponses.String(), false},
		{"OPENAI-RESPONSES", relay.ProtocolResponses.String(), false},
	} {
		channel := accessclient.Channel{ID: "c", Protocol: tc.channel, BaseURL: "http://mock-upstream:8090"}
		err := checkChannel(channel, tc.endpoint, noEnv)
		if (err == nil) != tc.ok {
			t.Errorf("%q channel on the %s endpoint: err = %v, want ok %v", tc.channel, tc.endpoint, err, tc.ok)
		}
		if err != nil && strings.Contains(err.Error(), tc.channel) && tc.channel != "" {
			t.Errorf("the reason %q holds the channel's protocol", err)
		}
	}
}

func TestSettleRequestSendsZeroCountsForNoCharge(t *testing.T) {
	seen := int64(7)
	for _, outcome := range []string{relay.OutcomeUpstreamError, relay.OutcomeNotDispatched} {
		req := settleRequest(relay.Result{Outcome: outcome, InputTokens: &seen})
		if req.InputTokens == nil || *req.InputTokens != 0 || req.OutputTokens == nil || *req.OutputTokens != 0 {
			t.Errorf("%s: counts %v, %v, want 0 and 0", outcome, req.InputTokens, req.OutputTokens)
		}
	}
	req := settleRequest(relay.Result{Outcome: relay.OutcomeInterrupted, InputTokens: &seen})
	if req.InputTokens == nil || *req.InputTokens != 7 || req.OutputTokens != nil || req.Outcome != "interrupted" {
		t.Errorf("interrupted: got %+v, want input 7 and output null", req)
	}
}
