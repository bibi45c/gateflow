// Package pgtest gives each integration test its own Postgres database, set up with
// the same roles and schemas as the compose stack (deploy/compose/postgres-init.sql).
//
// Tests run only when GATEFLOW_TEST_POSTGRES_URL holds a superuser URL; otherwise
// they skip. The URL's own database is only used to create and drop test databases,
// and DATABASE_URL is never read, so a developer's running database is untouched.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
)

// EnvURL names the variable that holds the superuser URL.
const EnvURL = "GATEFLOW_TEST_POSTGRES_URL"

const timeout = 30 * time.Second

// DB is one test's database.
type DB struct {
	url string
}

// New creates a uniquely named database, runs the init SQL in it and drops it when
// the test ends. It skips the test when GATEFLOW_TEST_POSTGRES_URL is unset.
func New(t testing.TB) *DB {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skipf("set %s to a Postgres superuser URL to run this test", EnvURL)
	}
	initSQL, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", "postgres-init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "gftest_" + hex.EncodeToString(suffix)
	ident := pgx.Identifier{name}.Sanitize()

	exec(t, admin, "CREATE DATABASE "+ident)
	// Cleanups run last-in first-out, so pools opened later are closed before this.
	t.Cleanup(func() { exec(t, admin, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)") })

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s: %v", EnvURL, err)
	}
	u.Path = "/" + name
	db := &DB{url: u.String()}
	exec(t, db.url, string(initSQL))
	return db
}

// URL is the superuser URL of this test's database.
func (db *DB) URL() string { return db.url }

// Pool connects to the database as role, such as "access" or "metering", or as the
// superuser when role is empty, and closes the pool when the test ends.
func (db *DB) Pool(t testing.TB, role string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(db.url)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		// The superuser takes on the role for the whole session, so every privilege
		// check is the role's own, without needing the role's password.
		config.ConnConfig.RuntimeParams["role"] = role
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		t.Fatalf("connect as %q: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Migrated returns a pool connected as role to a new database whose schema has the
// migrations in fsys applied, as the service would do at startup.
func Migrated(t testing.TB, role, schema string, fsys fs.FS) *pgxpool.Pool {
	t.Helper()
	pool := New(t).Pool(t, role)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := pgmigrate.Apply(ctx, pool, schema, fsys); err != nil {
		t.Fatal(err)
	}
	return pool
}

func exec(t testing.TB, connString, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

// repoRoot finds the directory holding go.mod, starting from the test's directory.
func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
