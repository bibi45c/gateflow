package store_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
	"github.com/bibi45c/gateflow/services/metering/migrations"
)

// row maps column names to values; nil is NULL.
type row map[string]any

// with returns a copy of r with changes applied.
func (r row) with(changes row) row {
	out := maps.Clone(r)
	maps.Copy(out, changes)
	return out
}

func insert(ctx context.Context, db *pgxpool.Pool, table string, r row) error {
	columns := slices.Sorted(maps.Keys(r))
	params := make([]string, len(columns))
	args := make([]any, len(columns))
	for i, column := range columns {
		params[i] = fmt.Sprintf("$%d", i+1)
		args[i] = r[column]
	}
	_, err := db.Exec(ctx, fmt.Sprintf("INSERT INTO accounting.%s (%s) VALUES (%s)",
		table, strings.Join(columns, ", "), strings.Join(params, ", ")), args...)
	return err
}

// reserved is a valid open reservation r-n for call call-n, attempt 1.
func reserved(n int) row {
	return row{
		"reservation_id":    fmt.Sprintf("r-%d", n),
		"call_id":           fmt.Sprintf("call-%d", n),
		"attempt_id":        1,
		"tenant_id":         "tenant-01",
		"price_version_id":  "pv-1",
		"input_price":       int64(3),
		"output_price":      int64(15),
		"max_input_tokens":  int64(1000),
		"max_output_tokens": int64(500),
		"max_cost":          int64(10500),
		"reserve_digest":    []byte("reserve"),
	}
}

// settled is reserved(n) settled as completed.
func settled(n int) row {
	return reserved(n).with(row{
		"status":        "settled",
		"input_tokens":  int64(100),
		"output_tokens": int64(50),
		"outcome":       "completed",
		"charged":       int64(1050),
		"settle_digest": []byte("settle"),
		"settled_at":    time.Now(),
	})
}

// entry is a valid ledger entry le-n for reservation r-n.
func entry(n int) row {
	return row{
		"ledger_entry_id": fmt.Sprintf("le-%d", n),
		"reservation_id":  fmt.Sprintf("r-%d", n),
		"call_id":         fmt.Sprintf("call-%d", n),
		"attempt_id":      1,
		"tenant_id":       "tenant-01",
		"amount":          int64(1050),
		"input_tokens":    int64(100),
		"output_tokens":   int64(50),
		"outcome":         "completed",
	}
}

// migrated returns a pool, connected as the metering role, to a new database with
// wallet tenant-01, open reservation r-1, and settled reservation r-2 with ledger
// entry le-2.
func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.Migrated(t, "metering", store.Schema, migrations.FS)
	ctx := context.Background()
	for _, step := range []struct {
		table string
		row   row
	}{
		{"wallets", row{"tenant_id": "tenant-01", "balance": int64(1_000_000), "held": int64(10500)}},
		{"reservations", reserved(1)},
		{"reservations", settled(2)},
		{"ledger_entries", entry(2)},
	} {
		if err := insert(ctx, pool, step.table, step.row); err != nil {
			t.Fatalf("insert valid %s row: %v", step.table, err)
		}
	}
	return pool
}

// wantRejected fails unless err is a Postgres error with code and, when constraint
// is not empty, that constraint.
func wantRejected(t *testing.T, err error, code, constraint, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
		t.Errorf("%s: got %v, want SQLSTATE %s on %q", what, err, code, constraint)
	}
}

