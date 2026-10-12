package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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
		{"openai-responses", "https://api.example.com", "", false},
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
