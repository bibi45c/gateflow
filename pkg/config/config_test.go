package config

import "testing"

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
