package ratelimit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/services/gateway/internal/ratelimit"
)

// EnvTestRedisURL names a disposable Redis for these tests. Without it they skip;
// the CI integration job sets it and fails when a test says so.
const EnvTestRedisURL = "GATEFLOW_TEST_REDIS_URL"

var tenantSeq atomic.Int64

// newLimiter returns a limiter on the test Redis and a tenant id no other test uses.
func newLimiter(t *testing.T, leaseTTL time.Duration) (*ratelimit.Limiter, string) {
	t.Helper()
	url := os.Getenv(EnvTestRedisURL)
	if url == "" {
		t.Skip("set " + EnvTestRedisURL + " to run the Redis tests")
	}
	l, err := ratelimit.New(ratelimit.Config{URL: url, LeaseTTL: leaseTTL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), tenantSeq.Add(1))
}

func TestUnlimitedNeverTouchesRedis(t *testing.T) {
	// The address refuses connections, so any Redis call would fail.
	l, err := ratelimit.New(ratelimit.Config{URL: "redis://127.0.0.1:1/0", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	release, refusal, err := l.Acquire(context.Background(), "tenant-01", "req_1", ratelimit.Limits{})
	if release == nil || refusal != nil || err != nil {
		t.Fatalf("unlimited: got %v %v, want admitted", refusal, err)
	}
	release() // a release with no lease does nothing
}

func TestUnreachableRedisIsUnavailable(t *testing.T) {
	l, err := ratelimit.New(ratelimit.Config{URL: "redis://127.0.0.1:1/0", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = l.Acquire(context.Background(), "tenant-01", "req_1", ratelimit.Limits{RPM: 10})
	if !errors.Is(err, ratelimit.ErrUnavailable) {
		t.Errorf("got %v, want ErrUnavailable", err)
	}
}

func TestBadURLDoesNotShowIt(t *testing.T) {
	_, err := ratelimit.New(ratelimit.Config{URL: "http://:secret-password@x"})
	if err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Errorf("got %v, want an error without the URL", err)
	}
}

func TestRPMRefusesOverTheLimitWithRetryAfter(t *testing.T) {
	l, tenant := newLimiter(t, time.Minute)
	ctx := context.Background()
	for i := range 3 {
		if _, refusal, err := l.Acquire(ctx, tenant, fmt.Sprintf("req_%d", i), ratelimit.Limits{RPM: 3}); refusal != nil || err != nil {
			t.Fatalf("call %d: got %v %v, want admitted", i, refusal, err)
		}
	}
	_, refusal, err := l.Acquire(ctx, tenant, "req_3", ratelimit.Limits{RPM: 3})
	if err != nil || refusal == nil || refusal.Reason != ratelimit.ReasonRPM {
		t.Fatalf("fourth call: got %v %v, want an rpm refusal", refusal, err)
	}
	if s := refusal.RetryAfterSeconds(); s < 1 || s > 60 {
		t.Errorf("retry after %d s, want 1 to 60", s)
	}
	// Another tenant is not affected.
	if _, refusal, err := l.Acquire(ctx, tenant+"-b", "req_0", ratelimit.Limits{RPM: 3}); refusal != nil || err != nil {
		t.Errorf("other tenant: got %v %v, want admitted", refusal, err)
	}
}

func TestConcurrencyLeaseIsReleasedAndExpires(t *testing.T) {
	l, tenant := newLimiter(t, 1500*time.Millisecond)
	ctx := context.Background()
	lim := ratelimit.Limits{MaxConcurrent: 2}
	first, _, err := l.Acquire(ctx, tenant, "req_1", lim)
	if err != nil || first == nil {
		t.Fatalf("first: %v", err)
	}
	_, _, _ = l.Acquire(ctx, tenant, "req_2", lim)
	if _, refusal, _ := l.Acquire(ctx, tenant, "req_3", lim); refusal == nil || refusal.Reason != ratelimit.ReasonConcurrency {
		t.Fatalf("third while two run: got %v, want a concurrency refusal", refusal)
	}
	first()
	first() // twice is harmless
	if release, refusal, err := l.Acquire(ctx, tenant, "req_4", lim); refusal != nil || err != nil || release == nil {
		t.Fatalf("after a release: got %v %v, want admitted", refusal, err)
	}
	// req_2 and req_4 are never released, as if their gateway died; they expire.
	time.Sleep(1700 * time.Millisecond)
	if _, refusal, err := l.Acquire(ctx, tenant, "req_5", lim); refusal != nil || err != nil {
		t.Errorf("after the lease TTL: got %v %v, want admitted", refusal, err)
	}
}

func TestConcurrentAcquiresNeverExceedTheLimit(t *testing.T) {
	l, tenant := newLimiter(t, time.Minute)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, refusal, err := l.Acquire(context.Background(), tenant, fmt.Sprintf("req_%d", i),
				ratelimit.Limits{RPM: 20, MaxConcurrent: 10})
			if err == nil && refusal == nil {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := admitted.Load(); n != 10 {
		t.Errorf("admitted %d of 50 at once, want exactly 10", n)
	}
}

func TestARefusedCallUsesNoRateOrLease(t *testing.T) {
	l, tenant := newLimiter(t, time.Minute)
	ctx := context.Background()
	lim := ratelimit.Limits{RPM: 2, MaxConcurrent: 1}
	held, _, _ := l.Acquire(ctx, tenant, "req_1", lim)
	// Refused for concurrency, so it must not count toward the rate.
	if _, refusal, _ := l.Acquire(ctx, tenant, "req_2", lim); refusal == nil || refusal.Reason != ratelimit.ReasonConcurrency {
		t.Fatalf("got %v, want a concurrency refusal", refusal)
	}
	held()
	if _, refusal, err := l.Acquire(ctx, tenant, "req_3", lim); refusal != nil || err != nil {
		t.Errorf("second admitted call: got %v %v, want admitted", refusal, err)
	}
}
