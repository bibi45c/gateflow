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
