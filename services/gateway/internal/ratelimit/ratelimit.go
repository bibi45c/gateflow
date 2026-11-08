// Package ratelimit enforces each tenant's request rate and concurrency limits
// across every gateway replica, with the shared state in Redis (issue #157).
//
// One Lua script checks and records both limits atomically, on the Redis server's
// clock, so replicas never disagree on time. The rate limit counts admitted calls
// in a sliding window of one minute. The concurrency limit keeps one lease per call
// in flight; a lease ends when the call reports, and expires after LeaseTTL if its
// gateway dies first.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Window is the span of the rate limit: a tenant may start Limits.RPM calls in any
// sliding Window.
const Window = time.Minute

// Defaults of Config.
const (
	DefaultLeaseTTL = 10 * time.Minute
	DefaultTimeout  = 200 * time.Millisecond
)

// ErrUnavailable means the limits could not be checked; the caller refuses the call.
var ErrUnavailable = errors.New("ratelimit: store unavailable")

// Limits are a tenant's limits; zero means no limit.
type Limits struct {
	RPM           int
	MaxConcurrent int
}

// Unlimited reports whether neither limit applies, so no check is needed.
func (l Limits) Unlimited() bool { return l.RPM <= 0 && l.MaxConcurrent <= 0 }

// Reason names the limit a refused call is over.
type Reason string

// The reasons of a Refusal.
const (
	ReasonRPM         Reason = "rpm"
	ReasonConcurrency Reason = "concurrency"
)

// Refusal says which limit refused a call and when trying again can help.
type Refusal struct {
	Reason     Reason
	RetryAfter time.Duration
}

// Config holds what a Limiter needs.
type Config struct {
	// URL is the Redis URL, redis://[:password@]host:port[/db].
	URL string
	// LeaseTTL bounds a concurrency lease whose call never reports. Zero means
	// DefaultLeaseTTL.
	LeaseTTL time.Duration
	// Timeout bounds each Redis call. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Limiter checks limits in Redis. It is safe for concurrent use.
type Limiter struct {
	rdb      *redis.Client
	leaseTTL time.Duration
	timeout  time.Duration
}

// New returns a Limiter on the Redis at cfg.URL. It does not connect yet.
func New(cfg Config) (*Limiter, error) {
	opt, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, errors.New("ratelimit: the Redis URL is not valid") // the URL may hold a password
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	opt.DialTimeout, opt.ReadTimeout, opt.WriteTimeout = cfg.Timeout, cfg.Timeout, cfg.Timeout
	opt.MaxRetries = -1 // a refused call is cheaper than a slow one
	return &Limiter{rdb: redis.NewClient(opt), leaseTTL: cfg.LeaseTTL, timeout: cfg.Timeout}, nil
}

// Close closes the connections to Redis.
func (l *Limiter) Close() error { return l.rdb.Close() }

// acquire checks the rate limit (KEYS[1], a sorted set of call starts) and then the
// concurrency limit (KEYS[2], a sorted set of lease expiries), and records the call
// in each only when both admit it. It returns {1, "", 0} or {0, reason, retry ms}.
var acquire = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local window, rpm, conc, ttl, member = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), ARGV[5]
if rpm > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)
  if redis.call('ZCARD', KEYS[1]) >= rpm then
    local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
    return {0, 'rpm', math.max(1, tonumber(oldest[2]) + window - now)}
  end
end
if conc > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
  if redis.call('ZCARD', KEYS[2]) >= conc then
    return {0, 'concurrency', 1000}
  end
  redis.call('ZADD', KEYS[2], now + ttl, member)
  redis.call('PEXPIRE', KEYS[2], ttl)
end
if rpm > 0 then
  redis.call('ZADD', KEYS[1], now, member)
  redis.call('PEXPIRE', KEYS[1], window)
end
return {1, '', 0}
`)

func keys(tenant string) (rpm, concurrency string) {
	// The braces keep both keys of a tenant in one slot of a Redis cluster.
	return "gateflow:rl:{" + tenant + "}:rpm", "gateflow:rl:{" + tenant + "}:concurrent"
}

// Acquire admits or refuses a call of tenant, identified by requestID, under lim.
// An admitted call gets a release function to call once when it ends: it frees the
// call's concurrency lease, and does nothing when there is none. When Redis cannot
// answer in time it returns ErrUnavailable. An unlimited tenant never touches Redis.
func (l *Limiter) Acquire(ctx context.Context, tenant, requestID string, lim Limits) (release func(), refusal *Refusal, err error) {
	if lim.Unlimited() {
		return noRelease, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	rpmKey, concKey := keys(tenant)
	reply, err := acquire.Run(ctx, l.rdb, []string{rpmKey, concKey},
		Window.Milliseconds(), max(lim.RPM, 0), max(lim.MaxConcurrent, 0), l.leaseTTL.Milliseconds(), requestID).Slice()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	allowed, reason, retryMS, err := parseReply(reply)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !allowed {
		return nil, &Refusal{Reason: reason, RetryAfter: time.Duration(retryMS) * time.Millisecond}, nil
	}
	if lim.MaxConcurrent <= 0 {
		return noRelease, nil, nil
	}
	var once sync.Once
	return func() { once.Do(func() { l.release(concKey, requestID) }) }, nil, nil
}

func noRelease() {}

// release frees a concurrency lease. It uses a fresh context, because the call's own
// may already be done. If Redis cannot be reached, the lease expires after the lease
// TTL instead.
func (l *Limiter) release(key, member string) {
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	_ = l.rdb.ZRem(ctx, key, member).Err()
}

func parseReply(reply []any) (allowed bool, reason Reason, retryMS int64, err error) {
	if len(reply) != 3 {
		return false, "", 0, fmt.Errorf("unexpected reply %v", reply)
	}
	flag, ok1 := reply[0].(int64)
	text, ok2 := reply[1].(string)
	retry, ok3 := reply[2].(int64)
	if !ok1 || !ok2 || !ok3 {
		return false, "", 0, fmt.Errorf("unexpected reply %v", reply)
	}
	return flag == 1, Reason(text), retry, nil
}

// RetryAfterSeconds rounds a refusal's wait up to whole seconds, at least 1, for a
// retry-after header.
func (r *Refusal) RetryAfterSeconds() int {
	return max(1, int(math.Ceil(r.RetryAfter.Seconds())))
}
