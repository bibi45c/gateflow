package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/store"
)

var tables = []string{"api_key_channels", "api_keys", "channel_models", "channels", "price_versions", "tenants"}

func TestMigrateTwiceAppliesOnce(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	for run, want := range []int{1, 0} {
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
