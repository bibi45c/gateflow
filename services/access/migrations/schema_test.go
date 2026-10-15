package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgmigrate"
	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/channel"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

const (
	checkViolation = "23514"
	raisedError    = "P0001"
)

func migrated(t *testing.T) *pgxpool.Pool {
	return pgtest.Migrated(t, "access", "access", migrations.FS)
}

// code returns the SQLSTATE of err, or "" when it is not a Postgres error.
func code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func exec(pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}

func TestPriceVersionsCheckRange(t *testing.T) {
	pool := migrated(t)
	insert := "INSERT INTO access.price_versions (id, input_micro_per_token, output_micro_per_token) VALUES ($1, $2, $3)"
	for i, prices := range [][2]int64{{0, 0}, {1000000, 1000000}, {3, 15}} {
		if err := exec(pool, insert, fmt.Sprintf("pv-ok-%d", i), prices[0], prices[1]); err != nil {
			t.Errorf("prices %v refused: %v", prices, err)
		}
	}
	for i, prices := range [][2]int64{{-1, 0}, {0, -1}, {1000001, 0}, {0, 1000001}} {
		if err := exec(pool, insert, fmt.Sprintf("pv-bad-%d", i), prices[0], prices[1]); code(err) != checkViolation {
			t.Errorf("prices %v: err %v, want a check violation", prices, err)
		}
	}
}

func TestPriceVersionsAreImmutable(t *testing.T) {
	pool := migrated(t)
	if err := exec(pool, "INSERT INTO access.price_versions VALUES ('pv-1', 3, 15)"); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"UPDATE access.price_versions SET input_micro_per_token = 4 WHERE id = 'pv-1'",
		"UPDATE access.price_versions SET id = 'pv-2'",
		"DELETE FROM access.price_versions WHERE id = 'pv-1'",
		"DELETE FROM access.price_versions",
	} {
		if err := exec(pool, sql); code(err) != raisedError {
			t.Errorf("%s: err %v, want the trigger to refuse it", sql, err)
		}
	}
	var input, output int64
	if err := pool.QueryRow(context.Background(), `SELECT input_micro_per_token, output_micro_per_token
		FROM access.price_versions WHERE id = 'pv-1'`).Scan(&input, &output); err != nil {
		t.Fatal(err)
	}
	if input != 3 || output != 15 {
		t.Errorf("prices changed to %d and %d", input, output)
	}
}

// Every base URL is tried in the database and with channel.CheckBaseURL, which must
// agree with each other and with the expected verdict.
func TestChannelBaseURLRule(t *testing.T) {
	pool := migrated(t)
	cases := map[string]bool{
		"http://mock-upstream:8090":         true,
		"http://host.docker.internal:11434": true,
		"https://api.example.com":           true,
		"https://10.0.0.1:443":              true,
		"http://mock-upstream:8090/":        false,
		"http://mock-upstream/v1":           false,
		"https://api.example.com/":          false,
		"http://mock-upstream?x=1":          false,
		"http://mock-upstream#top":          false,
		"ftp://mock-upstream":               false,
		"HTTP://mock-upstream":              false,
		"mock-upstream:8090":                false,
		"http://user:pass@mock-upstream":    false,
		"http://mock-upstream:123456":       false,
		"http://mock-upstream:":             false,
		"http://":                           false,
		" http://mock-upstream":             false,
		"http://mock-upstream\n":            false,
		"":                                  false,
	}
	i := 0
	for baseURL, valid := range cases {
		i++
		err := exec(pool, "INSERT INTO access.channels (id, protocol, base_url) VALUES ($1, $2, $3)",
			fmt.Sprintf("ch-%d", i), channel.Protocol, baseURL)
		if valid && err != nil {
			t.Errorf("%q refused: %v", baseURL, err)
		}
		if !valid && code(err) != checkViolation {
			t.Errorf("%q: err %v, want a check violation", baseURL, err)
		}
		if goValid := channel.CheckBaseURL(baseURL) == nil; goValid != valid {
			t.Errorf("%q: channel.CheckBaseURL accepts it = %v, want %v", baseURL, goValid, valid)
		}
	}
}

