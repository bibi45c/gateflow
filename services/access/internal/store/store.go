// Package store connects the access service to its Postgres schema.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

// Schema is the schema the access role owns and the service's tables live in.
const Schema = "access"

// OpenTimeout bounds connecting and migrating, so a missing database fails the start
// instead of hanging it.
const OpenTimeout = 30 * time.Second

// Open connects to the database at url, applies the pending migrations and returns
// the pool, which the caller closes.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate schema %s: %w", Schema, err)
	}
	return pool, nil
}

// Migrate applies the access migrations db has not run yet and returns how many ran.
func Migrate(ctx context.Context, db pgmigrate.Beginner) (int, error) {
	return pgmigrate.Apply(ctx, db, Schema, migrations.FS)
}
