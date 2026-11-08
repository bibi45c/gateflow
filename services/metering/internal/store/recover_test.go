package store_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
)

const recoverAfter = 30 * time.Minute

// age moves a reservation's creation back by d, as if it had been held that long.
func (m *meter) age(reservationID string, d time.Duration) {
	m.t.Helper()
	if _, err := m.pool.Exec(context.Background(), `UPDATE accounting.reservations
		SET created_at = created_at - make_interval(secs => $2) WHERE reservation_id = $1`,
		reservationID, d.Seconds()); err != nil {
		m.t.Fatal(err)
	}
}

func staleIDs(t *testing.T, rs *store.Reservations) []string {
	t.Helper()
	stale, err := rs.StaleReservations(context.Background(), recoverAfter, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(stale))
	for i, r := range stale {
		ids[i] = r.ID
	}
	return ids
}

func TestRecoverSettlesOnlyStaleReservationsAtMaxCost(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 10 * fixtureMaxCost})
	rs := store.NewReservations(m.pool)
	old, young := m.reserve("req_old", "tenant-01"), m.reserve("req_young", "tenant-01")
	m.age(old, time.Hour)
	m.age(young, recoverAfter-time.Minute)

	ids := staleIDs(t, rs)
	if len(ids) != 1 || ids[0] != old {
		t.Fatalf("stale: got %v, want only %s", ids, old)
	}
	settled, err := rs.Recover(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Charged != fixtureMaxCost || settled.LedgerEntryID == "" {
		t.Errorf("recover: got %+v, want a ledger entry charging %d", settled, fixtureMaxCost)
	}
	var outcome string
	var digest []byte
	if err := m.pool.QueryRow(context.Background(), `SELECT outcome, settle_digest
		FROM accounting.reservations WHERE reservation_id = $1`, old).Scan(&outcome, &digest); err != nil {
		t.Fatal(err)
	}
	if outcome != "interrupted" || !bytes.Equal(digest, store.RecoveryDigest) {
		t.Errorf("recorded outcome %q and digest %x, want interrupted with the recovery digest", outcome, digest)
	}
	// The young reservation is still held.
	m.wantWallet("tenant-01", 10*fixtureMaxCost-fixtureMaxCost, fixtureMaxCost)
	if ids := staleIDs(t, rs); len(ids) != 0 {
		t.Errorf("stale after recovery: got %v, want none", ids)
	}
	m.checkBooks()
}

func TestRecoverTwiceChargesOnce(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	rs := store.NewReservations(m.pool)
	id := m.reserve("req_1", "tenant-01")
	m.age(id, time.Hour)

	first, err := rs.Recover(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rs.Recover(context.Background(), id)
	if err != nil || second != first {
		t.Errorf("repeat: got %+v, %v; want %+v", second, err, first)
	}
	m.wantWallet("tenant-01", 0, 0)
	m.checkBooks()
}

func TestRecoverLeavesASettledReservationAlone(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	rs := store.NewReservations(m.pool)
	id := m.reserve("req_1", "tenant-01")
	if status, reply := m.post(settlePath(id), `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`); status != http.StatusOK {
		t.Fatalf("settle: got %d %v", status, reply)
	}
	m.age(id, time.Hour)

	if ids := staleIDs(t, rs); len(ids) != 0 {
		t.Errorf("stale: got %v, want none", ids)
	}
	if _, err := rs.Recover(context.Background(), id); !errors.Is(err, api.ErrConflict) {
		t.Errorf("recover after settle: got %v, want ErrConflict", err)
	}
	m.wantWallet("tenant-01", fixtureMaxCost-170, 0)
	m.checkBooks()
}

func TestLateSettleAfterRecoverIsAConflict(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	rs := store.NewReservations(m.pool)
	id := m.reserve("req_1", "tenant-01")
	m.age(id, time.Hour)
	if _, err := rs.Recover(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Even a late settle with the same outcome and no counts is not taken as a replay.
	for _, body := range []string{`{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`, `{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`} {
		status, reply := m.post(settlePath(id), body)
		if status != http.StatusUnprocessableEntity || errorCode(reply) != "idempotency_conflict" {
			t.Errorf("late settle %s: got %d %v, want 422 idempotency_conflict", body, status, reply)
		}
	}
	m.wantWallet("tenant-01", 0, 0)
	m.checkBooks()
}

func TestRecoverRacingASettleChargesOnce(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	rs := store.NewReservations(m.pool)
	id := m.reserve("req_1", "tenant-01")
	m.age(id, time.Hour)

	var (
		recoverErr error
		settleCode int
	)
	m.whileLocked(lockReservation, id, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, recoverErr = rs.Recover(context.Background(), id) }()
		go func() {
			defer wg.Done()
			settleCode, _ = m.post(settlePath(id), `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`)
		}()
		wg.Wait()
	})

	// Exactly one side wins; the other is refused as an idempotency conflict.
	recovered := recoverErr == nil
	settled := settleCode == http.StatusOK
	if recovered == settled {
		t.Errorf("recover err %v and settle status %d: want exactly one to win", recoverErr, settleCode)
	}
	if !recovered && !errors.Is(recoverErr, api.ErrConflict) {
		t.Errorf("losing recover: got %v, want ErrConflict", recoverErr)
	}
	if !settled && settleCode != http.StatusUnprocessableEntity {
		t.Errorf("losing settle: got %d, want 422", settleCode)
	}
	if n := m.count("SELECT count(*) FROM accounting.ledger_entries"); n != 1 {
		t.Errorf("ledger entries: got %d, want 1", n)
	}
	m.checkBooks()
}
