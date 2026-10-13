// Package billing computes what a model call may cost and what it is charged. It
// follows the pricing rule of the S1 contract: amounts are int64 micro-units, an
// unknown token count is charged at its maximum, and a charge never exceeds the
// reservation.
package billing

import "math"

// MaxPrice is the highest price a price version may carry, in micro-units per token.
const MaxPrice = 1_000_000

// Prices are the two rates of one price version, in micro-units per token.
type Prices struct {
	Input  int64
	Output int64
}

// Valid reports whether both prices are inside the contract's range.
func (p Prices) Valid() bool {
	return p.Input >= 0 && p.Input <= MaxPrice && p.Output >= 0 && p.Output <= MaxPrice
}

// Bounds are the token maxima a reservation is made with.
type Bounds struct {
	MaxInput  int64
	MaxOutput int64
}

// Usage holds the token counts reported at settlement. A nil count is unknown.
type Usage struct {
	Input  *int64
	Output *int64
}

// Outcome says how a call ended.
type Outcome string

const (
	Completed       Outcome = "completed"
	UpstreamError   Outcome = "upstream_error"
	ClientCancelled Outcome = "client_cancelled"
	Interrupted     Outcome = "interrupted"
	NotDispatched   Outcome = "not_dispatched"
)

// Valid reports whether o is one of the five outcomes of the contract.
func (o Outcome) Valid() bool {
	switch o {
	case Completed, UpstreamError, ClientCancelled, Interrupted, NotDispatched:
		return true
	}
	return false
}

// chargesNothing reports the outcomes taken as proof that no tokens were used.
func (o Outcome) chargesNothing() bool {
	return o == UpstreamError || o == NotDispatched
}

// MaxCost returns the most a call with these bounds can cost. ok is false when the
// amount does not fit in int64. Prices and bounds must not be negative.
func MaxCost(p Prices, b Bounds) (cost int64, ok bool) {
	return cost64(p, b.MaxInput, b.MaxOutput)
}

// Charge returns the amount to settle for a reservation of maxCost. An unknown count
// is replaced by its bound, and the result is capped at maxCost; an amount that does
// not fit in int64 also charges maxCost, so settlement never fails on a large count.
func Charge(p Prices, b Bounds, maxCost int64, u Usage, o Outcome) int64 {
	if o.chargesNothing() {
		return 0
	}
	input, output := b.MaxInput, b.MaxOutput
	if u.Input != nil {
		input = *u.Input
	}
	if u.Output != nil {
		output = *u.Output
	}
	cost, ok := cost64(p, input, output)
	if !ok || cost > maxCost {
		return maxCost
	}
	return cost
}

// cost64 returns input x input price + output x output price without wrapping.
func cost64(p Prices, input, output int64) (int64, bool) {
	in, ok := mul(input, p.Input)
	if !ok {
		return 0, false
	}
	out, ok := mul(output, p.Output)
	if !ok || in > math.MaxInt64-out {
		return 0, false
	}
	return in + out, true
}

// mul multiplies two non-negative numbers and reports whether the product fits.
func mul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}
