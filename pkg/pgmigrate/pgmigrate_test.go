package pgmigrate_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/pkg/pgtest"
)

func files(names ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(names); i += 2 {
		fsys[names[i]] = &fstest.MapFile{Data: []byte(names[i+1])}
	}
	return fsys
}

func TestLoadOrdersByVersionAndRejectsOtherFiles(t *testing.T) {
	got, err := pgmigrate.Load(files("0002_b.sql", "B", "0001_a.sql", "A", "0010_c.sql", "C"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if strings.Join(names, " ") != "0001_a.sql 0002_b.sql 0010_c.sql" {
		t.Errorf("order = %v", names)
	}
	for _, bad := range []fstest.MapFS{
		files("1_a.sql", "A"),
		files("0001_A.sql", "A"),
		files("0001_a.sql", "A", "README.md", "notes"),
		files("0001_a.sql", "A", "0001_b.sql", "B"),
	} {
		if _, err := pgmigrate.Load(bad); err == nil {
			t.Errorf("Load accepted %v", bad)
		}
	}
}

var migrations = files(
	"0001_tenants.sql", "CREATE TABLE tenants (id text PRIMARY KEY);\nCREATE TABLE keys (id text PRIMARY KEY);",
	"0002_names.sql", "ALTER TABLE tenants ADD COLUMN name text NOT NULL DEFAULT '';",
)

func TestApplyTwiceChangesNothing(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	for run, want := range []int{2, 0} {
		ran, err := pgmigrate.Apply(ctx, pool, "access", migrations)
		if err != nil || ran != want {
			t.Fatalf("run %d: applied %d, want %d, err %v", run+1, ran, want, err)
		}
	}
	var tables, versions int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_tables WHERE schemaname = 'access' AND tableowner = 'access'),
		(SELECT count(*) FROM access.schema_migrations)`).Scan(&tables, &versions); err != nil {
		t.Fatal(err)
	}
	// tenants, keys and the version table, all in the service's schema and owned by its role.
	if tables != 3 || versions != 2 {
		t.Errorf("tables = %d, versions = %d", tables, versions)
	}
}

func TestFailedMigrationLeavesNothing(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	broken := files("0001_ok.sql", "CREATE TABLE tenants (id text);", "0002_bad.sql", "CREATE TABLE nope (")
	if _, err := pgmigrate.Apply(ctx, pool, "access", broken); err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("err = %v, want one naming 0002_bad.sql", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'access'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d tables left after a failed run", count)
	}
}

func TestRenamedMigrationFails(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	if _, err := pgmigrate.Apply(ctx, pool, "access", files("0001_a.sql", "CREATE TABLE a (id int);")); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, pool, "access", files("0001_b.sql", "CREATE TABLE a (id int);")); err == nil {
		t.Error("a renamed applied migration was accepted")
	}
}

func TestReplicasStartingTogetherApplyOnce(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "metering")
	var wg sync.WaitGroup
	ran := make([]int, 4)
	errs := make([]error, 4)
	for i := range ran {
		wg.Go(func() { ran[i], errs[i] = pgmigrate.Apply(context.Background(), pool, "accounting", migrations) })
	}
	wg.Wait()
	total := 0
	for i := range ran {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		total += ran[i]
	}
	if total != 2 {
		t.Errorf("applied %d migrations in total, want 2", total)
	}
}