func TestMigrateTwiceAppliesOnce(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "metering")
	ctx := context.Background()
	for run, want := range []int{1, 0} {
		ran, err := store.Migrate(ctx, pool)
		if err != nil || ran != want {
			t.Fatalf("run %d: applied %d, want %d, err %v", run+1, ran, want, err)
		}
	}
	var owned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables
		WHERE schemaname = 'accounting' AND tableowner = 'metering'
		AND tablename IN ('wallets', 'reservations', 'ledger_entries')`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != 3 {
		t.Errorf("%d of the 3 tables are in schema accounting and owned by metering", owned)
	}
}

func TestDatabaseRejectsInvalidRows(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	const (
		check  = "23514"
		unique = "23505"
		fkey   = "23503"
	)
	for _, tc := range []struct {
		name, table string
		row         row
		code        string
		constraint  string
	}{
		{"held above balance", "wallets", row{"tenant_id": "w-1", "balance": int64(5), "held": int64(6)}, check, "wallets_available_check"},
		{"negative held", "wallets", row{"tenant_id": "w-2", "balance": int64(5), "held": int64(-1)}, check, "wallets_held_check"},
		{"negative balance", "wallets", row{"tenant_id": "w-3", "balance": int64(-1), "held": int64(0)}, check, "wallets_available_check"},
		{"negative input price", "reservations", reserved(10).with(row{"input_price": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative output price", "reservations", reserved(11).with(row{"output_price": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative max input tokens", "reservations", reserved(12).with(row{"max_input_tokens": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative max output tokens", "reservations", reserved(13).with(row{"max_output_tokens": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative max cost", "reservations", reserved(14).with(row{"max_cost": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative input tokens", "reservations", settled(15).with(row{"input_tokens": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative output tokens", "reservations", settled(16).with(row{"output_tokens": int64(-1)}), check, "reservations_nonnegative_check"},
		{"negative charge", "reservations", settled(17).with(row{"charged": int64(-1)}), check, "reservations_nonnegative_check"},
		{"charge above max cost", "reservations", settled(18).with(row{"charged": int64(10501)}), check, "reservations_charged_check"},
		{"unknown status", "reservations", reserved(19).with(row{"status": "pending"}), check, "reservations_status_check"},
		{"unknown outcome", "reservations", settled(20).with(row{"outcome": "timeout"}), check, "reservations_outcome_check"},
		{"settled without outcome", "reservations", settled(21).with(row{"outcome": nil}), check, "reservations_settled_check"},
		{"settled without charge", "reservations", settled(22).with(row{"charged": nil}), check, "reservations_settled_check"},
		{"settled without time", "reservations", settled(23).with(row{"settled_at": nil}), check, "reservations_settled_check"},
		{"reserved with an outcome", "reservations", reserved(24).with(row{"outcome": "completed"}), check, "reservations_reserved_check"},
		{"reserved with a charge", "reservations", reserved(25).with(row{"charged": int64(0)}), check, "reservations_reserved_check"},
		{"unknown tenant", "reservations", reserved(26).with(row{"tenant_id": "tenant-99"}), fkey, "reservations_tenant_id_fkey"},
		{"second reserve of an attempt", "reservations", reserved(1).with(row{"reservation_id": "r-1b"}), unique, "reservations_call_attempt_key"},
		{"negative ledger amount", "ledger_entries", entry(1).with(row{"amount": int64(-1)}), check, "ledger_entries_nonnegative_check"},
		{"negative ledger input tokens", "ledger_entries", entry(1).with(row{"input_tokens": int64(-1)}), check, "ledger_entries_nonnegative_check"},
		{"negative ledger output tokens", "ledger_entries", entry(1).with(row{"output_tokens": int64(-1)}), check, "ledger_entries_nonnegative_check"},
		{"unknown ledger outcome", "ledger_entries", entry(1).with(row{"outcome": "timeout"}), check, "ledger_entries_outcome_check"},
		{"unknown reservation", "ledger_entries", entry(30), fkey, "ledger_entries_reservation_id_fkey"},
		{"second entry of a reservation", "ledger_entries", entry(2).with(row{"ledger_entry_id": "le-2b"}), unique, "ledger_entries_reservation_id_key"},
	} {
		wantRejected(t, insert(ctx, pool, tc.table, tc.row), tc.code, tc.constraint, tc.name)
	}

	_, err := pool.Exec(ctx, "UPDATE accounting.wallets SET held = balance + 1 WHERE tenant_id = 'tenant-01'")
	wantRejected(t, err, check, "wallets_available_check", "holding more than the balance")
}

func TestLedgerRejectsUpdateAndDelete(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	for _, sql := range []string{
		"UPDATE accounting.ledger_entries SET amount = 0 WHERE ledger_entry_id = 'le-2'",
		"DELETE FROM accounting.ledger_entries WHERE ledger_entry_id = 'le-2'",
	} {
		_, err := pool.Exec(ctx, sql)
		wantRejected(t, err, "P0001", "", sql) // raise_exception, from the trigger
	}
	var amount int64
	if err := pool.QueryRow(ctx, "SELECT amount FROM accounting.ledger_entries WHERE ledger_entry_id = 'le-2'").Scan(&amount); err != nil {
		t.Fatal(err)
	}
	if amount != 1050 {
		t.Errorf("amount = %d after rejected changes, want 1050", amount)
	}
}

func TestSettlementKeepsNullTokenCounts(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	// Counts as sent: none for the input and zero for the output, which must stay apart.
	counts := row{"input_tokens": nil, "output_tokens": int64(0)}
	if err := insert(ctx, pool, "reservations", settled(40).with(counts)); err != nil {
		t.Fatal(err)
	}
	if err := insert(ctx, pool, "ledger_entries", entry(40).with(counts)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"reservations", "ledger_entries"} {
		var input, output *int64
		if err := pool.QueryRow(ctx, "SELECT input_tokens, output_tokens FROM accounting."+table+
			" WHERE reservation_id = 'r-40'").Scan(&input, &output); err != nil {
			t.Fatal(err)
		}
		if input != nil || output == nil || *output != 0 {
			t.Errorf("%s: input_tokens = %v, output_tokens = %v, want NULL and 0", table, input, output)
		}
	}
}
