package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxSeedTenants is the highest tenant number Seed accepts, so every seeded tenant
// ID has two digits.
const MaxSeedTenants = 99

// SeedTenantID names seeded tenant n, such as tenant-07.
func SeedTenantID(n int) string {
	return fmt.Sprintf("tenant-%02d", n)
}

// Seed creates the test wallets tenant-00 to tenant-NN, where NN is tenants:
// tenant-00 with balance 0, and the others with balance micro-units, all with
// nothing held. It inserts only missing wallets and never resets an existing
// balance, so it can run again. It returns how many wallets it created.
func Seed(ctx context.Context, db *pgxpool.Pool, balance int64, tenants int) (int, error) {
	if balance < 0 {
		return 0, fmt.Errorf("store: seed: balance %d is negative", balance)
	}
	if tenants < 0 || tenants > MaxSeedTenants {
		return 0, fmt.Errorf("store: seed: tenants %d is outside 0 to %d", tenants, MaxSeedTenants)
	}
	created := 0
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		for n := 0; n <= tenants; n++ {
			amount := balance
			if n == 0 {
				amount = 0
			}
			tag, err := tx.Exec(ctx, `INSERT INTO accounting.wallets (tenant_id, balance, held)
				VALUES ($1, $2, 0) ON CONFLICT (tenant_id) DO NOTHING`, SeedTenantID(n), amount)
			if err != nil {
				return err
			}
			created += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: seed: %w", err)
	}
	return created, nil
}

// SeedExtra creates a wallet with balance micro-units for each of tenantIDs that has
// none, such as the console's tenant-demo, and returns how many it created.
func SeedExtra(ctx context.Context, db *pgxpool.Pool, balance int64, tenantIDs []string) (int, error) {
	if balance < 0 {
		return 0, fmt.Errorf("store: seed: balance %d is negative", balance)
	}
	for _, id := range tenantIDs {
		if id == "" || len(id) > 200 {
			return 0, fmt.Errorf("store: seed: tenant id %q is empty or too long", id)
		}
	}
	tag, err := db.Exec(ctx, `INSERT INTO accounting.wallets (tenant_id, balance, held)
		SELECT id, $2, 0 FROM unnest($1::text[]) AS id ON CONFLICT (tenant_id) DO NOTHING`, tenantIDs, balance)
	if err != nil {
		return 0, fmt.Errorf("store: seed: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
