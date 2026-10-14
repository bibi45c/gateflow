package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/pgurl"
	"github.com/bibi45c/gateflow/services/access/internal/config"
)

// envRunMain, set to 1, makes runMain call main instead of starting a process.
const envRunMain = "GATEFLOW_TEST_RUN_MAIN"

// fakePassword stands in for the database password; no output may show it.
const fakePassword = "REVIEW_FAKE_SECRET"

// badDatabaseURL puts a space before "=", a libpq form whose password pgx's own error
// shows, and has a port that does not parse, so nothing connects.
const badDatabaseURL = "host=127.0.0.1 user=review password =" + fakePassword + " port=invalid"

func TestStartDoesNotShowTheDatabasePassword(t *testing.T) {
	code, stdout, stderr := runMain(t, map[string]string{
		config.EnvDatabaseURL:   badDatabaseURL,
		config.EnvKeyPepper:     "test-pepper",
		config.EnvInternalToken: "test-internal-token",
	})
	checkInvalidURL(t, code, stdout, stderr)
}

func TestSeedDoesNotShowTheDatabasePassword(t *testing.T) {
	code, stdout, stderr := runMain(t, map[string]string{
		config.EnvDatabaseURL:   badDatabaseURL,
		config.EnvKeyPepper:     "test-pepper",
		config.EnvSeedSecret:    "test-seed-secret",
		config.EnvMockBaseURL:   "http://mock-upstream:8090",
		config.EnvOllamaBaseURL: "http://host.docker.internal:11434",
		config.EnvOllamaModel:   "qwen2.5-coder:7b",
	}, "seed")
	checkInvalidURL(t, code, stdout, stderr)
}

// checkInvalidURL fails the test unless the command exited with 1 after saying that
// the database URL is not valid, and showed the password nowhere.
func checkInvalidURL(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if want := "access: connect to the database: " + pgurl.ErrInvalidURL.Error(); !strings.Contains(stderr, want) {
		t.Errorf("stderr %q does not say %q", stderr, want)
	}
	if strings.Contains(stdout, fakePassword) || strings.Contains(stderr, fakePassword) {
		t.Error("the output shows the database password")
	}
}

// runMain runs `access args...` with env added to the environment and returns its
// exit code and output. It starts this test binary again with only the calling test
// and envRunMain set, so that there the same call runs main, which exits the process.
func runMain(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	if os.Getenv(envRunMain) == "1" {
		os.Args = append([]string{"access"}, args...)
		main()
		os.Exit(0) // as the command does when main returns
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), envRunMain+"=1")
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("the command did not exit within a minute")
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}
