package store

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// RecoveryDigest is the settle digest that a recovery records. No settle request can
// produce it, so a recovered reservation can be told apart from one its caller
// settled, and a late settle from the gateway is refused as a conflict instead of
// being taken as a replay.
var RecoveryDigest = func() []byte {
	sum := sha256.Sum256([]byte("gateflow metering recover v1"))
	return sum[:]
}()

// Stale is a reservation still held after the recovery threshold.
type Stale struct {
	ID        string
	CallID    string
	TenantID  string
	MaxCost   int64
	CreatedAt time.Time
}

// StaleReservations returns at most limit reservations that are still reserved and
// were created more than olderThan ago, oldest first. It locks nothing: Recover
// settles each one under the reservation's row lock, as a settle request would.
func (s *Reservations) StaleReservations(ctx context.Context, olderThan time.Duration, limit int) ([]Stale, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT reservation_id, call_id, tenant_id, max_cost, created_at
		FROM accounting.reservations
		WHERE status = 'reserved' AND created_at < now() - make_interval(secs => $1)
		ORDER BY created_at, reservation_id LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, failure("stale reservations", err)
	}
	defer rows.Close()
	var stale []Stale
	for rows.Next() {
		var r Stale
		if err := rows.Scan(&r.ID, &r.CallID, &r.TenantID, &r.MaxCost, &r.CreatedAt); err != nil {
			return nil, failure("stale reservations", err)
		}
		stale = append(stale, r)
	}
	if err := rows.Err(); err != nil {
		return nil, failure("stale reservations", err)
	}
	return stale, nil
}

// Recover settles a held reservation whose caller never settled it, as UC6 asks. It
// records the outcome interrupted with unknown token counts, which the billing rules
// charge at the reservation's max cost, the same as an interrupted call whose counts
// were lost. It runs the settle transaction, so the ledger entry, the status and the
// release of the hold change together. If the caller settled the reservation first,
// it returns api.ErrConflict and changes nothing; a repeated recovery returns the
// recorded result.
func (s *Reservations) Recover(ctx context.Context, reservationID string) (api.Settled, error) {
	return s.Settle(ctx, reservationID, api.Settlement{Outcome: billing.Interrupted, Digest: RecoveryDigest})
}
