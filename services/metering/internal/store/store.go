// Package store connects the metering service to its accounting schema and keeps
// that schema's migrations and test wallets.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/services/metering/migrations"
)

// Schema is the Postgres schema the metering role owns.
const Schema = "accounting"

// OpenTimeout bounds connecting and migrating at startup.
const OpenTimeout = 30 * time.Second

// Open connects to the database at url, applies the migrations and returns the
// pool. It gives up after OpenTimeout. Errors never show the password.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, OpenTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if _, err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Migrate applies the migrations the schema has not recorded yet and returns how
// many it ran.
func Migrate(ctx context.Context, db pgmigrate.Beginner) (int, error) {
	ran, err := pgmigrate.Apply(ctx, db, Schema, migrations.FS)
	if err != nil {
		return 0, fmt.Errorf("store: migrate: %w", err)
	}
	return ran, nil
}
