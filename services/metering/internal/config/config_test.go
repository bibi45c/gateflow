package config_test

import (
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/metering/internal/config"
)

var complete = map[string]string{
	config.EnvDatabaseURL:           "postgres://metering:db-password@localhost/gateflow",
	config.EnvAccessURL:             "http://access.example:8081",
	config.EnvAccessInternalToken:   "access-token-value",
	config.EnvMeteringInternalToken: "metering-token-value",
}

// env returns a getenv over complete, without the variable named by empty.
func env(empty string) func(string) string {
	return func(key string) string {
		if key == empty {
			return ""
		}
		return complete[key]
	}
}

func TestLoadReadsEverySetting(t *testing.T) {
	got, err := config.Load(env(""))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		DatabaseURL:           complete[config.EnvDatabaseURL],
		AccessURL:             complete[config.EnvAccessURL],
		AccessInternalToken:   complete[config.EnvAccessInternalToken],
		MeteringInternalToken: complete[config.EnvMeteringInternalToken],
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadNamesTheEmptyVariable(t *testing.T) {
	for name := range complete {
		_, err := config.Load(env(name))
		if err == nil {
			t.Errorf("%s empty: no error", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s empty: error %q does not name it", name, err)
		}
		for _, value := range complete {
			if strings.Contains(err.Error(), value) {
				t.Errorf("%s empty: error %q shows a value", name, err)
			}
		}
	}
}

func TestLoadNamesEveryEmptyVariable(t *testing.T) {
	_, err := config.Load(func(string) string { return "" })
	if err == nil {
		t.Fatal("nothing set: no error")
	}
	for name := range complete {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

// withMaxConns returns a getenv over complete with DATABASE_MAX_CONNS set to value.
func withMaxConns(value string) func(string) string {
	return func(key string) string {
		if key == config.EnvDatabaseMaxConns {
			return value
		}
		return complete[key]
	}
}

func TestLoadReadsDatabaseMaxConns(t *testing.T) {
	got, err := config.Load(env(""))
	if err != nil || got.DatabaseMaxConns != 0 {
		t.Errorf("%s unset: got %d, %v, want 0", config.EnvDatabaseMaxConns, got.DatabaseMaxConns, err)
	}
	got, err = config.Load(withMaxConns("20"))
	if err != nil || got.DatabaseMaxConns != 20 {
		t.Errorf("%s=20: got %d, %v, want 20", config.EnvDatabaseMaxConns, got.DatabaseMaxConns, err)
	}
}

func TestLoadRejectsInvalidDatabaseMaxConns(t *testing.T) {
	for _, value := range []string{"0", "-1", "abc", "2147483648", "20 "} {
		_, err := config.Load(withMaxConns(value))
		if err == nil {
			t.Errorf("%s=%q: no error", config.EnvDatabaseMaxConns, value)
			continue
		}
		if !strings.Contains(err.Error(), config.EnvDatabaseMaxConns) {
			t.Errorf("%s=%q: error %q does not name it", config.EnvDatabaseMaxConns, value, err)
		}
		for _, secret := range []string{complete[config.EnvDatabaseURL], "db-password",
			complete[config.EnvAccessInternalToken], complete[config.EnvMeteringInternalToken]} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s=%q: error %q shows %q", config.EnvDatabaseMaxConns, value, err, secret)
			}
		}
	}
}

func TestLoadSeedNeedsOnlyTheDatabaseURL(t *testing.T) {
	onlyURL := func(key string) string {
		if key == config.EnvDatabaseURL {
			return complete[key]
		}
		return ""
	}
	if url, err := config.LoadSeed(onlyURL); err != nil || url != complete[config.EnvDatabaseURL] {
		t.Errorf("got %q, %v", url, err)
	}
	_, err := config.LoadSeed(env(config.EnvDatabaseURL))
	if err == nil || !strings.Contains(err.Error(), config.EnvDatabaseURL) {
		t.Errorf("DATABASE_URL empty: error %v, want one naming it", err)
	}
}
