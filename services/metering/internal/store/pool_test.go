package store_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/metering/internal/config"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
)

// poolEnv returns a getenv with the required service settings and
// DATABASE_MAX_CONNS set to maxConns, or unset when it is empty.
func poolEnv(maxConns string) func(string) string {
	env := map[string]string{
		config.EnvDatabaseURL:           "postgres://metering:db-password@localhost:5432/gateflow",
		config.EnvAccessURL:             "http://access.example:8081",
		config.EnvAccessInternalToken:   "access-token-value",
		config.EnvMeteringInternalToken: "metering-token-value",
		config.EnvDatabaseMaxConns:      maxConns,
	}
	return func(key string) string { return env[key] }
}

func poolConfigMaxConns(t *testing.T, maxConns string) int32 {
	t.Helper()
	cfg, err := config.Load(poolEnv(maxConns))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.PoolConfig(cfg.DatabaseURL, cfg.DatabaseMaxConns)
	if err != nil {
		t.Fatal(err)
	}
	return pool.MaxConns
}

func TestPoolConfigTakesMaxConnsFromTheEnvironment(t *testing.T) {
	if got := poolConfigMaxConns(t, "20"); got != 20 {
		t.Errorf("%s=20: MaxConns = %d, want 20", config.EnvDatabaseMaxConns, got)
	}
}

func TestPoolConfigKeepsThePgxDefault(t *testing.T) {
	// pgx's default: max(4, runtime.NumCPU()).
	want := max(int32(4), int32(runtime.NumCPU()))
	if got := poolConfigMaxConns(t, ""); got != want {
		t.Errorf("%s unset: MaxConns = %d, want %d", config.EnvDatabaseMaxConns, got, want)
	}
}

func TestPoolConfigErrorHidesTheURL(t *testing.T) {
	url := "host=127.0.0.1 user=review password =db-password port=invalid"
	_, err := store.PoolConfig(url, 20)
	if err == nil {
		t.Fatal("invalid URL: no error")
	}
	if strings.Contains(err.Error(), "db-password") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error %q shows the URL", err)
	}
}
