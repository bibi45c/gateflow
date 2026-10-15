package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/channel"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
	"github.com/bibi45c/gateflow/services/access/internal/store"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

var tables = []string{"api_key_channels", "api_keys", "channel_models", "channels", "price_versions", "tenants"}

// previousChannelSQL is, verbatim, the channel query of the access image before
// migration 0002, which knew one protocol and so filtered on none.
const previousChannelSQL = `SELECT c.id, c.protocol, c.base_url, c.credential_env, m.model_pattern
		FROM access.api_key_channels AS kc
		JOIN access.channels AS c ON c.id = kc.channel_id
		JOIN access.channel_models AS m ON m.channel_id = c.id
		WHERE kc.key_id = $1
		ORDER BY c.id COLLATE "C", m.model_pattern COLLATE "C"`

// previousChannel picks the channel of keyID for model as the previous image did: the
// first row whose pattern matches. ok is false when none does.
func previousChannel(t *testing.T, pool *pgxpool.Pool, keyID, model string) (found store.Channel, ok bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), previousChannelSQL, keyID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c       store.Channel
			pattern string
		)
		if err := rows.Scan(&c.ID, &c.Protocol, &c.BaseURL, &c.CredentialEnv, &pattern); err != nil {
			t.Fatal(err)
		}
		if !ok && channel.MatchModel(pattern, model) {
			found, ok = c, true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found, ok
}

func TestMigrateTwiceAppliesOnce(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	// 0001 and 0002 on the first run, nothing on the second.
	for run, want := range []int{2, 0} {
		ran, err := store.Migrate(ctx, pool)
		if err != nil || ran != want {
			t.Fatalf("run %d: applied %d, want %d, err %v", run+1, ran, want, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables
		WHERE schemaname = 'access' AND tableowner = 'access' AND tablename <> 'schema_migrations'
		ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, tables) {
		t.Errorf("tables in schema access owned by access = %v, want %v", got, tables)
	}
	var elsewhere int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE tablename = ANY($1) AND schemaname <> 'access'",
		tables).Scan(&elsewhere); err != nil {
		t.Fatal(err)
	}
	if elsewhere != 0 {
		t.Errorf("%d access tables outside schema access", elsewhere)
	}
}

// The previous access image, which asks by model alone, still gets the Messages
// channel from a schema with 0002 and the seeded Responses channels: the same one
// VerifyKey returns for anthropic-messages.
func TestPreviousImageQueryStillGetsMessagesChannels(t *testing.T) {
	pool := pgtest.Migrated(t, "access", store.Schema, migrations.FS)
	ctx := context.Background()
	cfg := config.Seed{
		KeyPepper:     []byte("test-pepper"),
		SeedSecret:    []byte("test-seed-secret"),
		MockBaseURL:   "http://mock-upstream:8090",
		OllamaBaseURL: "http://host.docker.internal:11434",
		OllamaModel:   "qwen2.5-coder:7b",
	}
	keys, err := seed.Run(ctx, pool, cfg, 2)
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	for _, k := range keys {
		keyID := "key-" + k.TenantID
		for _, model := range []string{"mock-fast", "mock-", cfg.OllamaModel, "claude-not-seeded"} {
			previous, found := previousChannel(t, pool, keyID, model)
			current, err := s.VerifyKey(ctx, apikey.HMAC(cfg.KeyPepper, k.Key), model, channel.Protocol)
			switch {
			case !found:
				if !errors.Is(err, store.ErrModelNotAllowed) {
					t.Errorf("%s, %s: the previous query found nothing, VerifyKey gave %v", keyID, model, err)
				}
			case err != nil:
				t.Errorf("%s, %s: the previous query found %s, VerifyKey failed: %v", keyID, model, previous.ID, err)
			case previous != current.Channel || previous.Protocol != channel.Protocol:
				t.Errorf("%s, %s: the previous query found %+v, want the Messages channel %+v",
					keyID, model, previous, current.Channel)
			}
		}
		if previous, _ := previousChannel(t, pool, keyID, "mock-fast"); previous.ID != "mock" {
			t.Errorf("%s: the previous query found %q for mock-fast, want mock", keyID, previous.ID)
		}
		if previous, _ := previousChannel(t, pool, keyID, cfg.OllamaModel); previous.ID != "ollama" {
			t.Errorf("%s: the previous query found %q for %s, want ollama", keyID, previous.ID, cfg.OllamaModel)
		}
	}
}