func TestChannelCredentialAndProtocolRules(t *testing.T) {
	pool := migrated(t)
	insert := "INSERT INTO access.channels (id, protocol, base_url, credential_env) VALUES ($1, $2, $3, $4)"
	cases := []struct {
		protocol, baseURL, credentialEnv string
		valid                            bool
	}{
		{channel.Protocol, "https://api.example.com", "UPSTREAM_CREDENTIAL_ANTHROPIC_1", true},
		{channel.Protocol, "http://mock-upstream:8090", "", true},
		{channel.Protocol, "https://api.example.com", "", true},
		{channel.Protocol, "http://api.example.com", "UPSTREAM_CREDENTIAL_ANTHROPIC", false},
		{channel.Protocol, "https://api.example.com", "UPSTREAM_CREDENTIAL_", false},
		{channel.Protocol, "https://api.example.com", "upstream_credential_anthropic", false},
		{channel.Protocol, "https://api.example.com", "ANTHROPIC_API_KEY", false},
		{channel.Protocol, "https://api.example.com", "UPSTREAM_CREDENTIAL_A-B", false},
		{channel.Protocol, "https://api.example.com", "X_UPSTREAM_CREDENTIAL_A", false},
		{channel.ProtocolResponses, "https://api.example.com", "", true},
		{channel.ProtocolResponses, "http://mock-upstream:8090", "", true},
		{channel.ProtocolResponses, "https://api.example.com", "UPSTREAM_CREDENTIAL_OPENAI", true},
		{channel.ProtocolResponses, "http://api.example.com", "UPSTREAM_CREDENTIAL_OPENAI", false},
		{channel.ProtocolResponses, "https://api.example.com", "OPENAI_API_KEY", false},
		{"openai-chat", "https://api.example.com", "", false},
		{"", "https://api.example.com", "", false},
	}
	for i, c := range cases {
		err := exec(pool, insert, fmt.Sprintf("ch-%d", i), c.protocol, c.baseURL, c.credentialEnv)
		if c.valid && err != nil {
			t.Errorf("%+v refused: %v", c, err)
		}
		if !c.valid && code(err) != checkViolation {
			t.Errorf("%+v: err %v, want a check violation", c, err)
		}
	}
}

// Every protocol is tried in the database and with channel.CheckProtocol, which must
// agree with each other and with the expected verdict.
func TestChannelProtocolRule(t *testing.T) {
	pool := migrated(t)
	cases := map[string]bool{
		"anthropic-messages":  true,
		"openai-responses":    true,
		"":                    false,
		"Anthropic-Messages":  false,
		"OPENAI-RESPONSES":    false,
		"openai-responses ":   false,
		"openai-chat":         false,
		"anthropic-messages,": false,
	}
	i := 0
	for protocol, valid := range cases {
		i++
		err := exec(pool, "INSERT INTO access.channels (id, protocol, base_url) VALUES ($1, $2, 'http://mock:8090')",
			fmt.Sprintf("ch-%d", i), protocol)
		if valid && err != nil {
			t.Errorf("%q refused: %v", protocol, err)
		}
		if !valid && code(err) != checkViolation {
			t.Errorf("%q: err %v, want a check violation", protocol, err)
		}
		if goValid := channel.CheckProtocol(protocol) == nil; goValid != valid {
			t.Errorf("%q: channel.CheckProtocol accepts it = %v, want %v", protocol, goValid, valid)
		}
	}
}

