package config

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

func lookup(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func serviceEnv() map[string]string {
	return map[string]string{
		EnvDatabaseURL:   "postgres://access:db-secret@localhost/gateflow",
		EnvKeyPepper:     "pepper-value",
		EnvInternalToken: "token-value",
	}
}

func seedEnv() map[string]string {
	return map[string]string{
		EnvDatabaseURL:   "postgres://access:db-secret@localhost/gateflow",
		EnvKeyPepper:     "pepper-value",
		EnvSeedSecret:    "seed-secret-value",
		EnvMockBaseURL:   "http://mock-upstream:8090",
		EnvOllamaBaseURL: "http://host.docker.internal:11434",
		EnvOllamaModel:   "qwen2.5-coder:7b",
	}
}

// checkMissing empties each required variable in turn and expects an error that
// names it and shows none of the values.
func checkMissing(t *testing.T, full map[string]string, required []string, load func(func(string) string) error) {
	t.Helper()
	for _, name := range required {
		env := maps.Clone(full)
		env[name] = ""
		err := load(lookup(env))
		if err == nil {
			t.Errorf("%s empty: no error", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s empty: error %q does not name it", name, err)
		}
		for key, value := range full {
			if strings.Contains(err.Error(), value) {
				t.Errorf("%s empty: error shows the value of %s", name, key)
			}
		}
	}
}

func TestLoadServiceNamesEachMissingVariable(t *testing.T) {
	required := []string{EnvDatabaseURL, EnvKeyPepper, EnvInternalToken}
	checkMissing(t, serviceEnv(), required, func(getenv func(string) string) error {
		_, err := LoadService(getenv)
		return err
	})
}

func TestLoadServiceNamesAllMissingVariables(t *testing.T) {
	_, err := LoadService(lookup(nil))
	if err == nil {
		t.Fatal("empty environment: no error")
	}
	for _, name := range []string{EnvDatabaseURL, EnvKeyPepper, EnvInternalToken} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

func TestLoadServiceReadsSettings(t *testing.T) {
	env := serviceEnv()
	got, err := LoadService(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	want := Service{
		DatabaseURL:   env[EnvDatabaseURL],
		KeyPepper:     []byte(env[EnvKeyPepper]),
		InternalToken: env[EnvInternalToken],
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("settings differ from the environment")
	}
}

func TestLoadServiceReadsDatabaseMaxConns(t *testing.T) {
	env := serviceEnv()
	got, err := LoadService(lookup(env))
	if err != nil || got.DatabaseMaxConns != 0 {
		t.Errorf("%s unset: got %d, %v, want 0", EnvDatabaseMaxConns, got.DatabaseMaxConns, err)
	}
	env[EnvDatabaseMaxConns] = "20"
	got, err = LoadService(lookup(env))
	if err != nil || got.DatabaseMaxConns != 20 {
		t.Errorf("%s=20: got %d, %v, want 20", EnvDatabaseMaxConns, got.DatabaseMaxConns, err)
	}
}

func TestLoadServiceRejectsInvalidDatabaseMaxConns(t *testing.T) {
	for _, value := range []string{"0", "-1", "abc", "2147483648", "20 "} {
		env := serviceEnv()
		env[EnvDatabaseMaxConns] = value
		_, err := LoadService(lookup(env))
		if err == nil {
			t.Errorf("%s=%q: no error", EnvDatabaseMaxConns, value)
			continue
		}
		if !strings.Contains(err.Error(), EnvDatabaseMaxConns) {
			t.Errorf("%s=%q: error %q does not name it", EnvDatabaseMaxConns, value, err)
		}
		for _, secret := range []string{env[EnvDatabaseURL], "db-secret", env[EnvKeyPepper], env[EnvInternalToken]} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s=%q: error %q shows %q", EnvDatabaseMaxConns, value, err, secret)
			}
		}
	}
}

func TestLoadSeedNamesEachMissingVariable(t *testing.T) {
	required := []string{EnvDatabaseURL, EnvKeyPepper, EnvSeedSecret}
	checkMissing(t, seedEnv(), required, func(getenv func(string) string) error {
		_, err := LoadSeed(getenv)
		return err
	})
}

func TestLoadSeedReadsSettings(t *testing.T) {
	env := seedEnv()
	got, err := LoadSeed(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	want := Seed{
		DatabaseURL:   env[EnvDatabaseURL],
		KeyPepper:     []byte(env[EnvKeyPepper]),
		SeedSecret:    []byte(env[EnvSeedSecret]),
		MockBaseURL:   env[EnvMockBaseURL],
		OllamaBaseURL: env[EnvOllamaBaseURL],
		OllamaModel:   env[EnvOllamaModel],
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("settings differ from the environment")
	}
}
