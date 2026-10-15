package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/services/access/internal/channel"
)

// What a query can answer besides a result. Any other error wraps ErrUnavailable.
var (
	// ErrInvalidKey means no key has the HMAC, or the key is revoked.
	ErrInvalidKey = errors.New("store: key is unknown or revoked")
	// ErrModelNotAllowed means none of the key's channels serves the model over the
	// protocol.
	ErrModelNotAllowed = errors.New("store: key has no channel for the model")
	// ErrPriceVersionNotFound means no price version has the id.
	ErrPriceVersionNotFound = errors.New("store: price version not found")
	// ErrUnavailable means the database did not answer the query.
	ErrUnavailable = errors.New("store: database unavailable")
)

// Channel is the upstream a verified key's call goes to.
type Channel struct {
	ID            string
	Protocol      string
	BaseURL       string
	CredentialEnv string
}

// VerifiedKey is a gateway key and the channel that serves the requested model.
type VerifiedKey struct {
	TenantID       string
	KeyID          string
	PriceVersionID string
	Channel        Channel
}

// PriceVersion is an immutable pair of prices, in micro-units per token.
type PriceVersion struct {
	ID                  string
	InputMicroPerToken  int64
	OutputMicroPerToken int64
}

// Store runs the access service's queries on a pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store on pool, which the caller still closes.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// VerifyKey finds the key stored under keyHMAC and the channel it may use for
// model over protocol. Only the key's channels that speak protocol count. It returns
// ErrInvalidKey when there is no such key or it is revoked, and ErrModelNotAllowed
// when none of those channels has a pattern matching model; when several do, the
// lowest channel id wins. Whenever the key was found, the result carries its
// TenantID and KeyID, also next to an error.
func (s *Store) VerifyKey(ctx context.Context, keyHMAC []byte, model, protocol string) (VerifiedKey, error) {
	var (
		key     VerifiedKey
		revoked bool
	)
	err := s.pool.QueryRow(ctx, `SELECT id, tenant_id, price_version_id, revoked_at IS NOT NULL
		FROM access.api_keys WHERE key_hmac = $1`, keyHMAC).
		Scan(&key.KeyID, &key.TenantID, &key.PriceVersionID, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return VerifiedKey{}, ErrInvalidKey
	case err != nil:
		return VerifiedKey{}, unavailable(err)
	case revoked:
		return key, ErrInvalidKey
	}

	rows, err := s.pool.Query(ctx, `SELECT c.id, c.protocol, c.base_url, c.credential_env, m.model_pattern
		FROM access.api_key_channels AS kc
		JOIN access.channels AS c ON c.id = kc.channel_id
		JOIN access.channel_models AS m ON m.channel_id = c.id
		WHERE kc.key_id = $1 AND c.protocol = $2
		ORDER BY c.id COLLATE "C", m.model_pattern COLLATE "C"`, key.KeyID, protocol)
	if err != nil {
		return key, unavailable(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var (
			c       Channel
			pattern string
		)
		if err := rows.Scan(&c.ID, &c.Protocol, &c.BaseURL, &c.CredentialEnv, &pattern); err != nil {
			return key, unavailable(err)
		}
		if !found && channel.MatchModel(pattern, model) {
			key.Channel, found = c, true
		}
	}
	if err := rows.Err(); err != nil {
		return key, unavailable(err)
	}
	if !found {
		return key, ErrModelNotAllowed
	}
	return key, nil
}

// PriceVersion returns the price version id, or ErrPriceVersionNotFound.
func (s *Store) PriceVersion(ctx context.Context, id string) (PriceVersion, error) {
	version := PriceVersion{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT input_micro_per_token, output_micro_per_token
		FROM access.price_versions WHERE id = $1`, id).
		Scan(&version.InputMicroPerToken, &version.OutputMicroPerToken)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PriceVersion{}, ErrPriceVersionNotFound
	case err != nil:
		return PriceVersion{}, unavailable(err)
	}
	return version, nil
}

func unavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
