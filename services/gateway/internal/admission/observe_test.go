package admission_test

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
	"github.com/prometheus/client_golang/prometheus"
)

// Through the real admission, a call names its verified tenant's slot from the
// moment access verifies its key, even when a later step refuses it. Only a call
// with its cost held is eligible and admitted; every refusal leaves both demand
// signals as they were, and no call stays active.
func TestAdmissionObservation(t *testing.T) {
	misconfigured := channelJSON{ID: "mock", Protocol: "openai-responses", BaseURL: "http://127.0.0.1:9"}
	tests := []struct {
		name      string
		setup     setup
		responses bool
		status    int
		result    string
		slot      string
		eligible  float64
	}{
		{"admitted", setup{}, false, http.StatusOK, "completed", "tenant-01", 1},
		{"admitted on Responses", setup{responses: true}, true, http.StatusOK, "completed", "tenant-01", 1},
		{"invalid key", setup{verify: internalError(http.StatusUnauthorized, "invalid_key")}, false,
			http.StatusUnauthorized, "authentication_denied", "unverified", 0},
		{"model not allowed", setup{verify: internalError(http.StatusForbidden, "model_not_allowed")}, false,
			http.StatusForbidden, "permission_denied", "unverified", 0},
		{"insufficient balance", setup{reserve: internalError(http.StatusConflict, "insufficient_balance")}, false,
			http.StatusPaymentRequired, "insufficient_balance", "tenant-01", 0},
		{"metering down", setup{meteringDown: true}, false,
			http.StatusServiceUnavailable, "dependency_error", "tenant-01", 0},
		{"channel misconfigured", setup{verify: verifyOK(misconfigured)}, false,
			http.StatusInternalServerError, "internal_error", "tenant-01", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := &syncBuffer{}
			registry := prometheus.NewRegistry()
			observer, err := relay.NewObserver(registry, slog.New(slog.NewJSONHandler(lines, nil)), []string{"mock-small"})
			if err != nil {
				t.Fatal(err)
			}
			tt.setup.observer = observer
			h := start(t, tt.setup)

			var resp *http.Response
			if tt.responses {
				resp = h.postResponses(context.Background(), responsesBody, nil)
			} else {
				resp = h.post(context.Background(), nil)
			}
			readBody(t, resp)
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			h.finish()

			line := requestLine(t, lines.String())
			if line["result"] != tt.result || line["tenant_slot"] != tt.slot || line["model"] != "mock-small" {
				t.Errorf("request line = %v, want result %s and tenant slot %s", line, tt.result, tt.slot)
			}
			if got := gathered(t, registry, "gateflow_gateway_eligible_arrivals_total"); got != tt.eligible {
				t.Errorf("eligible arrivals = %v, want %v", got, tt.eligible)
			}
			if got := gathered(t, registry, "gateflow_gateway_active_admitted_requests"); got != 0 {
				t.Errorf("active admitted = %v after the call ended, want 0", got)
			}
			for _, canary := range []string{gatewayKey, accessToken, meteringToken, bodyCanary} {
				if strings.Contains(lines.String(), canary) {
					t.Errorf("the request line holds %q", canary)
				}
			}
		})
	}
}

// requestLine returns the one request log line in logs.
func requestLine(t *testing.T, logs string) map[string]any {
	t.Helper()
	var found []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("log line %q: %v", scanner.Text(), err)
		}
		if line["msg"] == "request" {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d request lines, want 1:\n%s", len(found), logs)
	}
	return found[0]
}

// gathered returns the value of a metric family that has one unlabelled series.
func gathered(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) == 1 {
			metric := family.GetMetric()[0]
			return metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}
