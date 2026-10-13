package store_test

import (
	"context"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
	"github.com/bibi45c/gateflow/services/metering/migrations"
)

// wallets returns every wallet's balance by tenant, and fails on any held amount.
func wallets(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT tenant_id, balance, held FROM accounting.wallets")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	balances := map[string]int64{}
	for rows.Next() {
		var tenant string
		var balance, held int64
		if err := rows.Scan(&tenant, &balance, &held); err != nil {
			t.Fatal(err)
		}
		if held != 0 {
			t.Errorf("%s holds %d, want 0", tenant, held)
		}
		balances[tenant] = balance
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return balances
}

func TestSeedCreatesOnlyMissingWallets(t *testing.T) {
	pool := pgtest.Migrated(t, "metering", store.Schema, migrations.FS)
	ctx := context.Background()
	const balance = 100_000_000

	created, err := store.Seed(ctx, pool, balance, 10)
	if err != nil || created != 11 {
		t.Fatalf("first seed: created %d, want 11, err %v", created, err)
	}
	want := map[string]int64{"tenant-00": 0}
	for n := 1; n <= 10; n++ {
		want[store.SeedTenantID(n)] = balance
	}
	if got := wallets(t, pool); !maps.Equal(got, want) {
		t.Fatalf("after the first seed: %v, want %v", got, want)
	}

	// A spent wallet keeps its balance, also when the seed asks for another one.
	if _, err := pool.Exec(ctx, "UPDATE accounting.wallets SET balance = 42 WHERE tenant_id = 'tenant-03'"); err != nil {
		t.Fatal(err)
	}
	want["tenant-03"] = 42
	for _, again := range []int64{balance, 5} {
		created, err := store.Seed(ctx, pool, again, 10)
		if err != nil || created != 0 {
			t.Fatalf("seed again with balance %d: created %d, want 0, err %v", again, created, err)
		}
		if got := wallets(t, pool); !maps.Equal(got, want) {
			t.Errorf("after seeding again with balance %d: %v, want %v", again, got, want)
		}
	}
}

func TestSeedRejectsBadArguments(t *testing.T) {
	ctx := context.Background()
	// The arguments are checked before the database is used, so no pool is needed.
	for _, args := range []struct {
		balance int64
		tenants int
	}{{-1, 10}, {100, -1}, {100, store.MaxSeedTenants + 1}} {
		if _, err := store.Seed(ctx, nil, args.balance, args.tenants); err == nil {
			t.Errorf("Seed accepted balance %d and %d tenants", args.balance, args.tenants)
		}
	}
}
