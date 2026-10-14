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
	return s.verifyOver(key, model, "")
}

// verifyOver calls verify naming protocol, or without protocol when it is empty.
func (s seeded) verifyOver(key, model, protocol string) *httptest.ResponseRecorder {
	return call(s.handler, "POST", "/internal/v1/keys/verify", verifyBodyOver(key, model, protocol), authorized())
}

// exec runs each statement on the seeded database.
func (s seeded) exec(t *testing.T, statements ...string) {
	t.Helper()
	for _, sql := range statements {
		if _, err := s.pool.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
}

// channelOf returns the channel of a 200 verify answer.
func channelOf(t *testing.T, got *httptest.ResponseRecorder) channelResponse {
	t.Helper()
	var body verifyResponse
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil || got.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", got.Code, got.Body)
	}
	return body.Channel
}

func TestSeededKeyWithAnAllowedModelIsVerified(t *testing.T) {
	s := newSeeded(t)
	mock := channelResponse{ID: "mock", Protocol: "anthropic-messages", BaseURL: "http://mock-upstream:8090"}
	mockResponses := channelResponse{ID: "mock-responses", Protocol: "openai-responses", BaseURL: "http://mock-upstream:8090"}
	ollama := channelResponse{ID: "ollama", Protocol: "anthropic-messages", BaseURL: "http://host.docker.internal:11434"}
	ollamaResponses := channelResponse{ID: "ollama-responses", Protocol: "openai-responses", BaseURL: "http://host.docker.internal:11434"}
	tests := []struct {
		tenant, model, protocol string
		channel                 channelResponse
	}{
		{"tenant-01", "mock-fast", "", mock},
		{"tenant-10", "mock-", "", mock},
		{"tenant-00", ollamaModel, "", ollama},
		{"tenant-01", "mock-fast", "anthropic-messages", mock},
		{"tenant-00", ollamaModel, "anthropic-messages", ollama},
		{"tenant-01", "mock-fast", "openai-responses", mockResponses},
		{"tenant-10", "mock-", "openai-responses", mockResponses},
		{"tenant-00", ollamaModel, "openai-responses", ollamaResponses},
	}
	for _, tt := range tests {
		got := s.verifyOver(s.keys[tt.tenant], tt.model, tt.protocol)
		if got.Code != http.StatusOK {
			t.Fatalf("%s, %s over %q: status %d, want 200: %s", tt.tenant, tt.model, tt.protocol, got.Code, got.Body)
		}
		var body verifyResponse
		if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := verifyResponse{TenantID: tt.tenant, KeyID: "key-" + tt.tenant, PriceVersionID: seed.PriceVersionID, Channel: tt.channel}
		if body != want {
			t.Errorf("%s, %s over %q: body %+v, want %+v", tt.tenant, tt.model, tt.protocol, body, want)
		}
	}
}

// A gateway that sends no protocol gets the very answer it got before Responses
// channels existed, the same as one naming anthropic-messages.
func TestVerifyWithoutProtocolAnswersAsMessages(t *testing.T) {
	s := newSeeded(t)
	for _, tenant := range []string{"tenant-00", "tenant-01", "tenant-10"} {
		for _, model := range []string{"mock-fast", ollamaModel, "claude-not-seeded"} {
			without := s.verify(s.keys[tenant], model)
			named := s.verifyOver(s.keys[tenant], model, "anthropic-messages")
			if without.Code != named.Code {
				t.Errorf("%s, %s: status %d without protocol, %d with it", tenant, model, without.Code, named.Code)
			}
			if without.Code == http.StatusOK && without.Body.String() != named.Body.String() {
				t.Errorf("%s, %s: body %s without protocol, %s with it", tenant, model, without.Body, named.Body)
			}
			if without.Code == http.StatusOK && channelOf(t, without).Protocol != "anthropic-messages" {
				t.Errorf("%s, %s: no protocol gave %s", tenant, model, without.Body)
			}
		}
	}
	wantError(t, s.verify(s.keys["tenant-01"], "claude-not-seeded"), http.StatusForbidden, CodeModelNotAllowed)
}

func TestVerifyRefusesAnUnknownProtocol(t *testing.T) {
	s := newSeeded(t)
	key := s.keys["tenant-01"]
	for _, body := range []string{
		verifyBodyOver(key, "mock-fast", "openai-chat"),
		verifyBodyOver(key, "mock-fast", "Openai-Responses"),
		`{"key":"` + key + `","model":"mock-fast","protocol":""}`,
		`{"key":"` + key + `","model":"mock-fast","protocol":null}`,
	} {
		wantError(t, call(s.handler, "POST", "/internal/v1/keys/verify", body, authorized()), http.StatusBadRequest, CodeInvalidRequest)
	}
}

// Verify only considers the key's channels that speak the requested protocol, even
// when one of another protocol has a lower id or is the only one serving the model.
func TestVerifyConsidersOnlyChannelsOfTheProtocol(t *testing.T) {
	s := newSeeded(t)
	s.exec(t,
		`INSERT INTO access.channels (id, protocol, base_url) VALUES
			('a-before', 'openai-responses', 'http://before:1'), ('a-first', 'anthropic-messages', 'http://first:1'),
			('claude-only', 'anthropic-messages', 'http://claude:1'), ('gpt-only', 'openai-responses', 'http://gpt:1')`,
		`INSERT INTO access.channel_models (channel_id, model_pattern) VALUES
			('a-before', 'mock-*'), ('a-first', 'mock-fast'), ('claude-only', 'claude-sonnet-5'), ('gpt-only', 'gpt-5')`,
		`INSERT INTO access.api_key_channels (key_id, channel_id) VALUES ('key-tenant-03', 'a-before'),
			('key-tenant-03', 'a-first'), ('key-tenant-03', 'claude-only'), ('key-tenant-03', 'gpt-only')`,
	)
	key := s.keys["tenant-03"]
	tests := []struct{ model, protocol, channelID string }{
		{"mock-fast", "", "a-first"},
		{"mock-fast", "anthropic-messages", "a-first"},
		{"mock-slow", "anthropic-messages", "mock"},
		{"mock-fast", "openai-responses", "a-before"},
		{"mock-slow", "openai-responses", "a-before"},
		{ollamaModel, "openai-responses", "ollama-responses"},
		{"claude-sonnet-5", "anthropic-messages", "claude-only"},
		{"gpt-5", "openai-responses", "gpt-only"},
	}
	for _, tt := range tests {
		got := channelOf(t, s.verifyOver(key, tt.model, tt.protocol))
		want := tt.protocol
		if want == "" {
			want = "anthropic-messages"
		}
		if got.ID != tt.channelID || got.Protocol != want {
			t.Errorf("%s over %q: channel %q over %q, want %q over %q", tt.model, tt.protocol, got.ID, got.Protocol, tt.channelID, want)
		}
	}
	for _, refused := range [][2]string{{"claude-sonnet-5", "openai-responses"}, {"gpt-5", "anthropic-messages"}, {"gpt-5", ""}} {
		wantError(t, s.verifyOver(key, refused[0], refused[1]), http.StatusForbidden, CodeModelNotAllowed)
	}
	// Another key does not get the channels it was not given.
	if got := channelOf(t, s.verifyOver(s.keys["tenant-04"], "mock-fast", "openai-responses")); got.ID != "mock-responses" {
		t.Errorf("tenant-04 got channel %q, want mock-responses", got.ID)
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
