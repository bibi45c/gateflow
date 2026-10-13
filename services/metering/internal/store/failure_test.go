package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
)

func TestFailureMarksOnlyRetryableErrorsUnavailable(t *testing.T) {
	for name, err := range map[string]error{
		"deadline":            context.DeadlineExceeded,
		"cancelled":           fmt.Errorf("query: %w", context.Canceled),
		"closed pool":         errors.New("closed pool"),
		"connection lost":     &pgconn.PgError{Code: "08006"},
		"too many clients":    &pgconn.PgError{Code: "53300"},
		"server shutting":     &pgconn.PgError{Code: "57P01"},
		"statement cancelled": &pgconn.PgError{Code: "57014"},
		"lock not available":  &pgconn.PgError{Code: "55P03"},
		"deadlock":            &pgconn.PgError{Code: "40P01"},
	} {
		if got := failure("reserve", err); !errors.Is(got, api.ErrUnavailable) {
			t.Errorf("%s: %v is not marked unavailable", name, got)
		}
	}
	for name, err := range map[string]error{
		"check violation":  &pgconn.PgError{Code: "23514", ConstraintName: "wallets_available_check"},
		"unique violation": &pgconn.PgError{Code: "23505"},
		"undefined table":  &pgconn.PgError{Code: "42P01"},
		"ledger trigger":   &pgconn.PgError{Code: "P0001"},
	} {
		got := failure("settle", err)
		if errors.Is(got, api.ErrUnavailable) {
			t.Errorf("%s: %v is marked unavailable", name, got)
		}
		if !strings.HasPrefix(got.Error(), "store: settle: ") {
			t.Errorf("%s: %q does not name the operation", name, got)
		}
	}
}

func TestNewIDIsOpaqueAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := newID("res_")
		if len(id) != len("res_")+32 || !strings.HasPrefix(id, "res_") || seen[id] {
			t.Fatalf("id %q is malformed or repeated", id)
		}
		seen[id] = true
	}
}
