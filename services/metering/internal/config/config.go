// Package config reads the metering service's settings from the environment.
package config

import (
	"fmt"
	"strings"
)

// The environment variables the service reads.
const (
	EnvDatabaseURL           = "DATABASE_URL"
	EnvAccessURL             = "ACCESS_URL"
	EnvAccessInternalToken   = "ACCESS_INTERNAL_TOKEN"
	EnvMeteringInternalToken = "METERING_INTERNAL_TOKEN"
)

// Config holds the settings the service needs to serve.
type Config struct {
	// DatabaseURL connects as the metering role.
	DatabaseURL string
	// AccessURL is the base URL of the access service, which holds the price versions.
	AccessURL string
	// AccessInternalToken is the bearer token this service sends to access.
	AccessInternalToken string
	// MeteringInternalToken is the bearer token this service accepts from callers.
	MeteringInternalToken string
}

// Load reads the settings through getenv, such as os.Getenv. It fails when any of
// them is empty, naming every missing variable but never a value.
func Load(getenv func(string) string) (Config, error) {
	if err := require(getenv, EnvDatabaseURL, EnvAccessURL, EnvAccessInternalToken, EnvMeteringInternalToken); err != nil {
		return Config{}, err
	}
	return Config{
		DatabaseURL:           getenv(EnvDatabaseURL),
		AccessURL:             getenv(EnvAccessURL),
		AccessInternalToken:   getenv(EnvAccessInternalToken),
		MeteringInternalToken: getenv(EnvMeteringInternalToken),
	}, nil
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
