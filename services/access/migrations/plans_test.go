package migrations_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

const foreignKeyViolation = "23503"

// migrationsBefore returns the migrations whose names sort before name.
func migrationsBefore(t *testing.T, name string) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	subset := fstest.MapFS{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && e.Name() < name {
			data, err := fs.ReadFile(migrations.FS, e.Name())
			if err != nil {
				t.Fatal(err)
			}
			subset[e.Name()] = &fstest.MapFile{Data: data}
		}
	}
	return subset
}

// 0004 puts a tenant that existed before it on the unlimited plan, and the previous
// image, which knows only 0001 to 0003, still migrates without error afterwards.
func TestMigration0004KeepsExistingTenantsUnlimited(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	before := migrationsBefore(t, "0004")
	if ran, err := pgmigrate.Apply(ctx, pool, "access", before); err != nil || ran != 3 {
		t.Fatalf("0001 to 0003: applied %d, err %v", ran, err)
	}
	if err := exec(pool, "INSERT INTO access.tenants (id, name) VALUES ('tenant-old', 'old')"); err != nil {
		t.Fatal(err)
	}
	if ran, err := pgmigrate.Apply(ctx, pool, "access", migrations.FS); err != nil || ran != 1 {
		t.Fatalf("0004: applied %d, err %v", ran, err)
	}
	var plan string
	var rpm, concurrent int
	if err := pool.QueryRow(ctx, `SELECT p.id, p.rpm_limit, p.max_concurrent FROM access.tenants AS t
		JOIN access.plans AS p ON p.id = t.plan_id WHERE t.id = 'tenant-old'`).Scan(&plan, &rpm, &concurrent); err != nil {
		t.Fatal(err)
	}
	if plan != "unlimited" || rpm != 0 || concurrent != 0 {
		t.Errorf("existing tenant: plan %s with %d rpm and %d concurrent, want unlimited 0 0", plan, rpm, concurrent)
	}
	if ran, err := pgmigrate.Apply(ctx, pool, "access", before); err != nil || ran != 0 {
		t.Errorf("the previous image's migrations after 0004: applied %d, err %v; want 0 and no error", ran, err)
	}
}

func TestPlanRules(t *testing.T) {
	pool := migrated(t)
	var plans []string
	rows, err := pool.Query(context.Background(),
		"SELECT id || ' ' || rpm_limit || ' ' || max_concurrent FROM access.plans ORDER BY rpm_limit")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, p)
	}
	if got, want := strings.Join(plans, ", "), "unlimited 0 0, free 60 5, standard 600 50, premium 6000 200"; got != want {
		t.Errorf("seeded plans: %s, want %s", got, want)
	}
	for _, bad := range []string{
		"INSERT INTO access.plans (id, rpm_limit, max_concurrent) VALUES ('neg', -1, 0)",
		"INSERT INTO access.plans (id, rpm_limit, max_concurrent) VALUES ('huge', 0, 100001)",
		"INSERT INTO access.plans (id, rpm_limit, max_concurrent) VALUES ('Bad Name', 1, 1)",
	} {
		if err := exec(pool, bad); code(err) != checkViolation {
			t.Errorf("%s: err %v, want a check violation", bad, err)
		}
	}
	if err := exec(pool, "INSERT INTO access.tenants (id, name, plan_id) VALUES ('t', 't', 'gold')"); code(err) != foreignKeyViolation {
		t.Errorf("unknown plan: err %v, want a foreign key violation", err)
	}
}
