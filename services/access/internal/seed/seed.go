// Package seed fills the access schema with the fixed tenants, keys, channels and
// price version that the local stack and the load tests use. The key and channel
// APIs arrive later, so the seed writes to the service's own schema directly.
package seed

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/channel"
	"github.com/bibi45c/gateflow/services/access/internal/config"
)

// PriceVersionID is the price version every seeded key is charged at.
const PriceVersionID = "pv-seed-1"

const (
	inputMicroPerToken  = 3
	outputMicroPerToken = 15
	mockModels          = "mock-*"
)

// Key is a seeded tenant and its gateway key.
type Key struct {
	TenantID string
	Key      string
}

type seedChannel struct {
	id      string
	baseURL string
	models  []string
}

// DeriveKey returns the gateway key of tenantID: gf_ followed by
// base64url(HMAC-SHA256(secret, tenantID)). The same secret always gives the same
// keys, so a seeded key never needs to be stored.
func DeriveKey(secret []byte, tenantID string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tenantID))
	return "gf_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Run seeds tenant-00 to tenant-<tenants> in one transaction and returns their keys.
// tenant-00 is the tenant that metering leaves without balance. Each tenant gets one
// key that may use the mock and ollama channels at price version pv-seed-1; only the
// key's HMAC is stored.
//
// Tenants, keys and the price version are inserted only when absent. The channels
// and their models are replaced by the current settings, so a changed base URL or
// OLLAMA_MODEL takes effect on the next run. Run refuses channel settings that break
// the channel rules, naming the variable, before touching the database, and refuses
// keys already seeded with another pepper or seed secret, which would not verify.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg config.Seed, tenants int) ([]Key, error) {
	if tenants < 0 {
		return nil, fmt.Errorf("seed: tenant count %d is below 0", tenants)
	}
	channels, err := seedChannels(cfg)
	if err != nil {
		return nil, err
	}
	keys := make([]Key, tenants+1)
	for i := range keys {
		id := fmt.Sprintf("tenant-%02d", i)
		keys[i] = Key{TenantID: id, Key: DeriveKey(cfg.SeedSecret, id)}
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO access.price_versions (id, input_micro_per_token, output_micro_per_token)
			VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
			PriceVersionID, inputMicroPerToken, outputMicroPerToken); err != nil {
			return fmt.Errorf("seed price version: %w", err)
		}
		if err := upsertChannels(ctx, tx, channels); err != nil {
			return err
		}
		return insertTenants(ctx, tx, cfg.KeyPepper, keys, channels)
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// seedChannels checks the channel settings against the channel rules, naming the
// variable at fault but not its value, which may hold a credential.
func seedChannels(cfg config.Seed) ([]seedChannel, error) {
	checks := []struct {
		name, value string
		check       func(string) error
	}{
		{config.EnvMockBaseURL, cfg.MockBaseURL, channel.CheckBaseURL},
		{config.EnvOllamaBaseURL, cfg.OllamaBaseURL, channel.CheckBaseURL},
		{config.EnvOllamaModel, cfg.OllamaModel, channel.CheckModelPattern},
	}
	for _, c := range checks {
		if err := c.check(c.value); err != nil {
			return nil, fmt.Errorf("%s %w", c.name, err)
		}
	}
	return []seedChannel{
		{id: "mock", baseURL: cfg.MockBaseURL, models: []string{mockModels}},
		{id: "ollama", baseURL: cfg.OllamaBaseURL, models: []string{cfg.OllamaModel}},
	}, nil
}

// upsertChannels writes each channel and leaves it serving exactly its models.
func upsertChannels(ctx context.Context, tx pgx.Tx, channels []seedChannel) error {
	for _, c := range channels {
		if _, err := tx.Exec(ctx, `INSERT INTO access.channels (id, protocol, base_url, credential_env)
			VALUES ($1, $2, $3, '')
			ON CONFLICT (id) DO UPDATE SET protocol = excluded.protocol, base_url = excluded.base_url,
				credential_env = excluded.credential_env`,
			c.id, channel.Protocol, c.baseURL); err != nil {
			return fmt.Errorf("seed channel %s: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM access.channel_models
			WHERE channel_id = $1 AND model_pattern <> ALL($2::text[])`, c.id, c.models); err != nil {
			return fmt.Errorf("seed channel %s models: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access.channel_models (channel_id, model_pattern)
			SELECT $1::text, pattern FROM unnest($2::text[]) AS pattern
			ON CONFLICT DO NOTHING`, c.id, c.models); err != nil {
			return fmt.Errorf("seed channel %s models: %w", c.id, err)
		}
	}
	return nil
}

// insertTenants adds the tenants and their keys that are absent, and gives every
// seeded key all the channels.
func insertTenants(ctx context.Context, tx pgx.Tx, pepper []byte, keys []Key, channels []seedChannel) error {
	tenantIDs := make([]string, len(keys))
	keyIDs := make([]string, len(keys))
	hmacs := make([][]byte, len(keys))
	for i, k := range keys {
		tenantIDs[i], keyIDs[i], hmacs[i] = k.TenantID, "key-"+k.TenantID, apikey.HMAC(pepper, k.Key)
	}
	channelIDs := make([]string, len(channels))
	for i, c := range channels {
		channelIDs[i] = c.id
	}

	if _, err := tx.Exec(ctx, `INSERT INTO access.tenants (id, name)
		SELECT id, id FROM unnest($1::text[]) AS id
		ON CONFLICT (id) DO NOTHING`, tenantIDs); err != nil {
		return fmt.Errorf("seed tenants: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access.api_keys (id, tenant_id, key_hmac, price_version_id)
		SELECT k.id, k.tenant_id, k.key_hmac, $4
		FROM unnest($1::text[], $2::text[], $3::bytea[]) AS k (id, tenant_id, key_hmac)
		ON CONFLICT (id) DO NOTHING`, keyIDs, tenantIDs, hmacs, PriceVersionID); err != nil {
		return fmt.Errorf("seed keys: %w", err)
	}
	if err := checkStoredKeys(ctx, tx, keyIDs, hmacs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access.api_key_channels (key_id, channel_id)
		SELECT key_id, channel_id FROM unnest($1::text[]) AS key_id CROSS JOIN unnest($2::text[]) AS channel_id
		ON CONFLICT DO NOTHING`, keyIDs, channelIDs); err != nil {
		return fmt.Errorf("seed key channels: %w", err)
	}
	return nil
}

// checkStoredKeys fails when a key that was already there holds another HMAC, so the
// key printed for it would not verify.
func checkStoredKeys(ctx context.Context, tx pgx.Tx, keyIDs []string, hmacs [][]byte) error {
	var stale string
	err := tx.QueryRow(ctx, `SELECT a.id FROM access.api_keys AS a
		JOIN unnest($1::text[], $2::bytea[]) AS k (id, key_hmac) ON a.id = k.id
		WHERE a.key_hmac <> k.key_hmac
		ORDER BY a.id LIMIT 1`, keyIDs, hmacs).Scan(&stale)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("seed keys: %w", err)
	}
	return fmt.Errorf("seed: key %s was seeded with another %s or %s; reset the database or restore them",
		stale, config.EnvKeyPepper, config.EnvSeedSecret)
}
