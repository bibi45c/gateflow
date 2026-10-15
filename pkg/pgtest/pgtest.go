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
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
)

// EnvURL names the variable that holds the superuser URL.
const EnvURL = "GATEFLOW_TEST_POSTGRES_URL"

const timeout = 30 * time.Second

// DB is one test's database.
type DB struct {
	name string
	url  string
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
	target, err := targetURL(admin, name)
	if err != nil {
		t.Fatalf("%s: %v", EnvURL, err)
	}

	exec(t, admin, "CREATE DATABASE "+ident)
	// Cleanups run last-in first-out, so pools opened later are closed before this.
	t.Cleanup(func() { exec(t, admin, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)") })

	db := &DB{name: name, url: target}
	db.initialize(t, string(initSQL))
	return db
}

// Name is the name of this test's database.
func (db *DB) Name() string { return db.name }

// URL is the superuser URL of this test's database.
func (db *DB) URL() string { return db.url }

// targetURL points admin at the database name. pgx lets the dbname and database
// query parameters override the path, so every such parameter is dropped, in any
// letter case and however often it appears. Other parameters keep their order.
func targetURL(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		return "", err
	}
	// pgx reads anything else as keyword/value settings, where the path means nothing.
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("want a postgres:// or postgresql:// URL, got scheme %q", u.Scheme)
	}
	var kept []string
	for _, pair := range strings.Split(u.RawQuery, "&") {
		if pair == "" {
			continue
		}
		key, _, _ := strings.Cut(pair, "=")
		if decoded, err := url.QueryUnescape(key); err == nil && selectsDatabase(decoded) {
			continue
		}
		kept = append(kept, pair)
	}
	u.RawQuery = strings.Join(kept, "&")
	u.Path = "/" + name
	u.RawPath = ""
	return u.String(), nil
}

// selectsDatabase reports whether a query parameter names the database to use.
func selectsDatabase(key string) bool {
	return strings.EqualFold(key, "dbname") || strings.EqualFold(key, "database")
}

// initialize runs the init SQL in the test's database, after making sure the
// connection really landed there.
func (db *DB) initialize(t testing.TB, initSQL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, db.url)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := initDatabase(ctx, conn, db.name, initSQL); err != nil {
		t.Fatal(err)
	}
}

// querier is the part of *pgx.Conn that initDatabase uses.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// initDatabase runs initSQL on conn only when conn's current database is want, so a
// misdirected connection never changes another database.
func initDatabase(ctx context.Context, conn querier, want, initSQL string) error {
	var got string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&got); err != nil {
		return fmt.Errorf("check current database: %w", err)
	}
	if got != want {
		return fmt.Errorf("connected to database %q, want %q; init SQL not run", got, want)
	}
	_, err := conn.Exec(ctx, initSQL)
	return err
}

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
