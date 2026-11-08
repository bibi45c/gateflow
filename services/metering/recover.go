package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/config"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
)

// recoverTimeout bounds one run of the command.
const recoverTimeout = 5 * time.Minute

// recoverHeld runs `metering recover`, given the arguments after "recover". It settles
// the reservations that are still held after --older-than, oldest first and at most
// --limit of them, as UC6 asks: a caller that never settled, because its settle failed
// or its Pod died, would otherwise hold that amount of the wallet for good. Each is
// charged as an interrupted call with unknown counts, which is its max cost. With
// --dry-run it only lists them. It logs one JSON line per reservation and prints a
// summary, and fails when any recovery failed, so a scheduler marks the run failed.
func recoverHeld(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet(name+" recover", flag.ContinueOnError)
	olderThan := flags.Duration("older-than", 30*time.Minute, "recover reservations held longer than this")
	limit := flags.Int("limit", 1000, "recover at most this many reservations in one run")
	dryRun := flags.Bool("dry-run", false, "list the reservations that would be recovered and change nothing")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	switch {
	case flags.NArg() > 0:
		return fmt.Errorf("recover: unexpected arguments %q", flags.Args())
	case *olderThan < time.Second:
		return fmt.Errorf("recover: --older-than must be at least 1s, got %s", *olderThan)
	case *limit < 1 || *limit > 10_000:
		return fmt.Errorf("recover: --limit must be from 1 to 10000, got %d", *limit)
	}
	databaseURL, err := config.LoadSeed(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), recoverTimeout)
	defer cancel()
	pool, err := store.Open(ctx, databaseURL, 0)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger := slog.New(slog.NewJSONHandler(stdout, nil)).With("service", name, "command", "recover")
	return runRecover(ctx, store.NewReservations(pool), logger, stdout, *olderThan, *limit, *dryRun)
}

// recoverStore is what runRecover needs from store.Reservations.
type recoverStore interface {
	StaleReservations(ctx context.Context, olderThan time.Duration, limit int) ([]store.Stale, error)
	Recover(ctx context.Context, reservationID string) (api.Settled, error)
}

// runRecover recovers the stale reservations of rs and prints the summary line.
func runRecover(ctx context.Context, rs recoverStore, logger *slog.Logger, stdout io.Writer,
	olderThan time.Duration, limit int, dryRun bool) error {
	stale, err := rs.StaleReservations(ctx, olderThan, limit)
	if err != nil {
		return err
	}
	var recovered, skipped, failed int
	var lastErr error
	for _, r := range stale {
		attrs := []any{"reservation_id", r.ID, "call_id", r.CallID, "tenant_id", r.TenantID,
			"max_cost", r.MaxCost, "held_seconds", int64(time.Since(r.CreatedAt).Seconds())}
		if dryRun {
			logger.Info("would recover", attrs...)
			continue
		}
		settled, err := rs.Recover(ctx, r.ID)
		switch {
		case err == nil:
			recovered++
			logger.Warn("recovered a held reservation",
				append(attrs, "charged", settled.Charged, "ledger_entry_id", settled.LedgerEntryID)...)
		case errors.Is(err, api.ErrConflict), errors.Is(err, api.ErrReservationNotFound):
			skipped++ // settled by its caller after the query
			logger.Info("skipped: settled meanwhile", attrs...)
		default:
			failed++
			lastErr = err
			logger.Error("recovery failed", append(attrs, "error", err.Error())...)
		}
	}
	verb := "recovered"
	if dryRun {
		verb, recovered = "would recover", len(stale)
	}
	fmt.Fprintf(stdout, "%s recover: %d held longer than %s, %s %d, skipped %d, failed %d\n",
		name, len(stale), olderThan, verb, recovered, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("recover: %d of %d recoveries failed, last: %w", failed, len(stale), lastErr)
	}
	return nil
}
