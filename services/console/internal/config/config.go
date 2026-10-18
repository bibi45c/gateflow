// Package config reads the console's settings from the environment. Errors name the
// variables at fault and never include a value.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zone database
)

// Names of the variables the console reads.
const (
	EnvAccessURL             = "ACCESS_URL"
	EnvAccessInternalToken   = "ACCESS_INTERNAL_TOKEN"
	EnvMeteringURL           = "METERING_URL"
	EnvMeteringInternalToken = "METERING_INTERNAL_TOKEN"
	EnvGatewayURL            = "GATEWAY_URL"
	EnvSessionSecret         = "CONSOLE_SESSION_SECRET"
	// Optional ones.
	EnvPublicGatewayURL = "CONSOLE_PUBLIC_GATEWAY_URL"
	EnvPrometheusURL    = "PROMETHEUS_URL"
	EnvGrafanaURL       = "GRAFANA_URL"
	EnvGatewayJob       = "CONSOLE_GATEWAY_JOB"
	EnvTimeZone         = "CONSOLE_TIME_ZONE"
	EnvSecureCookie     = "CONSOLE_SECURE_COOKIE"
)

// Config is what the console needs.
type Config struct {
	AccessURL             string
	AccessInternalToken   string
	MeteringURL           string
	MeteringInternalToken string
	// GatewayURL is where the console sends playground calls, inside the network.
	GatewayURL string
	// PublicGatewayURL is the base URL shown to users; GatewayURL when unset.
	PublicGatewayURL string
	SessionSecret    []byte
	// PrometheusURL is empty when the dashboard has no data source.
	PrometheusURL string
	GrafanaURL    string
	// GatewayJob is the Prometheus job of the gateway's scrapes.
	GatewayJob string
	// Location is where "today" starts.
	Location     *time.Location
	SecureCookie bool
}

// MinSecretBytes is the shortest session secret accepted.
const MinSecretBytes = 16

// Load reads the settings.
func Load(getenv func(string) string) (Config, error) {
	var missing []string
	for _, name := range []string{EnvAccessURL, EnvAccessInternalToken, EnvMeteringURL,
		EnvMeteringInternalToken, EnvGatewayURL, EnvSessionSecret} {
		if getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("set the required environment variables: %s", strings.Join(missing, ", "))
	}
	if len(getenv(EnvSessionSecret)) < MinSecretBytes {
		return Config{}, fmt.Errorf("%s must be at least %d bytes", EnvSessionSecret, MinSecretBytes)
	}
	cfg := Config{
		AccessURL:             strings.TrimRight(getenv(EnvAccessURL), "/"),
		AccessInternalToken:   getenv(EnvAccessInternalToken),
		MeteringURL:           strings.TrimRight(getenv(EnvMeteringURL), "/"),
		MeteringInternalToken: getenv(EnvMeteringInternalToken),
		GatewayURL:            strings.TrimRight(getenv(EnvGatewayURL), "/"),
		PublicGatewayURL:      strings.TrimRight(getenv(EnvPublicGatewayURL), "/"),
		SessionSecret:         []byte(getenv(EnvSessionSecret)),
		PrometheusURL:         strings.TrimRight(getenv(EnvPrometheusURL), "/"),
		GrafanaURL:            getenv(EnvGrafanaURL),
		GatewayJob:            getenv(EnvGatewayJob),
		SecureCookie:          getenv(EnvSecureCookie) == "true",
	}
	if cfg.PublicGatewayURL == "" {
		cfg.PublicGatewayURL = cfg.GatewayURL
	}
	if cfg.GatewayJob == "" {
		cfg.GatewayJob = "gateflow-gateway"
	}
	for name, value := range map[string]string{EnvAccessURL: cfg.AccessURL, EnvMeteringURL: cfg.MeteringURL,
		EnvGatewayURL: cfg.GatewayURL, EnvPublicGatewayURL: cfg.PublicGatewayURL,
		EnvPrometheusURL: cfg.PrometheusURL, EnvGrafanaURL: cfg.GrafanaURL} {
		if value == "" {
			continue
		}
		if u, err := url.Parse(value); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	zone := getenv(EnvTimeZone)
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return Config{}, fmt.Errorf("%s must name a time zone", EnvTimeZone)
	}
	cfg.Location = location
	return cfg, nil
}
