package pgtest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/pkg/pgtest"
)

func migrations(sql string) fstest.MapFS {
	return fstest.MapFS{"0001_init.sql": &fstest.MapFile{Data: []byte(sql)}}
}

func wantDenied(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: got %v, want permission denied (42501)", what, err)
	}
}

// Each service role owns only its own schema: it can neither read nor create tables
// in the other service's schema or in public.
func TestServiceRolesAreIsolated(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	access, metering := db.Pool(t, "access"), db.Pool(t, "metering")
	if _, err := pgmigrate.Apply(ctx, access, "access", migrations("CREATE TABLE keys (id text);")); err != nil {
		t.Fatal(err)
	}
	if _, err := pgmigrate.Apply(ctx, metering, "accounting", migrations("CREATE TABLE wallets (id text);")); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		role *pgxpool.Pool
		sql  string
	}{
		{access, "SELECT * FROM accounting.wallets"},
		{access, "SELECT * FROM accounting.schema_migrations"},
		{access, "CREATE TABLE accounting.extra (id int)"},
		{access, "CREATE TABLE public.extra (id int)"},
		{metering, "SELECT * FROM access.keys"},
		{metering, "SELECT * FROM access.schema_migrations"},
		{metering, "CREATE TABLE access.extra (id int)"},
		{metering, "CREATE TABLE public.extra (id int)"},
	} {
		_, err := check.role.Exec(ctx, check.sql)
		wantDenied(t, err, check.sql)
	}
	if _, err := access.Exec(ctx, "SELECT * FROM access.keys"); err != nil {
		t.Errorf("access reading its own table: %v", err)
	}
}

// The compose stack's service URLs log in with the local-only passwords.
func TestServiceRolesLogIn(t *testing.T) {
	db := pgtest.New(t)
	for role, password := range map[string]string{"access": "access-local-only", "metering": "metering-local-only"} {
		u, err := url.Parse(db.URL())
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, password)
		conn, err := pgx.Connect(context.Background(), u.String())
		if err != nil {
			t.Errorf("log in as %s: %v", role, err)
			continue
		}
		var user string
		err = conn.QueryRow(context.Background(), "SELECT current_user").Scan(&user)
		_ = conn.Close(context.Background())
		if err != nil || user != role {
			t.Errorf("logged in as %q (err %v), want %s", user, err, role)
		}
	}
}

// Database query parameters in the superuser URL never move the helper off its own
// database: it initializes, migrates and drops only that database, and leaves the
// databases the parameters name exactly as they were. The plain URL is the control.
// Repeated and combined parameters name two different canaries.
func TestNewUsesOnlyItsOwnDatabase(t *testing.T) {
	admin := os.Getenv(pgtest.EnvURL)
	if admin == "" {
		t.Skipf("set %s to a Postgres superuser URL to run this test", pgtest.EnvURL)
	}
	for _, tc := range []struct{ name, query string }{
		{"plain", ""},
		{"dbname", "dbname={canary}"},
		{"database", "database={canary}"},
		{"encoded dbname", "dbn%61me={canary}"},
		{"repeated dbname", "dbname={canary}&dbname={other}"},
		{"dbname and database", "dbname={canary}&database={other}"},
		{"database and dbname", "database={canary}&dbname={other}"},
		{"other settings", "application_name=gfcanary_app&dbname={canary}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canaries := []string{createCanary(t, admin), createCanary(t, admin)}
			before := snapshots(t, admin, canaries)
			query := strings.NewReplacer("{canary}", canaries[0], "{other}", canaries[1]).Replace(tc.query)
			testURL := withQuery(t, admin, query)
			if query != "" {
				wantRedirected(t, testURL, canaries)
			}
			t.Setenv(pgtest.EnvURL, testURL)

			var name string
			t.Run("helper", func(t *testing.T) {
				db := pgtest.New(t)
				name = db.Name()
				wantTarget(t, db, query)
				pool := db.Pool(t, "access")
				if _, err := pgmigrate.Apply(context.Background(), pool, "access", migrations("CREATE TABLE keys (id text);")); err != nil {
					t.Fatal(err)
				}
			})
			after := snapshots(t, admin, canaries)
			for _, canary := range canaries {
				if after[canary] != before[canary] {
					t.Errorf("canary %s changed from %+v to %+v", canary, before[canary], after[canary])
				}
			}
			wantDropped(t, admin, name)
		})
	}
}

