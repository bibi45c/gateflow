package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// OperationTimeout bounds each method of Reservations, including any wait for a lock.
// It is below the 2 s a caller allows, so a call that waits for a lock is answered
// first. It does not bound a whole reserve, which also looks the key up and may fetch
// the price; a caller that gives up retries with the same ids.
const OperationTimeout = 1500 * time.Millisecond

// Reservations records cost holds and their settlement in the accounting schema. It
// is the api.Store of the service. Every method is one short transaction; nothing
// stays locked or connected between a reserve and its settle.
type Reservations struct {
	pool *pgxpool.Pool
}

// NewReservations returns the store over pool, which must already be migrated.
func NewReservations(pool *pgxpool.Pool) *Reservations {
	return &Reservations{pool: pool}
}

const reservationColumns = `reservation_id, call_id, attempt_id, tenant_id, price_version_id,
	input_price, output_price, max_input_tokens, max_output_tokens, max_cost, reserve_digest`

// FindReservation returns the reservation recorded for the key, if there is one.
func (s *Reservations) FindReservation(ctx context.Context, callID string, attemptID int32) (api.Reservation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	var r api.Reservation
	err := s.pool.QueryRow(ctx, `SELECT `+reservationColumns+` FROM accounting.reservations
		WHERE call_id = $1 AND attempt_id = $2`, callID, attemptID).Scan(
		&r.ID, &r.CallID, &r.AttemptID, &r.TenantID, &r.PriceVersionID,
		&r.Prices.Input, &r.Prices.Output, &r.Bounds.MaxInput, &r.Bounds.MaxOutput, &r.MaxCost, &r.Digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Reservation{}, false, nil
	}
	if err != nil {
		return api.Reservation{}, false, failure("find reservation", err)
	}
	return r, true, nil
}

// Reserve records r and holds r.MaxCost on the tenant's wallet, in one transaction.
//
// The reservation is inserted before the wallet is touched. A concurrent call with
// the same key then waits on the unique key for this one to finish and inserts
// nothing, instead of finding the balance already held and being refused.
func (s *Reservations) Reserve(ctx context.Context, r api.Reservation) (api.Reservation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	r.ID = newID("res_")
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO accounting.reservations (`+reservationColumns+`, model)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''))
			ON CONFLICT (call_id, attempt_id) DO NOTHING`,
			r.ID, r.CallID, r.AttemptID, r.TenantID, r.PriceVersionID,
			r.Prices.Input, r.Prices.Output, r.Bounds.MaxInput, r.Bounds.MaxOutput, r.MaxCost, r.Digest, r.Model)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // the key is already recorded
		}
		tag, err = tx.Exec(ctx, `UPDATE accounting.wallets SET held = held + $2, updated_at = now()
			WHERE tenant_id = $1 AND balance - held >= $2`, r.TenantID, r.MaxCost)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return api.ErrInsufficientBalance // rolls the reservation back
		}
		created = true
		return nil
	})
	switch {
	case errors.Is(err, api.ErrInsufficientBalance):
		return api.Reservation{}, false, err
	case isForeignKeyViolation(err):
		return api.Reservation{}, false, api.ErrWalletNotFound
	case err != nil:
		return api.Reservation{}, false, failure("reserve", err)
	case created:
		return r, true, nil
	}
	recorded, found, err := s.FindReservation(ctx, r.CallID, r.AttemptID)
	if err == nil && !found {
		err = fmt.Errorf("store: reserve: the reservation recorded for the same key is gone")
	}
	return recorded, false, err
}

// Settle charges a reservation once, in one transaction: it locks the reservation,
// writes the ledger entry, marks the reservation settled and releases the hold. A
// concurrent repeat waits on the lock and then reads the recorded result.
func (s *Reservations) Settle(ctx context.Context, reservationID string, settlement api.Settlement) (api.Settled, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	var settled api.Settled
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			r        api.Reservation
			status   string
			recorded []byte
			charged  *int64
		)
		err := tx.QueryRow(ctx, `SELECT call_id, attempt_id, tenant_id, input_price, output_price,
				max_input_tokens, max_output_tokens, max_cost, status, settle_digest, charged
			FROM accounting.reservations WHERE reservation_id = $1 FOR UPDATE`, reservationID).Scan(
			&r.CallID, &r.AttemptID, &r.TenantID, &r.Prices.Input, &r.Prices.Output,
			&r.Bounds.MaxInput, &r.Bounds.MaxOutput, &r.MaxCost, &status, &recorded, &charged)
		if errors.Is(err, pgx.ErrNoRows) {
			return api.ErrReservationNotFound
		}
		if err != nil {
			return err
		}
		if status == "settled" {
			if charged == nil || !bytes.Equal(recorded, settlement.Digest) {
				return api.ErrConflict
			}
			settled.Charged = *charged
			return tx.QueryRow(ctx, `SELECT ledger_entry_id FROM accounting.ledger_entries
				WHERE reservation_id = $1`, reservationID).Scan(&settled.LedgerEntryID)
		}

		settled = api.Settled{
			Charged:       billing.Charge(r.Prices, r.Bounds, r.MaxCost, settlement.Usage, settlement.Outcome),
			LedgerEntryID: newID("led_"),
		}
		outcome, usage := string(settlement.Outcome), settlement.Usage
		if _, err := tx.Exec(ctx, `INSERT INTO accounting.ledger_entries (ledger_entry_id, reservation_id,
				call_id, attempt_id, tenant_id, amount, input_tokens, output_tokens, outcome)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			settled.LedgerEntryID, reservationID, r.CallID, r.AttemptID, r.TenantID,
			settled.Charged, usage.Input, usage.Output, outcome); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounting.reservations SET status = 'settled',
				input_tokens = $2, output_tokens = $3, outcome = $4, charged = $5,
				settle_digest = $6, settled_at = now()
			WHERE reservation_id = $1`,
			reservationID, usage.Input, usage.Output, outcome, settled.Charged, settlement.Digest); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE accounting.wallets SET held = held - $2, balance = balance - $3,
				updated_at = now() WHERE tenant_id = $1`, r.TenantID, r.MaxCost, settled.Charged)
		return err
	})
	switch {
	case errors.Is(err, api.ErrReservationNotFound), errors.Is(err, api.ErrConflict):
		return api.Settled{}, err
	case err != nil:
		return api.Settled{}, failure("settle", err)
	}
	return settled, nil
}

// newID returns an opaque id: the prefix and 32 random hex characters.
func newID(prefix string) string {
	var random [16]byte
	_, _ = rand.Read(random[:]) // never fails
	return prefix + hex.EncodeToString(random[:])
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// failure wraps a database error, marking it api.ErrUnavailable when trying again can
// help: the server could not be reached or answered in time, shut down, ran out of
// resources, or gave up on a lock. A statement the server rejected for another reason
// is a bug or a broken rule, and stays an internal error.
func failure(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		retry := code == "55P03" || code == "40001" || code == "40P01" ||
			strings.HasPrefix(code, "08") || strings.HasPrefix(code, "53") || strings.HasPrefix(code, "57")
		if !retry {
			return fmt.Errorf("store: %s: %w", operation, err)
		}
	}
	// Anything that is not an answer from the server is a connection or timeout problem.
	return fmt.Errorf("store: %s: %w: %v", operation, api.ErrUnavailable, err)
}
