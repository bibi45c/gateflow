package admission_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/services/gateway/internal/ratelimit"
)

// fakeLimiter answers Acquire as told and records what it was asked.
type fakeLimiter struct {
	mu       sync.Mutex
	refusal  *ratelimit.Refusal
	err      error
	tenants  []string
	limits   []ratelimit.Limits
	released int
}

func (f *fakeLimiter) Acquire(_ context.Context, tenant, _ string, lim ratelimit.Limits) (func(), *ratelimit.Refusal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants, f.limits = append(f.tenants, tenant), append(f.limits, lim)
	if f.err != nil || f.refusal != nil {
		return nil, f.refusal, f.err
	}
	return func() { f.mu.Lock(); f.released++; f.mu.Unlock() }, nil, nil
}

func (f *fakeLimiter) releases() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

func errorOf(t *testing.T, resp *http.Response) (typ, message string) {
	t.Helper()
	var body struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("error body %s: %v", data, err)
	}
	return body.Error.Type, body.Error.Message
}

func TestOverTheRateLimitIsRefusedBeforeReserve(t *testing.T) {
	limiter := &fakeLimiter{refusal: &ratelimit.Refusal{Reason: ratelimit.ReasonRPM, RetryAfter: 3200 * time.Millisecond}}
	h := start(t, setup{limiter: limiter})
	resp := h.post(context.Background(), nil)

	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "4" {
		t.Errorf("status %d, retry-after %q; want 429 and 4", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if typ, msg := errorOf(t, resp); typ != "rate_limit_error" || msg != "The gateway key's tenant is over its plan's request rate limit." {
		t.Errorf("error %q %q", typ, msg)
	}
	h.finish()
	if n, m := len(h.reserve.requests()), len(h.upstream.requests()); n != 0 || m != 0 {
		t.Errorf("reserve called %d times and upstream %d; want neither", n, m)
	}
	if len(limiter.tenants) != 1 || limiter.tenants[0] != "tenant-01" ||
		limiter.limits[0] != (ratelimit.Limits{RPM: 60, MaxConcurrent: 5}) {
		t.Errorf("limiter asked for %v with %v; want tenant-01 with the plan from verify", limiter.tenants, limiter.limits)
	}
}

func TestOverTheConcurrencyLimitNamesIt(t *testing.T) {
	limiter := &fakeLimiter{refusal: &ratelimit.Refusal{Reason: ratelimit.ReasonConcurrency, RetryAfter: time.Second}}
	h := start(t, setup{limiter: limiter})
	resp := h.post(context.Background(), nil)
	if typ, msg := errorOf(t, resp); resp.StatusCode != http.StatusTooManyRequests || typ != "rate_limit_error" ||
		msg != "The gateway key's tenant is over its plan's concurrent request limit." {
		t.Errorf("got %d %q %q", resp.StatusCode, typ, msg)
	}
}

func TestUncheckableLimitsRefuseWith503(t *testing.T) {
	limiter := &fakeLimiter{err: errors.New("redis down")}
	h := start(t, setup{limiter: limiter})
	resp := h.post(context.Background(), nil)
	if typ, _ := errorOf(t, resp); resp.StatusCode != http.StatusServiceUnavailable || typ != "api_error" {
		t.Errorf("got %d %q, want 503 api_error", resp.StatusCode, typ)
	}
	h.finish()
	if n := len(h.reserve.requests()); n != 0 {
		t.Errorf("reserve called %d times, want none", n)
	}
}

func TestAdmittedCallReleasesItsLeaseOnce(t *testing.T) {
	limiter := &fakeLimiter{}
	h := start(t, setup{limiter: limiter})
	resp := h.post(context.Background(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, _ = io.ReadAll(resp.Body)
	h.finish()
	if n := limiter.releases(); n != 1 {
		t.Errorf("released %d times, want once", n)
	}
	if n := len(h.settle.requests()); n != 1 {
		t.Errorf("settled %d times, want once", n)
	}
}

func TestLeaseIsReleasedWhenReserveFails(t *testing.T) {
	limiter := &fakeLimiter{}
	h := start(t, setup{limiter: limiter, reserve: internalError(http.StatusConflict, "insufficient_balance")})
	resp := h.post(context.Background(), nil)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", resp.StatusCode)
	}
	h.finish()
	if n := limiter.releases(); n != 1 {
		t.Errorf("released %d times, want once", n)
	}
}
