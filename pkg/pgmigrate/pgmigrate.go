// Package pgmigrate applies a service's SQL migrations to its own Postgres schema.
//
// A service embeds its files as services/<service>/migrations/NNNN_name.sql and calls
// Apply at startup, connected as its own role. Each file runs once, in version order,
// with the search path set to the service's schema, so files name their tables
// without a schema. Applied versions are recorded in <schema>.schema_migrations.
package pgmigrate

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
)

var fileName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// Beginner starts a transaction; *pgxpool.Pool and *pgx.Conn both satisfy it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Migration is one SQL file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Load reads the migrations at the root of fsys in version order. Any other file is
// an error, so a misnamed migration is never silently skipped.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var migrations []Migration
	for _, entry := range entries {
		match := fileName.FindStringSubmatch(entry.Name())
		if entry.IsDir() || match == nil {
			return nil, fmt.Errorf("pgmigrate: %q is not a migration; name files NNNN_name.sql", entry.Name())
		}
		body, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, err
		}
		version, _ := strconv.Atoi(match[1])
		migrations = append(migrations, Migration{Version: version, Name: entry.Name(), SQL: string(body)})
	}
	slices.SortFunc(migrations, func(a, b Migration) int { return a.Version - b.Version })
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version == migrations[i-1].Version {
			return nil, fmt.Errorf("pgmigrate: %s and %s share version %d",
				migrations[i-1].Name, migrations[i].Name, migrations[i].Version)
		}
	}
	return migrations, nil
}

// Apply runs the migrations in fsys that schema has not recorded yet and returns how
// many it ran. Everything happens in one transaction under a lock on the schema, so
// replicas starting together apply each file once, and a failed file leaves nothing
// behind. A recorded version whose file name changed is an error.
func Apply(ctx context.Context, db Beginner, schema string, fsys fs.FS) (int, error) {
	migrations, err := Load(fsys)
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit

	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('pgmigrate:' || $1))", schema); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+ident); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, fmt.Errorf("pgmigrate: version table in schema %s: %w", schema, err)
	}
	rows, err := tx.Query(ctx, "SELECT version, name FROM schema_migrations")
	if err != nil {
		return 0, err
	}
	applied := map[int]string{}
	var version int
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&version, &name}, func() error {
		applied[version] = name
		return nil
	}); err != nil {
		return 0, err
	}

	ran := 0
	for _, m := range migrations {
		if recorded, ok := applied[m.Version]; ok {
			if recorded != m.Name {
				return 0, fmt.Errorf("pgmigrate: version %d was applied as %s, now named %s", m.Version, recorded, m.Name)
			}
			continue
		}
		// No arguments, so pgx uses the simple protocol and a file may hold several statements.
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return 0, fmt.Errorf("pgmigrate: %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
			return 0, err
		}
		ran++
	}
	return ran, tx.Commit(ctx)
}
