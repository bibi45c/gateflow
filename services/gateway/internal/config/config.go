// Package config reads the gateway's settings from the environment.
package config

import (
	"fmt"
	"strings"
)

// The environment variables the service reads.
const (
	EnvAccessURL             = "ACCESS_URL"
	EnvMeteringURL           = "METERING_URL"
	EnvAccessInternalToken   = "ACCESS_INTERNAL_TOKEN"
	EnvMeteringInternalToken = "METERING_INTERNAL_TOKEN"
)

// Config holds the settings the service needs to serve.
type Config struct {
	// AccessURL is where access listens, scheme://host[:port].
	AccessURL string
	// MeteringURL is where metering listens, scheme://host[:port].
	MeteringURL string
	// AccessInternalToken is the bearer token this service sends to access.
	AccessInternalToken string
	// MeteringInternalToken is the bearer token this service sends to metering.
	MeteringInternalToken string
}

// Load reads the settings through getenv, such as os.Getenv. It fails when any of
// them is empty, naming every missing variable but never a value.
func Load(getenv func(string) string) (Config, error) {
	var missing []string
	for _, key := range []string{EnvAccessURL, EnvMeteringURL, EnvAccessInternalToken, EnvMeteringInternalToken} {
		if getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: set %s", strings.Join(missing, ", "))
	}
	return Config{
		AccessURL:             getenv(EnvAccessURL),
		MeteringURL:           getenv(EnvMeteringURL),
		AccessInternalToken:   getenv(EnvAccessInternalToken),
		MeteringInternalToken: getenv(EnvMeteringInternalToken),
	}, nil
}
