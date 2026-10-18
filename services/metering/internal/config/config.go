// Package config reads the metering service's settings from the environment.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The environment variables the service reads.
const (
	EnvDatabaseURL           = "DATABASE_URL"
	EnvAccessURL             = "ACCESS_URL"
	EnvAccessInternalToken   = "ACCESS_INTERNAL_TOKEN"
	EnvMeteringInternalToken = "METERING_INTERNAL_TOKEN"
	// EnvDatabaseMaxConns is optional.
	EnvDatabaseMaxConns = "DATABASE_MAX_CONNS"
)

// Config holds the settings the service needs to serve.
type Config struct {
	// DatabaseURL connects as the metering role.
	DatabaseURL string
	// DatabaseMaxConns caps the database pool. 0, when DATABASE_MAX_CONNS is unset,
	// means pgx's default.
	DatabaseMaxConns int32
	// AccessURL is the base URL of the access service, which holds the price versions.
	AccessURL string
	// AccessInternalToken is the bearer token this service sends to access.
	AccessInternalToken string
	// MeteringInternalToken is the bearer token this service accepts from callers.
	MeteringInternalToken string
}

// Load reads the settings through getenv, such as os.Getenv. It fails when any of
// the required ones is empty, naming every missing variable but never a value, and
// when DATABASE_MAX_CONNS is set but invalid, naming it.
func Load(getenv func(string) string) (Config, error) {
	if err := require(getenv, EnvDatabaseURL, EnvAccessURL, EnvAccessInternalToken, EnvMeteringInternalToken); err != nil {
		return Config{}, err
	}
	maxConns, err := databaseMaxConns(getenv(EnvDatabaseMaxConns))
	if err != nil {
		return Config{}, err
	}
	return Config{
		DatabaseURL:           getenv(EnvDatabaseURL),
		DatabaseMaxConns:      maxConns,
		AccessURL:             getenv(EnvAccessURL),
		AccessInternalToken:   getenv(EnvAccessInternalToken),
		MeteringInternalToken: getenv(EnvMeteringInternalToken),
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
		return 0, fmt.Errorf("config: %s must be a whole number from 1 to %d", EnvDatabaseMaxConns, math.MaxInt32)
	}
	return int32(n), nil
}

// LoadSeed reads the only setting the seed command needs, the database URL, and
// fails when it is empty.
func LoadSeed(getenv func(string) string) (string, error) {
	if err := require(getenv, EnvDatabaseURL); err != nil {
		return "", err
	}
	return getenv(EnvDatabaseURL), nil
}

// require returns an error naming each of keys whose value is empty, in order.
func require(getenv func(string) string, keys ...string) error {
	var missing []string
	for _, key := range keys {
		if getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: set %s", strings.Join(missing, ", "))
	}
	return nil
}
