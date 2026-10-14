package config

import (
	"strings"
	"testing"
	"time"
)

func TestEnvUsesValueOrFallback(t *testing.T) {
	t.Setenv("GATEFLOW_TEST_SET", "value")
	t.Setenv("GATEFLOW_TEST_EMPTY", "")
	if got := Env("GATEFLOW_TEST_SET", "fallback"); got != "value" {
		t.Errorf("set variable: got %q", got)
	}
	if got := Env("GATEFLOW_TEST_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("empty variable: got %q", got)
	}
	if got := Env("GATEFLOW_TEST_UNSET", "fallback"); got != "fallback" {
		t.Errorf("unset variable: got %q", got)
	}
}

func TestDurationParsesValueOrUsesFallback(t *testing.T) {
	t.Setenv("GATEFLOW_TEST_DURATION", "1m30s")
	t.Setenv("GATEFLOW_TEST_EMPTY", "")
	if got, err := Duration("GATEFLOW_TEST_DURATION", time.Second); err != nil || got != 90*time.Second {
		t.Errorf("set variable: got %v, %v", got, err)
	}
	if got, err := Duration("GATEFLOW_TEST_EMPTY", time.Second); err != nil || got != time.Second {
		t.Errorf("empty variable: got %v, %v", got, err)
	}
	if got, err := Duration("GATEFLOW_TEST_UNSET", time.Second); err != nil || got != time.Second {
		t.Errorf("unset variable: got %v, %v", got, err)
	}
}

func TestDurationRejectsInvalidValuesWithoutEchoingThem(t *testing.T) {
	for _, value := range []string{"secret-value", "45", "-5s"} {
		t.Setenv("GATEFLOW_TEST_DURATION", value)
		_, err := Duration("GATEFLOW_TEST_DURATION", time.Second)
		if err == nil {
			t.Errorf("%q: no error", value)
			continue
		}
		if !strings.Contains(err.Error(), "GATEFLOW_TEST_DURATION") {
			t.Errorf("%q: error does not name the variable: %v", value, err)
		}
		if strings.Contains(err.Error(), value) {
			t.Errorf("%q: error contains the value: %v", value, err)
		}
	}
}
