package accessclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// access is a stand-in for the access service that answers every request with one
// status and body, and counts the requests it saw.
func access(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64, *http.Header) {
	t.Helper()
	var calls atomic.Int64
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seen = r.Header.Clone()
		seen.Set("Path", r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &calls, &seen
}

const fixture = `{"id":"pv-1","input_micro_per_token":2,"output_micro_per_token":5}`

func TestPriceVersionFetchesOnceAndSendsTheInternalHeaders(t *testing.T) {
	server, calls, seen := access(t, http.StatusOK, fixture)
	client := New(server.URL+"/", "access-token")

	for range 3 {
		prices, err := client.PriceVersion(context.Background(), "pv-1", "req_1")
		if err != nil {
			t.Fatal(err)
		}
		if prices != (billing.Prices{Input: 2, Output: 5}) {
			t.Fatalf("prices: got %+v", prices)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("requests: got %d, want 1 because price versions never change", calls.Load())
	}
	if got := seen.Get("Authorization"); got != "Bearer access-token" {
		t.Errorf("authorization: got %q", got)
	}
	if got := seen.Get("X-Request-Id"); got != "req_1" {
		t.Errorf("request id: got %q", got)
	}
	if got := seen.Get("Path"); got != "/internal/v1/price-versions/pv-1" {
		t.Errorf("path: got %q", got)
	}
}

func TestPriceVersionEscapesTheID(t *testing.T) {
	server, _, seen := access(t, http.StatusOK, `{"id":"a/b c","input_micro_per_token":0,"output_micro_per_token":0}`)
	if _, err := New(server.URL, "t").PriceVersion(context.Background(), "a/b c", ""); err != nil {
		t.Fatal(err)
	}
	if got := seen.Get("Path"); got != "/internal/v1/price-versions/a%2Fb%20c" {
		t.Errorf("path: got %q", got)
	}
	if _, sent := (*seen)["X-Request-Id"]; sent {
		t.Error("an empty request id was sent")
	}
}

func TestPriceVersionSortsFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unknown price version", http.StatusNotFound, `{"error":{"code":"price_version_not_found"}}`, ErrRejected},
		{"token refused", http.StatusUnauthorized, `{"error":{"code":"unauthorized"}}`, ErrRejected},
		{"access database down", http.StatusServiceUnavailable, `{"error":{"code":"dependency_unavailable"}}`, ErrUnavailable},
		{"access failed", http.StatusInternalServerError, `{"error":{"code":"internal_error"}}`, ErrUnavailable},
		{"not json", http.StatusOK, `<html>`, ErrRejected},
		{"another id", http.StatusOK, `{"id":"pv-2","input_micro_per_token":2,"output_micro_per_token":5}`, ErrRejected},
		{"missing price", http.StatusOK, `{"id":"pv-1","input_micro_per_token":2}`, ErrRejected},
		{"fractional price", http.StatusOK, `{"id":"pv-1","input_micro_per_token":2.5,"output_micro_per_token":5}`, ErrRejected},
		{"negative price", http.StatusOK, `{"id":"pv-1","input_micro_per_token":-1,"output_micro_per_token":5}`, ErrRejected},
		{"price above the range", http.StatusOK, `{"id":"pv-1","input_micro_per_token":2,"output_micro_per_token":1000001}`, ErrRejected},
	} {
		server, calls, _ := access(t, tc.status, tc.body)
		client := New(server.URL, "t")
		for range 2 {
			if _, err := client.PriceVersion(context.Background(), "pv-1", ""); !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
			}
		}
		if calls.Load() != 2 {
			t.Errorf("%s: requests: got %d, want 2 because a failure is never remembered", tc.name, calls.Load())
		}
	}
}

func TestPriceVersionGivesUpWhenAccessIsSlowOrGone(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := New(slow.URL, "t").PriceVersion(context.Background(), "pv-1", "")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("slow access: got %v", err)
	}
	if waited := time.Since(start); waited < Timeout || waited > Timeout+2*time.Second {
		t.Errorf("slow access: waited %v, want about %v", waited, Timeout)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if _, err := New(gone.URL, "t").PriceVersion(context.Background(), "pv-1", ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreachable access: got %v", err)
	}
}
