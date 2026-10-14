package seed_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

var tables = []string{"api_key_channels", "api_keys", "channel_models", "channels", "price_versions", "tenants"}

func testConfig() config.Seed {
	return config.Seed{
		KeyPepper:     []byte("test-pepper"),
		SeedSecret:    []byte("test-seed-secret"),
		MockBaseURL:   "http://mock-upstream:8090",
		OllamaBaseURL: "http://host.docker.internal:11434",
		OllamaModel:   "qwen2.5-coder:7b",
	}
}

func migrated(t *testing.T) *pgxpool.Pool {
	return pgtest.Migrated(t, "access", "access", migrations.FS)
}

func counts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, table := range tables {
		var n int
		sql := "SELECT count(*) FROM " + pgx.Identifier{"access", table}.Sanitize()
		if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		got[table] = n
	}
	return got
}

// channelRows lists every channel as "id protocol base_url [credential_env] models".
const channelRows = `SELECT format('%s %s %s [%s] %s', c.id, c.protocol, c.base_url, c.credential_env,
		(SELECT string_agg(model_pattern, ',' ORDER BY model_pattern COLLATE "C")
			FROM access.channel_models WHERE channel_id = c.id))
	FROM access.channels AS c ORDER BY c.id COLLATE "C"`

// texts runs a query returning one text column.
func texts(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDeriveKeyIsStable(t *testing.T) {
	secret := []byte("seed-secret")
	key := seed.DeriveKey(secret, "tenant-01")
	if key != seed.DeriveKey([]byte("seed-secret"), "tenant-01") {
		t.Error("the same secret and tenant gave two keys")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("tenant-01"))
	if key != "gf_"+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) {
		t.Error("key is not gf_ + base64url(HMAC-SHA256(secret, tenant))")
	}
	if !regexp.MustCompile(`^gf_[A-Za-z0-9_-]{43}$`).MatchString(key) {
		t.Error("key is not gf_ and 43 base64url characters")
	}
	if key == seed.DeriveKey(secret, "tenant-02") || key == seed.DeriveKey([]byte("other-secret"), "tenant-01") {
		t.Error("another tenant or secret gave the same key")
	}
}

