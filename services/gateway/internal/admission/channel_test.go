package admission

import (
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
		if err := checkChannel(channel, getenv); (err == nil) != tc.ok {
			t.Errorf("%q, %q: err = %v, want ok %v", tc.baseURL, tc.credentialEnv, err, tc.ok)
		}
	}
}

func TestCheckChannelNeedsTheAnthropicProtocol(t *testing.T) {
	channel := accessclient.Channel{ID: "c", Protocol: "openai-responses", BaseURL: "http://mock-upstream:8090"}
	if checkChannel(channel, func(string) string { return "" }) == nil {
		t.Error("a channel of another protocol passed")
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
