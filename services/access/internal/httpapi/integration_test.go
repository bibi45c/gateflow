package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
	"github.com/bibi45c/gateflow/services/access/internal/store"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

const ollamaModel = "qwen2.5-coder:7b"

// seeded is the service's handler on a database holding the seed of tenant-00 to
// tenant-10.
type seeded struct {
	handler http.Handler
	logs    *bytes.Buffer
	pool    *pgxpool.Pool
	keys    map[string]string
}

func newSeeded(t *testing.T) seeded {
	t.Helper()
	pool := pgtest.Migrated(t, "access", store.Schema, migrations.FS)
	keys, err := seed.Run(context.Background(), pool, config.Seed{
		KeyPepper:     []byte(testPepper),
		SeedSecret:    []byte("test-seed-secret"),
		MockBaseURL:   "http://mock-upstream:8090",
		OllamaBaseURL: "http://host.docker.internal:11434",
		OllamaModel:   ollamaModel,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	s := seeded{pool: pool, keys: map[string]string{}}
	for _, k := range keys {
		s.keys[k.TenantID] = k.Key
	}
	s.handler, s.logs = newTestServer(store.New(pool), testToken)
	return s
}

func (s seeded) verify(key, model string) *httptest.ResponseRecorder {
	return call(s.handler, "POST", "/internal/v1/keys/verify", verifyBody(key, model), authorized())
}

func TestSeededKeyWithAnAllowedModelIsVerified(t *testing.T) {
	s := newSeeded(t)
	tests := []struct {
		tenant, model string
		channel       channelResponse
	}{
		{"tenant-01", "mock-fast", channelResponse{ID: "mock", Protocol: "anthropic-messages", BaseURL: "http://mock-upstream:8090"}},
		{"tenant-10", "mock-", channelResponse{ID: "mock", Protocol: "anthropic-messages", BaseURL: "http://mock-upstream:8090"}},
		{"tenant-00", ollamaModel, channelResponse{ID: "ollama", Protocol: "anthropic-messages", BaseURL: "http://host.docker.internal:11434"}},
	}
	for _, tt := range tests {
		got := s.verify(s.keys[tt.tenant], tt.model)
		if got.Code != http.StatusOK {
			t.Fatalf("%s, %s: status %d, want 200: %s", tt.tenant, tt.model, got.Code, got.Body)
		}
		var body verifyResponse
		if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := verifyResponse{TenantID: tt.tenant, KeyID: "key-" + tt.tenant, PriceVersionID: seed.PriceVersionID, Channel: tt.channel}
		if body != want {
			t.Errorf("%s, %s: body %+v, want %+v", tt.tenant, tt.model, body, want)
		}
	}
}

func TestUnknownKeyIsInvalid(t *testing.T) {
	s := newSeeded(t)
	for _, key := range []string{"gf_not-a-seeded-key", s.keys["tenant-01"] + "x", strings.ToUpper(s.keys["tenant-01"])} {
		wantError(t, s.verify(key, "mock-fast"), http.StatusUnauthorized, CodeInvalidKey)
	}
}

func TestRevokedKeyIsInvalid(t *testing.T) {
	s := newSeeded(t)
	const revokedKey = "gf_revoked-test-key"
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO access.api_keys (id, tenant_id, key_hmac, price_version_id, revoked_at)
		VALUES ('key-revoked', 'tenant-01', $1, $2, now())`,
		apikey.HMAC([]byte(testPepper), revokedKey), seed.PriceVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO access.api_key_channels (key_id, channel_id) VALUES ('key-revoked', 'mock')`); err != nil {
		t.Fatal(err)
	}
	wantError(t, s.verify(revokedKey, "mock-fast"), http.StatusUnauthorized, CodeInvalidKey)
	// The tenant's other key still verifies.
	if got := s.verify(s.keys["tenant-01"], "mock-fast"); got.Code != http.StatusOK {
		t.Errorf("the tenant's live key: status %d, want 200", got.Code)
	}
}

func TestModelTheKeyDoesNotAllowIsRefused(t *testing.T) {
	s := newSeeded(t)
	for _, model := range []string{"claude-not-seeded", "mock", ollamaModel + "-large", "%", "mock-*x"} {
		if model == "mock-*x" {
			// Starts with the mock channel's prefix, so it is allowed.
			if got := s.verify(s.keys["tenant-01"], model); got.Code != http.StatusOK {
				t.Errorf("model %q: status %d, want 200", model, got.Code)
			}
			continue
		}
		wantError(t, s.verify(s.keys["tenant-01"], model), http.StatusForbidden, CodeModelNotAllowed)
	}

	// A key with no channel at all may use no model.
	ctx := context.Background()
	const bareKey = "gf_key-without-channels"
	if _, err := s.pool.Exec(ctx, `INSERT INTO access.api_keys (id, tenant_id, key_hmac, price_version_id)
		VALUES ('key-bare', 'tenant-02', $1, $2)`, apikey.HMAC([]byte(testPepper), bareKey), seed.PriceVersionID); err != nil {
		t.Fatal(err)
	}
	wantError(t, s.verify(bareKey, "mock-fast"), http.StatusForbidden, CodeModelNotAllowed)
}

func TestLowestChannelIDWinsWhenSeveralMatch(t *testing.T) {
	s := newSeeded(t)
	ctx := context.Background()
	for _, sql := range []string{
		`INSERT INTO access.channels (id, protocol, base_url) VALUES
			('a-first', 'anthropic-messages', 'http://first:1'), ('z-last', 'anthropic-messages', 'http://last:1')`,
		`INSERT INTO access.channel_models (channel_id, model_pattern) VALUES
			('a-first', 'mock-fast'), ('a-first', 'other'), ('z-last', 'mock-*')`,
		`INSERT INTO access.api_key_channels (key_id, channel_id) VALUES
			('key-tenant-03', 'z-last'), ('key-tenant-03', 'a-first')`,
	} {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for model, channelID := range map[string]string{"mock-fast": "a-first", "mock-slow": "mock"} {
		got := s.verify(s.keys["tenant-03"], model)
		var body verifyResponse
		if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || got.Code != http.StatusOK {
			t.Fatalf("model %s: status %d, %v", model, got.Code, err)
		}
		if body.Channel.ID != channelID {
			t.Errorf("model %s: channel %q, want %q", model, body.Channel.ID, channelID)
		}
	}
	// Another key does not get the channels it was not given.
	got := s.verify(s.keys["tenant-04"], "mock-fast")
	if !strings.Contains(got.Body.String(), `"id":"mock"`) {
		t.Errorf("tenant-04 got %s, want the mock channel", got.Body)
	}
}

func TestCallWithoutTheTokenIsUnauthorized(t *testing.T) {
	s := newSeeded(t)
	body := verifyBody(s.keys["tenant-01"], "mock-fast")
	wantError(t, call(s.handler, "POST", "/internal/v1/keys/verify", body, nil), http.StatusUnauthorized, CodeUnauthorized)
	wantError(t, call(s.handler, "GET", "/internal/v1/price-versions/"+seed.PriceVersionID, "", nil), http.StatusUnauthorized, CodeUnauthorized)
	wrong := map[string]string{"Authorization": "Bearer wrong"}
	wantError(t, call(s.handler, "POST", "/internal/v1/keys/verify", body, wrong), http.StatusUnauthorized, CodeUnauthorized)
}

func TestClosedPoolIsDependencyUnavailable(t *testing.T) {
	s := newSeeded(t)
	key := s.keys["tenant-01"]
	s.pool.Close()
	wantError(t, s.verify(key, "mock-fast"), http.StatusServiceUnavailable, CodeDependencyUnavailable)
	got := call(s.handler, "GET", "/internal/v1/price-versions/"+seed.PriceVersionID, "", authorized())
	wantError(t, got, http.StatusServiceUnavailable, CodeDependencyUnavailable)
}

func TestPriceVersionReturnsTheSeededRates(t *testing.T) {
	s := newSeeded(t)
	got := call(s.handler, "GET", "/internal/v1/price-versions/"+seed.PriceVersionID, "", authorized())
	if got.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", got.Code, got.Body)
	}
	const want = `{"id":"pv-seed-1","input_micro_per_token":3,"output_micro_per_token":15}`
	if body := strings.TrimSpace(got.Body.String()); body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	for _, path := range []string{"pv-unknown", "PV-SEED-1", "pv-seed-1%20", "pv%00"} {
		got := call(s.handler, "GET", "/internal/v1/price-versions/"+path, "", authorized())
		wantError(t, got, http.StatusNotFound, CodePriceVersionNotFound)
	}
}

func TestVerifyLogsAndDatabaseDoNotHoldTheKey(t *testing.T) {
	s := newSeeded(t)
	key := s.keys["tenant-05"]
	calls := []*httptest.ResponseRecorder{
		s.verify(key, "mock-fast"),
		s.verify(key, "claude-not-seeded"),
		s.verify(key+"x", "mock-fast"),
		call(s.handler, "POST", "/internal/v1/keys/verify", verifyBody(key, "mock-fast"), nil),
		call(s.handler, "POST", "/internal/v1/keys/verify", `{"key":"`+key+`"`, authorized()),
	}
	for i, status := range []int{200, 403, 401, 401, 400} {
		if calls[i].Code != status {
			t.Fatalf("call %d: status %d, want %d", i, calls[i].Code, status)
		}
	}
	logs := s.logs.String()
	if lines := strings.Count(logs, "\n"); lines != len(calls) {
		t.Errorf("%d log lines for %d calls: %s", lines, len(calls), logs)
	}
	if !strings.Contains(logs, `"key_id":"key-tenant-05","tenant_id":"tenant-05"`) {
		t.Errorf("the log of a verified call lacks key_id and tenant_id: %s", logs)
	}
	for _, secret := range secretsOf(key, []byte(testPepper), testToken) {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs hold %q: %s", secret, logs)
		}
	}

	// The database holds the HMAC and the key nowhere.
	var stored []byte
	if err := s.pool.QueryRow(context.Background(),
		"SELECT key_hmac FROM access.api_keys WHERE id = 'key-tenant-05'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, apikey.HMAC([]byte(testPepper), key)) {
		t.Error("key_hmac is not HMAC(pepper, key)")
	}
	var holding int
	if err := s.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM access.api_keys AS k WHERE to_jsonb(k)::text LIKE '%' || $1 || '%')
		+ (SELECT count(*) FROM access.tenants AS t WHERE to_jsonb(t)::text LIKE '%' || $1 || '%')`,
		key).Scan(&holding); err != nil {
		t.Fatal(err)
	}
	if holding != 0 {
		t.Errorf("%d rows hold the raw key", holding)
	}
}
