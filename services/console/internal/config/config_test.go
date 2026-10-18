package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{
		EnvAccessURL: "http://access:8081", EnvAccessInternalToken: "a",
		EnvMeteringURL: "http://metering:8082/", EnvMeteringInternalToken: "m",
		EnvGatewayURL: "http://gateway:8080", EnvSessionSecret: "0123456789abcdef",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeteringURL != "http://metering:8082" || cfg.PublicGatewayURL != "http://gateway:8080" ||
		cfg.GatewayJob != "gateflow-gateway" || cfg.Location.String() != "UTC" || cfg.PrometheusURL != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadRefusesBadSettingsWithoutValues(t *testing.T) {
	cases := map[string]func(map[string]string){
		EnvSessionSecret: func(m map[string]string) { m[EnvSessionSecret] = "short-secret" },
		EnvTimeZone:      func(m map[string]string) { m[EnvTimeZone] = "Mars/Base" },
		EnvPrometheusURL: func(m map[string]string) { m[EnvPrometheusURL] = "ftp://secret-host" },
		EnvAccessURL:     func(m map[string]string) { delete(m, EnvAccessURL) },
	}
	for name, change := range cases {
		m := base()
		change(m)
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "secret-host") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	m := base()
	m[EnvTimeZone] = "Asia/Singapore"
	if cfg, err := Load(env(m)); err != nil || cfg.Location.String() != "Asia/Singapore" {
		t.Errorf("zone: %v %v", cfg.Location, err)
	}
}
