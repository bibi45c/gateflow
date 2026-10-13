package pgtest_test

import (
	"context"
	"errors"
	"net/url"
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
