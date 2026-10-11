// Package config reads process settings from the environment.
package config

import "os"

// Env returns the value of the environment variable key, or fallback when it is
// unset or empty.
func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
