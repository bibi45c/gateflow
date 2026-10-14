// Package config reads process settings from the environment.
package config

import (
	"fmt"
	"os"
	"time"
)

// Env returns the value of the environment variable key, or fallback when it is
// unset or empty.
func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Duration reads the environment variable key as a non-negative Go duration such
// as "30s", or returns fallback when it is unset or empty. Its errors name the
// variable but not the value, in case a secret was set there by mistake.
func Duration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: not a duration such as 30s", key)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s: must not be negative", key)
	}
	return duration, nil
}
