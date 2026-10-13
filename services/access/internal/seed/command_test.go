package seed_test

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
)

func commandEnv(databaseURL string) map[string]string {
	return map[string]string{
		config.EnvDatabaseURL:   databaseURL,
		config.EnvKeyPepper:     "test-pepper",
		config.EnvSeedSecret:    "test-seed-secret",
		config.EnvMockBaseURL:   "http://mock-upstream:8090",
		config.EnvOllamaBaseURL: "http://host.docker.internal:11434",
		config.EnvOllamaModel:   "qwen2.5-coder:7b",
	}
}

func run(env map[string]string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	err = seed.Command(context.Background(), args, func(key string) string { return env[key] }, &out, &errOut)
	return out.String(), errOut.String(), err
}

// These fail before the command connects, so the database URL is never used.
func TestCommandRefusesBadInput(t *testing.T) {
	unreachable := "postgres://access@127.0.0.1:1/gateflow"
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"negative count", commandEnv(unreachable), []string{"--tenants", "-1"}, "--tenants"},
		{"count not a number", commandEnv(unreachable), []string{"--tenants", "ten"}, "tenants"},
		{"extra argument", commandEnv(unreachable), []string{"now"}, "now"},
		{"no seed secret", map[string]string{config.EnvDatabaseURL: unreachable, config.EnvKeyPepper: "p"}, nil,
			config.EnvSeedSecret},
		{"bad mock URL", func() map[string]string {
			env := commandEnv(unreachable)
			env[config.EnvMockBaseURL] = "http://mock-upstream:8090/v1"
			return env
		}(), nil, config.EnvMockBaseURL},
	}
	for _, c := range cases {
		stdout, _, err := run(c.env, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one mentioning %s", c.name, err, c.want)
		}
		if stdout != "" {
			t.Errorf("%s: wrote %q to stdout", c.name, stdout)
		}
	}
}

func TestCommandMigratesSeedsAndPrintsKeys(t *testing.T) {
	db := pgtest.New(t)
	// Log in as the access role, as the service does, so the migrations' tables are its own.
	u, err := url.Parse(db.URL())
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("role", "access")
	u.RawQuery = query.Encode()
	env := commandEnv(u.String())

	for range 2 {
		stdout, stderr, err := run(env, "--tenants", "3")
		if err != nil {
			t.Fatal(err)
		}
		var want strings.Builder
		for _, tenant := range []string{"tenant-00", "tenant-01", "tenant-02", "tenant-03"} {
			key := seed.DeriveKey([]byte(env[config.EnvSeedSecret]), tenant)
			want.WriteString(tenant + " " + key + "\n")
			if strings.Contains(stderr, key) {
				t.Error("a key was written to stderr")
			}
		}
		if stdout != want.String() {
			t.Error("stdout is not one tenant_id key line per tenant, tenant-00 to tenant-03")
		}
	}
	var owned, tenants int
	if err := db.Pool(t, "access").QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM pg_tables WHERE schemaname = 'access' AND tableowner = 'access'),
		(SELECT count(*) FROM access.tenants)`).Scan(&owned, &tenants); err != nil {
		t.Fatal(err)
	}
	// The six tables and the migrations' version table.
	if owned != 7 || tenants != 4 {
		t.Errorf("tables owned by access = %d, tenants = %d; want 7 and 4", owned, tenants)
	}
}
