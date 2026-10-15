package config_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/config"
)

var complete = map[string]string{
	config.EnvAccessURL:             "http://access:8081",
	config.EnvMeteringURL:           "http://metering:8082",
	config.EnvAccessInternalToken:   "access-token-value",
	config.EnvMeteringInternalToken: "metering-token-value",
}

// env returns a getenv over complete, without the variable named by empty.
func env(empty string) func(string) string {
	return func(key string) string {
		if key == empty {
			return ""
		}
		return complete[key]
	}
}

func TestLoadReadsEverySetting(t *testing.T) {
	got, err := config.Load(env(""))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		AccessURL:             complete[config.EnvAccessURL],
		MeteringURL:           complete[config.EnvMeteringURL],
		AccessInternalToken:   complete[config.EnvAccessInternalToken],
		MeteringInternalToken: complete[config.EnvMeteringInternalToken],
		// The optional settings are unset here, so they hold their defaults.
		ModelAliases: config.DefaultModelAliases,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// with returns a getenv over complete and the optional settings in extra.
func with(extra map[string]string) func(string) string {
	return func(key string) string {
		if value, ok := extra[key]; ok {
			return value
		}
		return complete[key]
	}
}

func TestLoadReadsTheOptionalSettings(t *testing.T) {
	got, err := config.Load(with(map[string]string{
		config.EnvModelAliases:         " mock-text, ,qwen3:8b ,org/model-1.5_x ",
		config.EnvUpstreamMaxIdleConns: "64",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mock-text", "qwen3:8b", "org/model-1.5_x"}; !slices.Equal(got.ModelAliases, want) {
		t.Errorf("aliases = %q, want %q", got.ModelAliases, want)
	}
	if got.UpstreamMaxIdleConns != 64 {
		t.Errorf("idle connections = %d, want 64", got.UpstreamMaxIdleConns)
	}
}

// The default aliases cover every mock scenario and the Ollama models, within the
// helper's bounds, and Load hands out a copy.
func TestDefaultModelAliases(t *testing.T) {
	for _, model := range []string{"mock-text", "mock-tool", "mock-coding", "mock-429", "mock-500", "mock-drop",
		"mock-error-event", "mock-load", "qwen3:8b"} {
		if !slices.Contains(config.DefaultModelAliases, model) {
			t.Errorf("the default aliases lack %s", model)
		}
	}
	got, err := config.Load(env(""))
	if err != nil {
		t.Fatal(err)
	}
	got.ModelAliases[0] = "changed"
	if config.DefaultModelAliases[0] == "changed" {
		t.Error("Load returned the default list itself")
	}
}

func TestLoadRefusesInvalidOptionalSettings(t *testing.T) {
	tooMany := strings.Repeat("m,", 17)
	tests := []struct{ name, value string }{
		{config.EnvModelAliases, " , "},
		{config.EnvModelAliases, "mock text"},
		{config.EnvModelAliases, "mock-text,model?secret=1"},
		{config.EnvModelAliases, strings.Repeat("a", 65)},
		{config.EnvModelAliases, tooMany},
		{config.EnvUpstreamMaxIdleConns, "0"},
		{config.EnvUpstreamMaxIdleConns, "-1"},
		{config.EnvUpstreamMaxIdleConns, "10001"},
		{config.EnvUpstreamMaxIdleConns, "many"},
	}
	for _, tt := range tests {
		_, err := config.Load(with(map[string]string{tt.name: tt.value}))
		if err == nil {
			t.Errorf("%s=%q: no error", tt.name, tt.value)
			continue
		}
		if !strings.Contains(err.Error(), tt.name) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s=%q: error %q should name the variable but not the value", tt.name, tt.value, err)
		}
	}
}

func TestLoadNamesTheEmptyVariable(t *testing.T) {
	for name := range complete {
		_, err := config.Load(env(name))
		if err == nil {
			t.Errorf("%s empty: no error", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s empty: error %q does not name it", name, err)
		}
		for _, value := range complete {
			if strings.Contains(err.Error(), value) {
				t.Errorf("%s empty: error %q shows a value", name, err)
			}
		}
	}
}

func TestLoadNamesEveryEmptyVariable(t *testing.T) {
	_, err := config.Load(func(string) string { return "" })
	if err == nil {
		t.Fatal("nothing set: no error")
	}
	for name := range complete {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}
