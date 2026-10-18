// Package config reads the access service's settings from the environment.
//
// Functions take a getenv function, normally os.Getenv, so tests need no process
// environment. Errors name the variables at fault and never include a value.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Names of the variables the access service reads.
const (
	EnvDatabaseURL   = "DATABASE_URL"
	EnvKeyPepper     = "ACCESS_KEY_PEPPER"
	EnvInternalToken = "ACCESS_INTERNAL_TOKEN"
	EnvSeedSecret    = "ACCESS_SEED_SECRET"
	EnvMockBaseURL   = "MOCK_UPSTREAM_BASE_URL"
	EnvOllamaBaseURL = "OLLAMA_BASE_URL"
	EnvOllamaModel   = "OLLAMA_MODEL"
	// EnvDatabaseMaxConns is optional.
	EnvDatabaseMaxConns = "DATABASE_MAX_CONNS"
)

// Service is what the service needs to serve requests.
type Service struct {
	DatabaseURL string
	// DatabaseMaxConns caps the database pool. 0, when DATABASE_MAX_CONNS is unset,
	// means pgx's default.
	DatabaseMaxConns int32
	KeyPepper        []byte
	InternalToken    string
}

// LoadService reads the service settings. It fails when DATABASE_URL,
// ACCESS_KEY_PEPPER or ACCESS_INTERNAL_TOKEN is empty, or when DATABASE_MAX_CONNS
// is set but not a whole number of at least 1.
func LoadService(getenv func(string) string) (Service, error) {
	if err := require(getenv, EnvDatabaseURL, EnvKeyPepper, EnvInternalToken); err != nil {
		return Service{}, err
	}
	maxConns, err := databaseMaxConns(getenv(EnvDatabaseMaxConns))
	if err != nil {
		return Service{}, err
	}
	return Service{
		DatabaseURL:      getenv(EnvDatabaseURL),
		DatabaseMaxConns: maxConns,
		KeyPepper:        []byte(getenv(EnvKeyPepper)),
		InternalToken:    getenv(EnvInternalToken),
	}, nil
}

// databaseMaxConns reads a whole number of connections. An empty value means 0,
// pgx's default.
func databaseMaxConns(value string) (int32, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a whole number from 1 to %d", EnvDatabaseMaxConns, math.MaxInt32)
	}
	return int32(n), nil
}

// Seed is what the seed command needs. The channel settings are checked by the
// seed itself, against the channel rules.
type Seed struct {
	DatabaseURL   string
	KeyPepper     []byte
	SeedSecret    []byte
	MockBaseURL   string
	OllamaBaseURL string
	OllamaModel   string
}

// LoadSeed reads the seed settings. It fails when DATABASE_URL, ACCESS_KEY_PEPPER or
// ACCESS_SEED_SECRET is empty.
func LoadSeed(getenv func(string) string) (Seed, error) {
	if err := require(getenv, EnvDatabaseURL, EnvKeyPepper, EnvSeedSecret); err != nil {
		return Seed{}, err
	}
	return Seed{
		DatabaseURL:   getenv(EnvDatabaseURL),
		KeyPepper:     []byte(getenv(EnvKeyPepper)),
		SeedSecret:    []byte(getenv(EnvSeedSecret)),
		MockBaseURL:   getenv(EnvMockBaseURL),
		OllamaBaseURL: getenv(EnvOllamaBaseURL),
		OllamaModel:   getenv(EnvOllamaModel),
	}, nil
}

// require returns an error naming every one of names that is unset or empty.
func require(getenv func(string) string, names ...string) error {
	var missing []string
	for _, name := range names {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("set the required environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}
