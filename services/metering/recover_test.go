package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/config"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
)

type fakeRecoverStore struct {
	stale     []store.Stale
	results   map[string]error
	recovered []string
}

func (f *fakeRecoverStore) StaleReservations(context.Context, time.Duration, int) ([]store.Stale, error) {
	return f.stale, nil
}

func (f *fakeRecoverStore) Recover(_ context.Context, id string) (api.Settled, error) {
	f.recovered = append(f.recovered, id)
	if err := f.results[id]; err != nil {
		return api.Settled{}, err
	}
	return api.Settled{Charged: 300, LedgerEntryID: "led_" + id}, nil
}

func threeStale() *fakeRecoverStore {
	now := time.Now()
	return &fakeRecoverStore{stale: []store.Stale{
		{ID: "res_a", TenantID: "tenant-01", MaxCost: 300, CreatedAt: now.Add(-time.Hour)},
		{ID: "res_b", TenantID: "tenant-02", MaxCost: 300, CreatedAt: now.Add(-time.Hour)},
		{ID: "res_c", TenantID: "tenant-03", MaxCost: 300, CreatedAt: now.Add(-time.Hour)},
	}}
}

func TestRunRecoverCountsEachResult(t *testing.T) {
	rs := threeStale()
	rs.results = map[string]error{"res_b": api.ErrConflict, "res_c": errors.New("database down")}
	var out bytes.Buffer
	err := runRecover(context.Background(), rs, slog.New(slog.NewJSONHandler(io.Discard, nil)), &out,
		30*time.Minute, 100, false)

	if err == nil || !strings.Contains(err.Error(), "1 of 3 recoveries failed") {
		t.Errorf("error: got %v, want one failed recovery reported", err)
	}
	if want := "metering recover: 3 held longer than 30m0s, recovered 1, skipped 1, failed 1\n"; out.String() != want {
		t.Errorf("summary: got %q, want %q", out.String(), want)
	}
	if len(rs.recovered) != 3 {
		t.Errorf("recover calls: got %v, want all three tried", rs.recovered)
	}
}

func TestRunRecoverDryRunChangesNothing(t *testing.T) {
	rs := threeStale()
	var out, logs bytes.Buffer
	if err := runRecover(context.Background(), rs, slog.New(slog.NewJSONHandler(&logs, nil)), &out,
		time.Hour, 100, true); err != nil {
		t.Fatal(err)
	}
	if len(rs.recovered) != 0 {
		t.Errorf("dry run called Recover for %v", rs.recovered)
	}
	if want := "metering recover: 3 held longer than 1h0m0s, would recover 3, skipped 0, failed 0\n"; out.String() != want {
		t.Errorf("summary: got %q, want %q", out.String(), want)
	}
	if n := strings.Count(logs.String(), `"msg":"would recover"`); n != 3 {
		t.Errorf("log lines: got %d, want 3 in %s", n, logs.String())
	}
}

func TestRecoverRejectsBadFlagsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{{"--older-than", "0s"}, {"--limit", "0"}, {"--limit", "10001"}, {"extra"}} {
		// No DATABASE_URL is set, so reaching the database step would fail differently.
		if err := recoverHeld(args, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "recover:") {
			t.Errorf("%v: got %v, want a recover argument error", args, err)
		}
	}
}

func TestRecoverDoesNotShowTheDatabasePassword(t *testing.T) {
	code, stdout, stderr := runMain(t, map[string]string{config.EnvDatabaseURL: badDatabaseURL}, "recover")
	checkInvalidURL(t, code, stdout, stderr)
}
