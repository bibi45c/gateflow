// Package store connects the access service to its Postgres schema.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/pkg/pgurl"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

// Schema is the schema the access role owns and the service's tables live in.
const Schema = "access"

// OpenTimeout bounds connecting and migrating, so a missing database fails the start
// instead of hanging it.
const OpenTimeout = 30 * time.Second

// PoolConfig parses url into the pool configuration Open uses. A maxConns above 0
// caps the pool's connections; 0 keeps pgx's default. Errors never show the URL.
func PoolConfig(url string, maxConns int32) (*pgxpool.Config, error) {
	cfg, err := pgurl.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	return cfg, nil
}

// Open connects to the database at url with at most maxConns connections, or pgx's
// default when maxConns is 0, applies the pending migrations and returns the pool,
// which the caller closes. Errors never show the password.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	cfg, err := PoolConfig(url, maxConns)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", pgurl.Redact(err, cfg.ConnConfig.Password))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the database: %w", pgurl.Redact(err, cfg.ConnConfig.Password))
	}
	if _, err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate schema %s: %w", Schema, pgurl.Redact(err, cfg.ConnConfig.Password))
	}
	return pool, nil
}

// Migrate applies the access migrations db has not run yet and returns how many ran.
func Migrate(ctx context.Context, db pgmigrate.Beginner) (int, error) {
	return pgmigrate.Apply(ctx, db, Schema, migrations.FS)
}
