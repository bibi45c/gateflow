// Package config reads the gateway's settings from the environment.
package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The environment variables the service reads.
const (
	EnvAccessURL             = "ACCESS_URL"
	EnvMeteringURL           = "METERING_URL"
	EnvAccessInternalToken   = "ACCESS_INTERNAL_TOKEN"
	EnvMeteringInternalToken = "METERING_INTERNAL_TOKEN"
	// EnvModelAliases and EnvUpstreamMaxIdleConns are optional.
	EnvModelAliases         = "MODEL_ALIASES"
	EnvUpstreamMaxIdleConns = "UPSTREAM_MAX_IDLE_CONNS"
)

// DefaultModelAliases are the model names that request logs show as they are when
// MODEL_ALIASES is unset: the mock upstream's scenarios and the Ollama models that
// the compose stack and the client guides use. Any other model is logged as other.
var DefaultModelAliases = []string{
	"mock-text", "mock-tool", "mock-coding", "mock-429", "mock-500", "mock-drop",
	"mock-error-event", "mock-load", "qwen3:8b", "qwen2.5-coder:7b",
}

// Bounds of the optional settings. The alias bounds are those of the observability
// helper, so a list that loads is one the helper accepts.
const (
	maxModelAliases         = 16
	maxUpstreamMaxIdleConns = 10000
)

var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9._:/-]{1,64}$`)

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
	// ModelAliases are the model names that request logs show as they are, from the
	// comma-separated MODEL_ALIASES, or DefaultModelAliases when it is unset.
	ModelAliases []string
	// UpstreamMaxIdleConns is how many idle upstream connections the relay keeps, in
	// all and to each upstream host. 0, when UPSTREAM_MAX_IDLE_CONNS is unset, means
	// the relay's default.
	UpstreamMaxIdleConns int
}

// Load reads the settings through getenv, such as os.Getenv. It fails when any of
// the required ones is empty, naming every missing variable but never a value, and
// when an optional one is set but invalid, naming it.
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
	aliases, err := modelAliases(getenv(EnvModelAliases))
	if err != nil {
		return Config{}, err
	}
	idleConns, err := upstreamMaxIdleConns(getenv(EnvUpstreamMaxIdleConns))
	if err != nil {
		return Config{}, err
	}
	return Config{
		AccessURL:             getenv(EnvAccessURL),
		MeteringURL:           getenv(EnvMeteringURL),
		AccessInternalToken:   getenv(EnvAccessInternalToken),
		MeteringInternalToken: getenv(EnvMeteringInternalToken),
		ModelAliases:          aliases,
		UpstreamMaxIdleConns:  idleConns,
	}, nil
}

// modelAliases reads a comma-separated list, ignoring spaces around a name and empty
// items. An empty value means DefaultModelAliases.
func modelAliases(value string) ([]string, error) {
	if value == "" {
		return slices.Clone(DefaultModelAliases), nil
	}
	var aliases []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			aliases = append(aliases, item)
		}
	}
	valid := len(aliases) > 0 && len(aliases) <= maxModelAliases
	for _, alias := range aliases {
		valid = valid && aliasPattern.MatchString(alias)
	}
	if !valid {
		return nil, fmt.Errorf("config: %s must list 1 to %d model names, each 1 to 64 letters, digits or ._:/-",
			EnvModelAliases, maxModelAliases)
	}
	return aliases, nil
}

// upstreamMaxIdleConns reads a whole number of connections. An empty value means 0,
// the relay's default.
func upstreamMaxIdleConns(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > maxUpstreamMaxIdleConns {
		return 0, fmt.Errorf("config: %s must be a whole number from 1 to %d",
			EnvUpstreamMaxIdleConns, maxUpstreamMaxIdleConns)
	}
	return n, nil
}