// Two helper databases in one test are separate databases.
func TestDatabasesAreSeparate(t *testing.T) {
	first, second := pgtest.New(t), pgtest.New(t)
	if first.Name() == second.Name() {
		t.Fatalf("both databases are %s", first.Name())
	}
	ctx := context.Background()
	if _, err := first.Pool(t, "").Exec(ctx, "CREATE TABLE public.only_here (id int)"); err != nil {
		t.Fatal(err)
	}
	var tables int
	err := second.Pool(t, "").QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE tablename = 'only_here'").Scan(&tables)
	if err != nil || tables != 0 {
		t.Errorf("second database sees %d tables from the first (err %v)", tables, err)
	}
}

var generatedName = regexp.MustCompile(`^gftest_[0-9a-f]{16}$`)

// wantTarget checks that the URL and the pools all reach exactly db.Name(), and that
// application_name from the query, if any, still reaches the server.
func wantTarget(t *testing.T, db *pgtest.DB, query string) {
	t.Helper()
	if !generatedName.MatchString(db.Name()) {
		t.Errorf("name %q is not a generated name", db.Name())
	}
	config, err := pgconn.ParseConfig(db.URL())
	if err != nil {
		t.Errorf("parse URL: %v", err)
	} else if config.Database != db.Name() {
		t.Errorf("URL targets %q, want %s", config.Database, db.Name())
	}
	for _, role := range []string{"", "access"} {
		var current, app string
		err := db.Pool(t, role).QueryRow(context.Background(),
			"SELECT current_database(), current_setting('application_name')").Scan(&current, &app)
		if err != nil || current != db.Name() {
			t.Errorf("role %q is in %q (err %v), want %s", role, current, err, db.Name())
		}
		if strings.Contains(query, "application_name=gfcanary_app") && app != "gfcanary_app" {
			t.Errorf("role %q has application_name %q, want gfcanary_app", role, app)
		}
	}
}

// createCanary creates a database the test URL will name, and drops it when the
// test ends.
func createCanary(t *testing.T, admin string) string {
	t.Helper()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	canary := "gfcanary_" + hex.EncodeToString(suffix)
	ident := pgx.Identifier{canary}.Sanitize()
	adminExec(t, admin, "CREATE DATABASE "+ident)
	t.Cleanup(func() { adminExec(t, admin, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)") })
	return canary
}

// state is what the helper's init SQL and migrations would change in a database.
type state struct {
	DatabaseACL, PublicACL string
	ServiceSchemas, Tables int
}

func snapshot(t *testing.T, admin, database string) state {
	t.Helper()
	ctx := context.Background()
	conn := connect(t, admin, database)
	defer func() { _ = conn.Close(ctx) }()
	var s state
	err := conn.QueryRow(ctx, `SELECT
		(SELECT coalesce(datacl::text, '') FROM pg_database WHERE datname = current_database()),
		(SELECT coalesce(nspacl::text, '') FROM pg_namespace WHERE nspname = 'public'),
		(SELECT count(*) FROM pg_namespace WHERE nspname IN ('access', 'accounting')),
		(SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema'))`,
	).Scan(&s.DatabaseACL, &s.PublicACL, &s.ServiceSchemas, &s.Tables)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func snapshots(t *testing.T, admin string, databases []string) map[string]state {
	t.Helper()
	states := make(map[string]state, len(databases))
	for _, database := range databases {
		states[database] = snapshot(t, admin, database)
	}
	return states
}

// wantRedirected checks that pgx, given the parameterised URL as it is, reaches one
// of the canaries, so the case really exercises the helper's rewrite.
func wantRedirected(t *testing.T, rawURL string, canaries []string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var current string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&current); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(canaries, current)
	if i < 0 {
		t.Fatalf("the URL as given reaches %q, want one of the canaries %v", current, canaries)
	}
	t.Logf("the URL as given reaches canary %d of %d", i+1, len(canaries))
}

func wantDropped(t *testing.T, admin, name string) {
	t.Helper()
	if name == "" {
		t.Fatal("the helper did not create a database")
	}
	ctx := context.Background()
	conn := connect(t, admin, "")
	defer func() { _ = conn.Close(ctx) }()
	var left int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", name).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("database %s is still there after cleanup", name)
	}
}

// connect opens admin's connection, moved to database when it is not empty.
func connect(t *testing.T, admin, database string) *pgx.Conn {
	t.Helper()
	config, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	if database != "" {
		config.Database = database
	}
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func adminExec(t *testing.T, admin, sql string) {
	t.Helper()
	conn := connect(t, admin, "")
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

func withQuery(t *testing.T, admin, query string) string {
	t.Helper()
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	if query != "" && u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += query
	return u.String()
}
