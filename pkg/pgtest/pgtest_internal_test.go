package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const testName = "gftest_0123456789abcdef"

// Every query parameter that could pick the database is dropped, the path names the
// generated database and the other parameters are kept as they were.
func TestTargetURLKeepsOnlyTheGeneratedDatabase(t *testing.T) {
	const base = "postgres://u:p@localhost:5432/postgres"
	want := "postgres://u:p@localhost:5432/" + testName
	for _, tc := range []struct{ admin, want string }{
		{base, want},
		{"postgres://u:p@localhost:5432", want},
		{"postgres://u:p@localhost:5432/", want},
		{"postgresql://u:p@localhost:5432/postgres", "postgresql://u:p@localhost:5432/" + testName},
		{base + "?dbname=x", want},
		{base + "?database=x", want},
		{base + "?DBNAME=x", want},
		{base + "?Database=x", want},
		{base + "?dbn%61me=x", want},
		{base + "?dbname=a&dbname=b", want},
		{base + "?dbname=a&database=b&DBNAME=c", want},
		{base + "?dbname=", want},
		{base + "?sslmode=disable& dbname=canary", want + "?sslmode=disable"},
		{base + "?dbname =canary&sslmode=disable", want + "?sslmode=disable"},
		{base + "?%20database%20=canary&sslmode=disable", want + "?sslmode=disable"},
		{base + "?sslmode=disable&application_name=t", want + "?sslmode=disable&application_name=t"},
		{base + "?sslmode=disable&dbname=x&application_name=t&database=y", want + "?sslmode=disable&application_name=t"},
		{base + "?application_name=t&sslmode=disable&dbname=x", want + "?application_name=t&sslmode=disable"},
	} {
		got, err := targetURL(tc.admin, testName)
		if err != nil {
			t.Errorf("%s: %v", tc.admin, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.admin, got, tc.want)
		}
		again, _ := targetURL(tc.admin, testName)
		if again != got {
			t.Errorf("%s: got %s, then %s", tc.admin, got, again)
		}
		wantParsedTarget(t, got)
	}
}

// pgx itself must read the rewritten URL as the generated database with no
// leftover database parameter.
func wantParsedTarget(t *testing.T, target string) {
	t.Helper()
	config, err := pgconn.ParseConfig(target)
	if err != nil {
		t.Errorf("%s: %v", target, err)
		return
	}
	if config.Database != testName {
		t.Errorf("%s: pgx targets %q, want %q", target, config.Database, testName)
	}
	for key := range config.RuntimeParams {
		if selectsDatabase(key) {
			t.Errorf("%s: runtime parameter %q left over", target, key)
		}
	}
	if strings.Contains(target, "application_name=t") && config.RuntimeParams["application_name"] != "t" {
		t.Errorf("%s: application_name is %q, want t", target, config.RuntimeParams["application_name"])
	}
}

// The forms the rewrite guards against really do move pgx to another database, so
// the cases above are not vacuous.
func TestPgxLetsQueryParametersPickTheDatabase(t *testing.T) {
	const base = "postgres://u:p@localhost:5432/postgres"
	for _, query := range []string{
		"?dbname=canary",
		"?database=canary",
		"?dbn%61me=canary",
		"?sslmode=disable& dbname=canary",
		"?application_name=t#&dbname=canary",
	} {
		config, err := pgconn.ParseConfig(base + query)
		if err != nil {
			t.Errorf("%s: %v", query, err)
		} else if config.Database != "canary" {
			t.Errorf("%s: pgx targets %q, want canary", query, config.Database)
		}
	}
}

// URLs pgx would read as keyword/value settings are refused, since their path
// would not pick the database, and so are URLs with a fragment, since Go and pgx
// disagree on where the query ends.
func TestTargetURLRejectsOtherForms(t *testing.T) {
	const base = "postgres://u:p@localhost:5432/postgres"
	for _, admin := range []string{
		"host=localhost dbname=postgres",
		"mysql://u:p@localhost/postgres",
		"localhost:5432/postgres",
		base + "?application_name=t#&dbname=canary",
		base + "#&dbname=canary",
		base + "#x",
	} {
		if got, err := targetURL(admin, testName); err == nil {
			t.Errorf("%s: got %s, want an error", admin, got)
		}
	}
}

type fakeRow struct {
	value string
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.value
	return nil
}

type fakeConn struct {
	row   fakeRow
	execs []string
}

func (c *fakeConn) QueryRow(context.Context, string, ...any) pgx.Row { return c.row }

func (c *fakeConn) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.execs = append(c.execs, sql)
	return pgconn.CommandTag{}, nil
}

// The init SQL runs only when the connection's current database is the wanted one.
func TestInitDatabaseChecksCurrentDatabase(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		row     fakeRow
		wantRun bool
	}{
		{fakeRow{value: testName}, true},
		{fakeRow{value: "postgres"}, false},
		{fakeRow{value: testName + "x"}, false},
		{fakeRow{value: strings.ToUpper(testName)}, false},
		{fakeRow{err: errors.New("boom")}, false},
	} {
		conn := &fakeConn{row: tc.row}
		err := initDatabase(ctx, conn, testName, "INIT")
		if tc.wantRun && (err != nil || len(conn.execs) != 1 || conn.execs[0] != "INIT") {
			t.Errorf("current %q: err %v, execs %q, want INIT run once", tc.row.value, err, conn.execs)
		}
		if !tc.wantRun && (err == nil || len(conn.execs) != 0) {
			t.Errorf("current %q: err %v, execs %q, want an error and nothing run", tc.row.value, err, conn.execs)
		}
	}
}

// Against a real server, a connection to another database fails the check and the
// init SQL leaves that database untouched.
func TestInitDatabaseRefusesAnotherDatabase(t *testing.T) {
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skipf("set %s to a Postgres superuser URL to run this test", EnvURL)
	}
	ctx := context.Background()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	canary := "gfcanary_" + hex.EncodeToString(suffix)
	ident := pgx.Identifier{canary}.Sanitize()
	exec(t, admin, "CREATE DATABASE "+ident)
	t.Cleanup(func() { exec(t, admin, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)") })

	config, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = canary
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	err = initDatabase(ctx, conn, testName, "CREATE SCHEMA gfcanary_marker")
	if err == nil || !strings.Contains(err.Error(), canary) {
		t.Errorf("got %v, want a mismatch error naming %s", err, canary)
	}
	var schemas int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_namespace WHERE nspname = 'gfcanary_marker'").Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas != 0 {
		t.Errorf("init SQL ran in %s", canary)
	}
}
