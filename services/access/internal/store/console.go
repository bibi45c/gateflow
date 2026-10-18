package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// What the console queries can answer besides a result.
var (
	// ErrUserNotFound means no console user has the username.
	ErrUserNotFound = errors.New("store: console user not found")
	// ErrTenantNotFound means no tenant has the id.
	ErrTenantNotFound = errors.New("store: tenant not found")
	// ErrKeyNotFound means the tenant has no key with the id.
	ErrKeyNotFound = errors.New("store: key not found")
)

// ConsoleUser is someone who signs in to the console. TenantID is empty for an admin.
type ConsoleUser struct {
	ID           string
	Username     string
	PasswordHash string
	Role         string
	TenantID     string
}

// Tenant is a tenant and how many of its keys are not revoked.
type Tenant struct {
	ID         string
	Name       string
	ActiveKeys int
	CreatedAt  time.Time
}

// Key describes a gateway key without its secret.
type Key struct {
	ID             string
	TenantID       string
	Name           string
	Prefix         string
	PriceVersionID string
	CreatedAt      time.Time
	RevokedAt      *time.Time
}

// ChannelInfo is a channel and the model patterns it serves.
type ChannelInfo struct {
	ID       string
	Protocol string
	BaseURL  string
	Models   []string
}

// PriceVersionInfo is a price version and when it was written.
type PriceVersionInfo struct {
	PriceVersion
	CreatedAt time.Time
}

// UserByName returns the console user called username, or ErrUserNotFound.
func (s *Store) UserByName(ctx context.Context, username string) (ConsoleUser, error) {
	var (
		u      ConsoleUser
		tenant *string
	)
	err := s.pool.QueryRow(ctx, `SELECT id, username, password_hash, role, tenant_id
		FROM access.console_users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &tenant)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ConsoleUser{}, ErrUserNotFound
	case err != nil:
		return ConsoleUser{}, unavailable(err)
	}
	if tenant != nil {
		u.TenantID = *tenant
	}
	return u, nil
}

// Tenants returns every tenant, by id.
func (s *Store) Tenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.id, t.name, t.created_at,
			count(k.id) FILTER (WHERE k.revoked_at IS NULL)
		FROM access.tenants AS t LEFT JOIN access.api_keys AS k ON k.tenant_id = t.id
		GROUP BY t.id ORDER BY t.id COLLATE "C"`)
	if err != nil {
		return nil, unavailable(err)
	}
	tenants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tenant, error) {
		var t Tenant
		err := row.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.ActiveKeys)
		return t, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return tenants, nil
}

const keyColumns = `id, tenant_id, name, key_prefix, price_version_id, created_at, revoked_at`

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.PriceVersionID, &k.CreatedAt, &k.RevokedAt)
	return k, err
}

// Keys returns the keys of tenantID, newest first, or ErrTenantNotFound.
func (s *Store) Keys(ctx context.Context, tenantID string) ([]Key, error) {
	if err := s.tenantExists(ctx, s.pool, tenantID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+keyColumns+` FROM access.api_keys
		WHERE tenant_id = $1 ORDER BY created_at DESC, id`, tenantID)
	if err != nil {
		return nil, unavailable(err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) { return scanKey(row) })
	if err != nil {
		return nil, unavailable(err)
	}
	return keys, nil
}

// CreateKey stores a new key of tenantID under keyHMAC, at the newest price version
// and on every channel. It returns ErrTenantNotFound for an unknown tenant and
// ErrPriceVersionNotFound when there is no price version.
func (s *Store) CreateKey(ctx context.Context, tenantID, name, prefix string, keyHMAC []byte) (Key, error) {
	var key Key
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.tenantExists(ctx, tx, tenantID); err != nil {
			return err
		}
		var priceVersion string
		err := tx.QueryRow(ctx, `SELECT id FROM access.price_versions
			ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&priceVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPriceVersionNotFound
		}
		if err != nil {
			return unavailable(err)
		}
		key, err = scanKey(tx.QueryRow(ctx, `INSERT INTO access.api_keys
				(id, tenant_id, key_hmac, price_version_id, name, key_prefix)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+keyColumns,
			newKeyID(), tenantID, keyHMAC, priceVersion, name, prefix))
		if err != nil {
			return unavailable(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO access.api_key_channels (key_id, channel_id)
			SELECT $1, id FROM access.channels`, key.ID); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		return Key{}, err
	}
	return key, nil
}

// RevokeKey revokes keyID of tenantID and returns it. Revoking a revoked key keeps
// its first revocation time. It returns ErrKeyNotFound when the tenant has no such
// key.
func (s *Store) RevokeKey(ctx context.Context, tenantID, keyID string) (Key, error) {
	key, err := scanKey(s.pool.QueryRow(ctx, `UPDATE access.api_keys
		SET revoked_at = coalesce(revoked_at, now())
		WHERE tenant_id = $1 AND id = $2 RETURNING `+keyColumns, tenantID, keyID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Key{}, ErrKeyNotFound
	case err != nil:
		return Key{}, unavailable(err)
	}
	return key, nil
}

// Channels returns every channel with its model patterns, by id.
func (s *Store) Channels(ctx context.Context) ([]ChannelInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.protocol, c.base_url,
			coalesce(array_agg(m.model_pattern ORDER BY m.model_pattern COLLATE "C")
				FILTER (WHERE m.model_pattern IS NOT NULL), '{}')
		FROM access.channels AS c LEFT JOIN access.channel_models AS m ON m.channel_id = c.id
		GROUP BY c.id ORDER BY c.id COLLATE "C"`)
	if err != nil {
		return nil, unavailable(err)
	}
	channels, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChannelInfo, error) {
		var c ChannelInfo
		err := row.Scan(&c.ID, &c.Protocol, &c.BaseURL, &c.Models)
		return c, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return channels, nil
}

// PriceVersions returns every price version, newest first.
func (s *Store) PriceVersions(ctx context.Context) ([]PriceVersionInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, input_micro_per_token, output_micro_per_token, created_at
		FROM access.price_versions ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, unavailable(err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PriceVersionInfo, error) {
		var v PriceVersionInfo
		err := row.Scan(&v.ID, &v.InputMicroPerToken, &v.OutputMicroPerToken, &v.CreatedAt)
		return v, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return versions, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) tenantExists(ctx context.Context, db queryRower, tenantID string) error {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM access.tenants WHERE id = $1)`,
		tenantID).Scan(&exists); err != nil {
		return unavailable(err)
	}
	if !exists {
		return ErrTenantNotFound
	}
	return nil
}

// newKeyID returns key_ and 16 random hex digits.
func newKeyID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "key_" + hex.EncodeToString(b[:])
}
