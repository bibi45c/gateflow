package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
)

// Console answers the console's wallet and usage queries and applies wallet
// adjustments. It is the api.ConsoleStore of the service.
type Console struct {
	pool *pgxpool.Pool
}

// NewConsole returns the console store over pool, which must already be migrated.
func NewConsole(pool *pgxpool.Pool) *Console {
	return &Console{pool: pool}
}

const walletColumns = `tenant_id, balance, held, balance - held, updated_at`

func scanWallet(row pgx.Row) (api.Wallet, error) {
	var w api.Wallet
	err := row.Scan(&w.TenantID, &w.Balance, &w.Held, &w.Available, &w.UpdatedAt)
	return w, err
}

// Wallets returns every wallet, by tenant.
func (s *Console) Wallets(ctx context.Context) ([]api.Wallet, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT `+walletColumns+` FROM accounting.wallets
		ORDER BY tenant_id COLLATE "C"`)
	if err != nil {
		return nil, failure("wallets", err)
	}
	wallets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (api.Wallet, error) { return scanWallet(row) })
	if err != nil {
		return nil, failure("wallets", err)
	}
	return wallets, nil
}

// Usage returns the tenant's wallet, the summary of the calls reserved since since,
// and its latest limit calls, newest first.
func (s *Console) Usage(ctx context.Context, tenantID string, since time.Time, limit int) (api.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	usage := api.Usage{Summary: api.UsageSummary{Since: since}, Items: []api.UsageItem{}}
	wallet, err := scanWallet(s.pool.QueryRow(ctx, `SELECT `+walletColumns+` FROM accounting.wallets
		WHERE tenant_id = $1`, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Usage{}, api.ErrWalletNotFound
	}
	if err != nil {
		return api.Usage{}, failure("usage", err)
	}
	usage.Wallet = wallet
	err = s.pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(charged), 0),
			coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0)
		FROM accounting.reservations WHERE tenant_id = $1 AND created_at >= $2`, tenantID, since).
		Scan(&usage.Summary.Requests, &usage.Summary.Charged, &usage.Summary.InputTokens, &usage.Summary.OutputTokens)
	if err != nil {
		return api.Usage{}, failure("usage", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT reservation_id, call_id, model, status, outcome,
			input_tokens, output_tokens, max_cost, charged, created_at, settled_at
		FROM accounting.reservations WHERE tenant_id = $1
		ORDER BY created_at DESC, reservation_id LIMIT $2`, tenantID, limit)
	if err != nil {
		return api.Usage{}, failure("usage", err)
	}
	usage.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (api.UsageItem, error) {
		var i api.UsageItem
		err := row.Scan(&i.ReservationID, &i.CallID, &i.Model, &i.Status, &i.Outcome,
			&i.InputTokens, &i.OutputTokens, &i.MaxCost, &i.Charged, &i.CreatedAt, &i.SettledAt)
		return i, err
	})
	if err != nil {
		return api.Usage{}, failure("usage", err)
	}
	return usage, nil
}

const adjustmentColumns = `adjustment_id, tenant_id, amount, note, balance_after, created_at`

func scanAdjustment(row pgx.Row) (api.Adjustment, error) {
	var a api.Adjustment
	err := row.Scan(&a.ID, &a.TenantID, &a.Amount, &a.Note, &a.BalanceAfter, &a.CreatedAt)
	return a, err
}

// Adjust applies a in one transaction. A transaction lock on the adjustment id makes
// a concurrent repeat wait for this one and then read the recorded row.
func (s *Console) Adjust(ctx context.Context, a api.Adjustment) (api.Adjustment, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	var stored api.Adjustment
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, a.ID); err != nil {
			return err
		}
		var err error
		stored, err = scanAdjustment(tx.QueryRow(ctx, `SELECT `+adjustmentColumns+`
			FROM accounting.wallet_adjustments WHERE adjustment_id = $1`, a.ID))
		if err == nil {
			if stored.TenantID != a.TenantID || stored.Amount != a.Amount || stored.Note != a.Note {
				return api.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO accounting.wallets (tenant_id, balance, held)
			VALUES ($1, 0, 0) ON CONFLICT (tenant_id) DO NOTHING`, a.TenantID); err != nil {
			return err
		}
		var balance int64
		// wallets_available_check refuses a balance below what is held.
		if err := tx.QueryRow(ctx, `UPDATE accounting.wallets SET balance = balance + $2, updated_at = now()
			WHERE tenant_id = $1 RETURNING balance`, a.TenantID, a.Amount).Scan(&balance); err != nil {
			return err
		}
		stored, err = scanAdjustment(tx.QueryRow(ctx, `INSERT INTO accounting.wallet_adjustments
				(adjustment_id, tenant_id, amount, note, balance_after)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+adjustmentColumns, a.ID, a.TenantID, a.Amount, a.Note, balance))
		created = err == nil
		return err
	})
	switch {
	case errors.Is(err, api.ErrConflict):
		return api.Adjustment{}, false, err
	case isCheckViolation(err):
		return api.Adjustment{}, false, api.ErrInsufficientBalance
	case err != nil:
		return api.Adjustment{}, false, failure("adjust", err)
	}
	return stored, created, nil
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