// firstMigrationOnly holds what the access image before 0002 embeds: 0001 alone.
func firstMigrationOnly(t *testing.T) fstest.MapFS {
	t.Helper()
	sql, err := fs.ReadFile(migrations.FS, "0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{"0001_init.sql": {Data: sql}}
}

// channelRows lists every channels and channel_models row, with all its columns and
// the transaction that last wrote it.
func channelRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT c.xmin::text || ' ' || to_jsonb(c)::text FROM access.channels AS c
		UNION ALL SELECT m.xmin::text || ' ' || to_jsonb(m)::text FROM access.channel_models AS m
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 0002 runs on a schema that has only 0001, once, and changes no row; the previous
// image, which knows only 0001, still migrates without error afterwards.
func TestMigration0002AfterOnly0001(t *testing.T) {
	pool := pgtest.New(t).Pool(t, "access")
	ctx := context.Background()
	if ran, err := pgmigrate.Apply(ctx, pool, "access", firstMigrationOnly(t)); err != nil || ran != 1 {
		t.Fatalf("0001 alone: applied %d, err %v", ran, err)
	}
	responses := "INSERT INTO access.channels (id, protocol, base_url) VALUES ('responses', $1, 'http://mock:8090')"
	if err := exec(pool, responses, channel.ProtocolResponses); code(err) != checkViolation {
		t.Fatalf("openai-responses under 0001: err %v, want a check violation", err)
	}
	for _, sql := range []string{
		`INSERT INTO access.channels (id, protocol, base_url, credential_env) VALUES
			('mock', 'anthropic-messages', 'http://mock-upstream:8090', ''),
			('hosted', 'anthropic-messages', 'https://api.example.com', 'UPSTREAM_CREDENTIAL_HOSTED')`,
		"INSERT INTO access.channel_models (channel_id, model_pattern) VALUES ('mock', 'mock-*'), ('hosted', 'claude-sonnet-5')",
	} {
		if err := exec(pool, sql); err != nil {
			t.Fatal(err)
		}
	}
	before := channelRows(t, pool)

	for run, want := range []int{1, 0} {
		if ran, err := pgmigrate.Apply(ctx, pool, "access", migrations.FS); err != nil || ran != want {
			t.Fatalf("run %d with 0002: applied %d, want %d, err %v", run+1, ran, want, err)
		}
	}
	if after := channelRows(t, pool); !slices.Equal(after, before) {
		t.Errorf("rows after 0002 = %v, want them unchanged %v", after, before)
	}
	var versions []string
	rows, err := pool.Query(ctx, "SELECT version || ' ' || name FROM access.schema_migrations ORDER BY version")
	if err == nil {
		versions, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if want := []string{"1 0001_init.sql", "2 0002_channel_protocols.sql"}; err != nil || !slices.Equal(versions, want) {
		t.Errorf("recorded migrations = %v, err %v; want %v", versions, err, want)
	}
	if err := exec(pool, responses, channel.ProtocolResponses); err != nil {
		t.Errorf("openai-responses after 0002 refused: %v", err)
	}

	if ran, err := pgmigrate.Apply(ctx, pool, "access", firstMigrationOnly(t)); err != nil || ran != 0 {
		t.Errorf("the previous image's migrations after 0002: applied %d, err %v; want 0 and no error", ran, err)
	}
}

func TestChannelModelPatternRule(t *testing.T) {
	pool := migrated(t)
	if err := exec(pool, "INSERT INTO access.channels (id, protocol, base_url) VALUES ('mock', $1, 'http://mock:8090')",
		channel.Protocol); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"mock-*":           true,
		"qwen2.5-coder:7b": true,
		"claude-sonnet-5":  true,
		"":                 false,
		"*":                false,
		"mock-**":          false,
		"qwen*:7b":         false,
	}
	for pattern, valid := range cases {
		err := exec(pool, "INSERT INTO access.channel_models (channel_id, model_pattern) VALUES ('mock', $1)", pattern)
		if valid && err != nil {
			t.Errorf("%q refused: %v", pattern, err)
		}
		if !valid && code(err) != checkViolation {
			t.Errorf("%q: err %v, want a check violation", pattern, err)
		}
		if goValid := channel.CheckModelPattern(pattern) == nil; goValid != valid {
			t.Errorf("%q: channel.CheckModelPattern accepts it = %v, want %v", pattern, goValid, valid)
		}
	}
}