func TestRunTwiceChangesNothing(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	first, err := seed.Run(ctx, pool, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	afterFirst := counts(t, pool)
	want := map[string]int{"tenants": 11, "api_keys": 11, "api_key_channels": 44, "channels": 4,
		"channel_models": 4, "price_versions": 1}
	if !maps.Equal(afterFirst, want) {
		t.Errorf("rows after the first run = %v, want %v", afterFirst, want)
	}
	channelsAfterFirst := texts(t, pool, channelRows)
	second, err := seed.Run(ctx, pool, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if afterSecond := counts(t, pool); !maps.Equal(afterSecond, afterFirst) {
		t.Errorf("rows after the second run = %v, want %v", afterSecond, afterFirst)
	}
	if got := texts(t, pool, channelRows); !slices.Equal(got, channelsAfterFirst) {
		t.Errorf("channels after the second run = %v, want %v", got, channelsAfterFirst)
	}
	if !slices.Equal(first, second) {
		t.Error("the second run returned other keys")
	}
	if len(first) != 11 || first[0].TenantID != "tenant-00" || first[10].TenantID != "tenant-10" {
		t.Errorf("seeded %d tenants, want tenant-00 to tenant-10", len(first))
	}
}

func TestRunStoresKeysAsSpecified(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	keys, err := seed.Run(ctx, pool, cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		var tenantName, priceVersion, channels string
		var stored []byte
		if err := pool.QueryRow(ctx, `SELECT t.name, a.key_hmac, a.price_version_id,
				(SELECT string_agg(channel_id, ' ' ORDER BY channel_id) FROM access.api_key_channels WHERE key_id = a.id)
			FROM access.api_keys AS a JOIN access.tenants AS t ON t.id = a.tenant_id
			WHERE a.id = $1 AND a.revoked_at IS NULL`, "key-"+k.TenantID).
			Scan(&tenantName, &stored, &priceVersion, &channels); err != nil {
			t.Fatalf("key of %s: %v", k.TenantID, err)
		}
		if tenantName != k.TenantID || priceVersion != seed.PriceVersionID ||
			channels != "mock mock-responses ollama ollama-responses" {
			t.Errorf("%s: name %q, price version %q, channels %q", k.TenantID, tenantName, priceVersion, channels)
		}
		if !hmac.Equal(stored, apikey.HMAC(cfg.KeyPepper, k.Key)) {
			t.Errorf("%s: key_hmac is not HMAC-SHA256(pepper, key)", k.TenantID)
		}
	}
	var input, output int64
	if err := pool.QueryRow(ctx, `SELECT input_micro_per_token, output_micro_per_token
		FROM access.price_versions WHERE id = $1`, seed.PriceVersionID).Scan(&input, &output); err != nil {
		t.Fatal(err)
	}
	if input != 3 || output != 15 {
		t.Errorf("%s prices = %d and %d, want 3 and 15", seed.PriceVersionID, input, output)
	}
}

func TestRunReplacesChannelSettings(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	if _, err := seed.Run(ctx, pool, cfg, 2); err != nil {
		t.Fatal(err)
	}
	cfg.OllamaModel = "llama3.2:3b"
	cfg.OllamaBaseURL = "http://ollama:11434"
	if _, err := seed.Run(ctx, pool, cfg, 2); err != nil {
		t.Fatal(err)
	}
	models := "SELECT model_pattern FROM access.channel_models WHERE channel_id = $1 ORDER BY model_pattern"
	for _, id := range []string{"ollama", "ollama-responses"} {
		if got := texts(t, pool, models, id); !slices.Equal(got, []string{"llama3.2:3b"}) {
			t.Errorf("%s models = %v, want only the new OLLAMA_MODEL", id, got)
		}
	}
	for _, id := range []string{"mock", "mock-responses"} {
		if got := texts(t, pool, models, id); !slices.Equal(got, []string{"mock-*"}) {
			t.Errorf("%s models = %v", id, got)
		}
	}
	want := []string{
		"mock anthropic-messages http://mock-upstream:8090 [] mock-*",
		"mock-responses openai-responses http://mock-upstream:8090 [] mock-*",
		"ollama anthropic-messages http://ollama:11434 [] llama3.2:3b",
		"ollama-responses openai-responses http://ollama:11434 [] llama3.2:3b",
	}
	if got := texts(t, pool, channelRows); !slices.Equal(got, want) {
		t.Errorf("channels = %v, want %v", got, want)
	}
}

// A database seeded before Responses channels existed keeps its two channels exactly
// as they were, and its keys gain the two Responses channels.
func TestRunKeepsChannelsSeededBeforeResponses(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	// What the previous seed wrote for tenant-00.
	for _, sql := range []string{
		"INSERT INTO access.price_versions (id, input_micro_per_token, output_micro_per_token) VALUES ('pv-seed-1', 3, 15)",
		`INSERT INTO access.channels (id, protocol, base_url, credential_env) VALUES
			('mock', 'anthropic-messages', 'http://mock-upstream:8090', ''),
			('ollama', 'anthropic-messages', 'http://host.docker.internal:11434', '')`,
		"INSERT INTO access.channel_models (channel_id, model_pattern) VALUES ('mock', 'mock-*'), ('ollama', 'qwen2.5-coder:7b')",
		"INSERT INTO access.tenants (id, name) VALUES ('tenant-00', 'tenant-00')",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access.api_keys (id, tenant_id, key_hmac, price_version_id)
		VALUES ('key-tenant-00', 'tenant-00', $1, 'pv-seed-1')`,
		apikey.HMAC(cfg.KeyPepper, seed.DeriveKey(cfg.SeedSecret, "tenant-00"))); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access.api_key_channels (key_id, channel_id) VALUES
		('key-tenant-00', 'mock'), ('key-tenant-00', 'ollama')`); err != nil {
		t.Fatal(err)
	}
	// Every column, created_at too. The upsert rewrites a channel row with the same
	// values, so the row version is not compared.
	const previousRows = `SELECT to_jsonb(c)::text FROM access.channels AS c WHERE c.id IN ('mock', 'ollama')
		UNION ALL SELECT to_jsonb(m)::text FROM access.channel_models AS m WHERE m.channel_id IN ('mock', 'ollama')
		ORDER BY 1`
	before := texts(t, pool, previousRows)
	for range 2 {
		if _, err := seed.Run(ctx, pool, cfg, 1); err != nil {
			t.Fatal(err)
		}
	}
	if after := texts(t, pool, previousRows); !slices.Equal(after, before) {
		t.Errorf("mock and ollama rows after seeding = %v, want them unchanged %v", after, before)
	}
	if got := texts(t, pool, "SELECT id FROM access.channels ORDER BY id COLLATE \"C\""); !slices.Equal(got,
		[]string{"mock", "mock-responses", "ollama", "ollama-responses"}) {
		t.Errorf("channels = %v, want the two seeded before and their Responses twins", got)
	}
	keyChannels := "SELECT channel_id FROM access.api_key_channels WHERE key_id = 'key-tenant-00' ORDER BY channel_id COLLATE \"C\""
	if got := texts(t, pool, keyChannels); !slices.Equal(got, []string{"mock", "mock-responses", "ollama", "ollama-responses"}) {
		t.Errorf("channels of the key seeded before = %v, want all four", got)
	}
}

func TestRunRefusesBadChannelSettings(t *testing.T) {
	pool := migrated(t)
	cases := []struct {
		name, value string
		set         func(*config.Seed, string)
	}{
		{config.EnvMockBaseURL, "http://mock-upstream:8090/", func(c *config.Seed, v string) { c.MockBaseURL = v }},
		{config.EnvMockBaseURL, "ftp://mock-upstream", func(c *config.Seed, v string) { c.MockBaseURL = v }},
		{config.EnvMockBaseURL, "", func(c *config.Seed, v string) { c.MockBaseURL = v }},
		{config.EnvOllamaBaseURL, "http://user:secret-pass@ollama:11434", func(c *config.Seed, v string) { c.OllamaBaseURL = v }},
		{config.EnvOllamaBaseURL, "http://ollama:11434/v1", func(c *config.Seed, v string) { c.OllamaBaseURL = v }},
		{config.EnvOllamaBaseURL, "http://ollama?x=1", func(c *config.Seed, v string) { c.OllamaBaseURL = v }},
		{config.EnvOllamaModel, "", func(c *config.Seed, v string) { c.OllamaModel = v }},
		{config.EnvOllamaModel, "qwen*:7b", func(c *config.Seed, v string) { c.OllamaModel = v }},
	}
	for _, c := range cases {
		cfg := testConfig()
		c.set(&cfg, c.value)
		_, err := seed.Run(context.Background(), pool, cfg, 2)
		if err == nil {
			t.Errorf("%s=%q accepted", c.name, c.value)
			continue
		}
		if !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s=%q: error %q does not name the variable", c.name, c.value, err)
		}
		if c.value != "" && strings.Contains(err.Error(), c.value) {
			t.Errorf("%s=%q: error shows the value", c.name, c.value)
		}
	}
	for table, n := range counts(t, pool) {
		if n != 0 {
			t.Errorf("%s has %d rows after refused runs", table, n)
		}
	}
}

func TestRunIsOneTransaction(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	// Another key already holds tenant-05's HMAC, so inserting the seeded keys fails.
	taken := apikey.HMAC(cfg.KeyPepper, seed.DeriveKey(cfg.SeedSecret, "tenant-05"))
	for _, sql := range []string{
		"INSERT INTO access.tenants (id, name) VALUES ('other', 'other')",
		"INSERT INTO access.price_versions (id, input_micro_per_token, output_micro_per_token) VALUES ('pv-other', 1, 1)",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access.api_keys (id, tenant_id, key_hmac, price_version_id)
		VALUES ('key-other', 'other', $1, 'pv-other')`, taken); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Run(ctx, pool, cfg, 10); err == nil {
		t.Fatal("seed succeeded despite a duplicate key HMAC")
	}
	want := map[string]int{"tenants": 1, "api_keys": 1, "api_key_channels": 0, "channels": 0,
		"channel_models": 0, "price_versions": 1}
	if got := counts(t, pool); !maps.Equal(got, want) {
		t.Errorf("rows after the failed run = %v, want only the rows from before it %v", got, want)
	}
}

func TestRunRefusesKeysFromAnotherPepper(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	if _, err := seed.Run(ctx, pool, cfg, 2); err != nil {
		t.Fatal(err)
	}
	cfg.KeyPepper = []byte("another-pepper")
	_, err := seed.Run(ctx, pool, cfg, 2)
	if err == nil || !strings.Contains(err.Error(), config.EnvKeyPepper) {
		t.Fatalf("err = %v, want one naming %s", err, config.EnvKeyPepper)
	}
	if strings.Contains(err.Error(), "another-pepper") || strings.Contains(err.Error(), "test-pepper") {
		t.Error("error shows a pepper")
	}
}

// No row in any table of the schema holds a key, as text or as bytes, nor the HMAC
// the key is made of.
func TestNoTableHoldsAKey(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	cfg := testConfig()
	keys, err := seed.Run(ctx, pool, cfg, 10)
	if err != nil {
		t.Fatal(err)
	}
	var secrets, stored []string
	for _, k := range keys {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(k.Key, "gf_"))
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, k.Key, hex.EncodeToString([]byte(k.Key)), hex.EncodeToString(raw))
		stored = append(stored, hex.EncodeToString(apikey.HMAC(cfg.KeyPepper, k.Key)))
	}
	matches := func(table string, needles []string) int {
		var n int
		sql := "SELECT count(*) FROM " + pgx.Identifier{"access", table}.Sanitize() +
			" AS r, unnest($1::text[]) AS needle WHERE strpos(r::text, needle) > 0"
		if err := pool.QueryRow(ctx, sql, needles).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The search sees bytea columns: each stored HMAC is found once.
	if n := matches("api_keys", stored); n != len(keys) {
		t.Fatalf("found %d stored HMACs, want %d", n, len(keys))
	}
	for _, table := range texts(t, pool, "SELECT tablename FROM pg_tables WHERE schemaname = 'access'") {
		if n := matches(table, secrets); n != 0 {
			t.Errorf("%s: %d rows hold a key", table, n)
		}
	}
}
