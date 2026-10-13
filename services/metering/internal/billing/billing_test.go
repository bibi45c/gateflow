package billing

import (
	"bytes"
	"math"
	"testing"
)

func count(n int64) *int64 { return &n }

func TestMaxCostMultipliesAndAdds(t *testing.T) {
	cost, ok := MaxCost(Prices{Input: 2, Output: 5}, Bounds{MaxInput: 100, MaxOutput: 20})
	if !ok || cost != 300 {
		t.Errorf("shared fixture: got %d, %v", cost, ok)
	}
	cost, ok = MaxCost(Prices{Input: 3, Output: 15}, Bounds{MaxInput: 1000, MaxOutput: 100})
	if !ok || cost != 4500 {
		t.Errorf("worked example: got %d, %v", cost, ok)
	}
	if cost, ok := MaxCost(Prices{}, Bounds{MaxInput: math.MaxInt64, MaxOutput: math.MaxInt64}); !ok || cost != 0 {
		t.Errorf("free model: got %d, %v", cost, ok)
	}
}

func TestMaxCostReportsOverflow(t *testing.T) {
	for name, bounds := range map[string]Bounds{
		"input product":  {MaxInput: math.MaxInt64, MaxOutput: 0},
		"output product": {MaxInput: 0, MaxOutput: math.MaxInt64/MaxPrice + 1},
		"sum":            {MaxInput: math.MaxInt64 / MaxPrice, MaxOutput: math.MaxInt64 / MaxPrice},
	} {
		if cost, ok := MaxCost(Prices{Input: MaxPrice, Output: MaxPrice}, bounds); ok {
			t.Errorf("%s: got %d, want overflow", name, cost)
		}
	}
	// The largest request the gateway sends stays far inside int64.
	if _, ok := MaxCost(Prices{Input: MaxPrice, Output: MaxPrice}, Bounds{MaxInput: 2 << 20, MaxOutput: 32768}); !ok {
		t.Error("gateway bounds overflowed")
	}
}

func TestChargeFollowsTheContractExamples(t *testing.T) {
	fixture, fixtureBounds := Prices{Input: 2, Output: 5}, Bounds{MaxInput: 100, MaxOutput: 20}
	worked, workedBounds := Prices{Input: 3, Output: 15}, Bounds{MaxInput: 1000, MaxOutput: 100}
	for _, tc := range []struct {
		name    string
		prices  Prices
		bounds  Bounds
		maxCost int64
		usage   Usage
		outcome Outcome
		want    int64
	}{
		{"fixture known", fixture, fixtureBounds, 300, Usage{count(60), count(10)}, Completed, 170},
		{"fixture known small", fixture, fixtureBounds, 300, Usage{count(20), count(4)}, ClientCancelled, 60},
		{"fixture output unknown", fixture, fixtureBounds, 300, Usage{count(20), nil}, Interrupted, 140},
		{"fixture both unknown", fixture, fixtureBounds, 300, Usage{}, Interrupted, 300},
		{"fixture upstream error", fixture, fixtureBounds, 300, Usage{count(0), count(0)}, UpstreamError, 0},
		{"worked completed", worked, workedBounds, 4500, Usage{count(800), count(50)}, Completed, 3150},
		{"worked above the bounds", worked, workedBounds, 4500, Usage{count(1200), count(100)}, Completed, 4500},
		{"worked output unknown", worked, workedBounds, 4500, Usage{count(800), nil}, Interrupted, 3900},
		{"worked cancelled unknown", worked, workedBounds, 4500, Usage{}, ClientCancelled, 4500},
		{"worked not dispatched", worked, workedBounds, 4500, Usage{count(0), count(0)}, NotDispatched, 0},
		{"input unknown only", fixture, fixtureBounds, 300, Usage{nil, count(4)}, Interrupted, 220},
		{"known zero output", fixture, fixtureBounds, 300, Usage{count(60), count(0)}, Completed, 120},
		{"counts ignored on upstream error", fixture, fixtureBounds, 300, Usage{count(60), count(10)}, UpstreamError, 0},
		{"overflow charges the reservation", fixture, fixtureBounds, 300, Usage{count(math.MaxInt64), count(1)}, Completed, 300},
	} {
		if got := Charge(tc.prices, tc.bounds, tc.maxCost, tc.usage, tc.outcome); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPricesAndOutcomesAreValidated(t *testing.T) {
	for _, p := range []Prices{{0, 0}, {MaxPrice, MaxPrice}, {3, 15}} {
		if !p.Valid() {
			t.Errorf("%+v: want valid", p)
		}
	}
	for _, p := range []Prices{{-1, 0}, {0, -1}, {MaxPrice + 1, 0}, {0, MaxPrice + 1}} {
		if p.Valid() {
			t.Errorf("%+v: want invalid", p)
		}
	}
	for _, o := range []Outcome{Completed, UpstreamError, ClientCancelled, Interrupted, NotDispatched} {
		if !o.Valid() {
			t.Errorf("%q: want valid", o)
		}
	}
	for _, o := range []Outcome{"", "Completed", "failed"} {
		if o.Valid() {
			t.Errorf("%q: want invalid", o)
		}
	}
}

func TestDigestsSeparateEveryComparedField(t *testing.T) {
	base := ReserveDigest("tenant-01", "pv-1", Bounds{MaxInput: 100, MaxOutput: 20})
	if len(base) != 32 {
		t.Fatalf("length: got %d", len(base))
	}
	if !bytes.Equal(base, ReserveDigest("tenant-01", "pv-1", Bounds{MaxInput: 100, MaxOutput: 20})) {
		t.Error("the same content gave different digests")
	}
	for name, other := range map[string][]byte{
		"tenant":         ReserveDigest("tenant-02", "pv-1", Bounds{MaxInput: 100, MaxOutput: 20}),
		"price version":  ReserveDigest("tenant-01", "pv-2", Bounds{MaxInput: 100, MaxOutput: 20}),
		"max input":      ReserveDigest("tenant-01", "pv-1", Bounds{MaxInput: 101, MaxOutput: 20}),
		"max output":     ReserveDigest("tenant-01", "pv-1", Bounds{MaxInput: 100, MaxOutput: 21}),
		"swapped maxima": ReserveDigest("tenant-01", "pv-1", Bounds{MaxInput: 20, MaxOutput: 100}),
		// A separator inside an id must not make two different requests look alike.
		"shifted ids": ReserveDigest(`tenant-01","pv-1`, "", Bounds{MaxInput: 100, MaxOutput: 20}),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("%s: digest did not change", name)
		}
	}
}

func TestSettleDigestKeepsUnknownApartFromZero(t *testing.T) {
	known := SettleDigest(Completed, Usage{count(0), count(0)})
	for name, other := range map[string][]byte{
		"both unknown":   SettleDigest(Completed, Usage{}),
		"input unknown":  SettleDigest(Completed, Usage{nil, count(0)}),
		"output unknown": SettleDigest(Completed, Usage{count(0), nil}),
		"other outcome":  SettleDigest(Interrupted, Usage{count(0), count(0)}),
		"other count":    SettleDigest(Completed, Usage{count(1), count(0)}),
		"swapped counts": SettleDigest(Completed, Usage{count(0), count(1)}),
	} {
		if bytes.Equal(known, other) {
			t.Errorf("%s: digest did not change", name)
		}
	}
	if !bytes.Equal(SettleDigest(Interrupted, Usage{count(20), nil}), SettleDigest(Interrupted, Usage{count(20), nil})) {
		t.Error("the same content gave different digests")
	}
}
