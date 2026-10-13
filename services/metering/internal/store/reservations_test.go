package store_test

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentReservesNeverOverspend(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 10 * fixtureMaxCost})
	var counts map[int]int
	m.whileLocked(lockWallet, "tenant-01", func() {
		counts = together(50, func(i int) int {
			status, _ := m.post(reservePath, reserveBody(fmt.Sprintf("req_%d", i), "tenant-01"))
			return status
		})
	})

	if len(counts) != 2 || counts[http.StatusCreated] != 10 || counts[http.StatusConflict] != 40 {
		t.Errorf("statuses: got %v, want 10 x 201 and 40 x 409", counts)
	}
	m.wantWallet("tenant-01", 10*fixtureMaxCost, 10*fixtureMaxCost)
	if n := m.count("SELECT count(*) FROM accounting.reservations"); n != 10 {
		t.Errorf("reservations: got %d, want 10", n)
	}
	m.checkBooks()
}

func TestDuplicateReserveHoldsOnce(t *testing.T) {
	// The wallet covers exactly one hold, so a duplicate that was judged on its own
	// would be refused for balance.
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	var counts map[int]int
	// While the wallet is locked, the first call has inserted its reservation and
	// waits to hold the cost; the others wait on its key.
	m.whileLocked(lockWallet, "tenant-01", func() {
		counts = together(50, func(int) int {
			status, _ := m.post(reservePath, reserveBody("req_1", "tenant-01"))
			return status
		})
	})
	if len(counts) != 2 || counts[http.StatusCreated] != 1 || counts[http.StatusOK] != 49 {
		t.Errorf("statuses: got %v, want one 201 and 49 x 200", counts)
	}
	m.wantWallet("tenant-01", fixtureMaxCost, fixtureMaxCost)

	var recorded string
	if err := m.pool.QueryRow(context.Background(),
		"SELECT reservation_id FROM accounting.reservations").Scan(&recorded); err != nil {
		t.Fatalf("want exactly one reservation: %v", err)
	}
	status, reply := m.post(reservePath, reserveBody("req_1", "tenant-01"))
	if status != http.StatusOK || reply["reservation_id"] != recorded || reply["max_cost"] != float64(fixtureMaxCost) {
		t.Errorf("repeat: got %d %v, want 200 with %s", status, reply, recorded)
	}
	m.checkBooks()
}

func TestReserveReplayAfterSettlementNeedsNeitherAccessNorBalance(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	id := m.reserve("req_1", "tenant-01")
	if status, reply := m.post(settlePath(id), `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`); status != http.StatusOK {
		t.Fatalf("settle: got %d %v", status, reply)
	}
	m.accessDown.Store(true)
	lookups := m.priceRequests.Load()

	status, reply := m.post(reservePath, reserveBody("req_1", "tenant-01"))
	if status != http.StatusOK || reply["reservation_id"] != id || reply["status"] != "reserved" {
		t.Errorf("replay: got %d %v", status, reply)
	}
	if m.priceRequests.Load() != lookups {
		t.Errorf("the replay asked access for the price")
	}
	// 170 was charged, so the balance no longer covers another hold of 300.
	m.wantWallet("tenant-01", fixtureMaxCost-170, 0)
	if n := m.count("SELECT count(*) FROM accounting.reservations"); n != 1 {
		t.Errorf("reservations: got %d, want 1", n)
	}
	m.checkBooks()
}

func TestRejectedReserveLeavesNothingAndIsJudgedAgain(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": fixtureMaxCost})
	first := m.reserve("req_a", "tenant-01")

	for range 2 {
		if status, reply := m.post(reservePath, reserveBody("req_b", "tenant-01")); status != http.StatusConflict || errorCode(reply) != "insufficient_balance" {
			t.Errorf("no balance left: got %d %v", status, reply)
		}
	}
	if status, reply := m.post(reservePath, reserveBody("req_c", "tenant-99")); status != http.StatusNotFound || errorCode(reply) != "wallet_not_found" {
		t.Errorf("unknown wallet: got %d %v", status, reply)
	}
	if n := m.count("SELECT count(*) FROM accounting.reservations WHERE call_id <> 'req_a'"); n != 0 {
		t.Errorf("rejected reserves left %d reservations", n)
	}
	if n := m.count("SELECT count(*) FROM accounting.ledger_entries"); n != 0 {
		t.Errorf("ledger entries: got %d, want 0", n)
	}
	m.wantWallet("tenant-01", fixtureMaxCost, fixtureMaxCost)

	// Releasing the first hold makes room, and the same ids now succeed.
	if status, reply := m.post(settlePath(first), `{"input_tokens":0,"output_tokens":0,"outcome":"upstream_error"}`); status != http.StatusOK || reply["charged"] != float64(0) {
		t.Fatalf("settle: got %d %v", status, reply)
	}
	m.reserve("req_b", "tenant-01")
	m.checkBooks()
}

func TestConflictingReuseChangesNothing(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 1000, "tenant-02": 1000})
	id := m.reserve("req_1", "tenant-01")
	fixture := reserveBody("req_1", "tenant-01")
	for name, body := range map[string]string{
		"tenant":        reserveBody("req_1", "tenant-02"),
		"price version": strings.Replace(fixture, "pv-1", "pv-2", 1),
		"max input":     strings.Replace(fixture, `"max_input_tokens":100`, `"max_input_tokens":101`, 1),
		"max output":    strings.Replace(fixture, `"max_output_tokens":20`, `"max_output_tokens":21`, 1),
	} {
		if status, reply := m.post(reservePath, body); status != http.StatusUnprocessableEntity || errorCode(reply) != "idempotency_conflict" {
			t.Errorf("reserve with another %s: got %d %v", name, status, reply)
		}
	}

	if status, reply := m.post(settlePath(id), `{"input_tokens":20,"output_tokens":null,"outcome":"interrupted"}`); status != http.StatusOK || reply["charged"] != float64(140) {
		t.Fatalf("settle: got %d %v", status, reply)
	}
	for name, body := range map[string]string{
		"count":             `{"input_tokens":21,"output_tokens":null,"outcome":"interrupted"}`,
		"zero for unknown":  `{"input_tokens":20,"output_tokens":0,"outcome":"interrupted"}`,
		"unknown for known": `{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`,
		"outcome":           `{"input_tokens":20,"output_tokens":null,"outcome":"client_cancelled"}`,
	} {
		if status, reply := m.post(settlePath(id), body); status != http.StatusUnprocessableEntity || errorCode(reply) != "idempotency_conflict" {
			t.Errorf("settle with another %s: got %d %v", name, status, reply)
		}
	}
	if status, reply := m.post(settlePath("res_missing"), `{"input_tokens":1,"output_tokens":1,"outcome":"completed"}`); status != http.StatusNotFound || errorCode(reply) != "reservation_not_found" {
		t.Errorf("unknown reservation: got %d %v", status, reply)
	}

	m.wantWallet("tenant-01", 1000-140, 0)
	m.wantWallet("tenant-02", 1000, 0)
	if reservations, entries := m.count("SELECT count(*) FROM accounting.reservations"), m.count("SELECT count(*) FROM accounting.ledger_entries"); reservations != 1 || entries != 1 {
		t.Errorf("got %d reservations and %d ledger entries, want 1 and 1", reservations, entries)
	}
	m.checkBooks()
}

func TestSettleChargesAndRecordsEveryCase(t *testing.T) {
	count := func(n int64) *int64 { return &n }
	cases := []struct {
		body        string
		outcome     string
		input       *int64
		output      *int64
		wantCharged int64
	}{
		{`{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`, "completed", count(60), count(10), 170},
		{`{"input_tokens":20,"output_tokens":4,"outcome":"client_cancelled"}`, "client_cancelled", count(20), count(4), 60},
		{`{"input_tokens":20,"output_tokens":null,"outcome":"interrupted"}`, "interrupted", count(20), nil, 140},
		{`{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`, "interrupted", nil, nil, 300},
		{`{"input_tokens":0,"output_tokens":0,"outcome":"upstream_error"}`, "upstream_error", count(0), count(0), 0},
		{`{"input_tokens":0,"output_tokens":0,"outcome":"not_dispatched"}`, "not_dispatched", count(0), count(0), 0},
		{`{"input_tokens":1000,"output_tokens":100,"outcome":"completed"}`, "completed", count(1000), count(100), 300},
		{`{"input_tokens":9223372036854775807,"output_tokens":1,"outcome":"completed"}`, "completed", count(math.MaxInt64), count(1), 300},
	}
	const start = 100_000
	m := newMeter(t, map[string]int64{"tenant-01": start})
	same := func(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	var total int64
	for i, tc := range cases {
		callID := fmt.Sprintf("req_%d", i)
		id := m.reserve(callID, "tenant-01")
		status, reply := m.post(settlePath(id), tc.body)
		entryID, _ := reply["ledger_entry_id"].(string)
		if status != http.StatusOK || reply["charged"] != float64(tc.wantCharged) || reply["status"] != "settled" || entryID == "" {
			t.Errorf("%s: got %d %v, want charged %d", tc.body, status, reply, tc.wantCharged)
			continue
		}
		total += tc.wantCharged

		var (
			state, outcome string
			input, output  *int64
			charged        int64
		)
		if err := m.pool.QueryRow(context.Background(), `SELECT status, outcome, input_tokens, output_tokens, charged
			FROM accounting.reservations WHERE reservation_id = $1 AND settled_at IS NOT NULL`, id).Scan(
			&state, &outcome, &input, &output, &charged); err != nil {
			t.Fatalf("%s: reservation: %v", tc.body, err)
		}
		if state != "settled" || outcome != tc.outcome || !same(input, tc.input) || !same(output, tc.output) || charged != tc.wantCharged {
			t.Errorf("%s: the reservation was recorded differently", tc.body)
		}

		var (
			reservationID, entryCall, tenant string
			attempt                          int32
			amount                           int64
		)
		if err := m.pool.QueryRow(context.Background(), `SELECT reservation_id, call_id, attempt_id, tenant_id,
				amount, input_tokens, output_tokens, outcome
			FROM accounting.ledger_entries WHERE ledger_entry_id = $1`, entryID).Scan(
			&reservationID, &entryCall, &attempt, &tenant, &amount, &input, &output, &outcome); err != nil {
			t.Fatalf("%s: ledger entry: %v", tc.body, err)
		}
		if reservationID != id || entryCall != callID || attempt != 1 || tenant != "tenant-01" || amount != tc.wantCharged ||
			!same(input, tc.input) || !same(output, tc.output) || outcome != tc.outcome {
			t.Errorf("%s: the ledger entry was recorded differently", tc.body)
		}
	}
	m.wantWallet("tenant-01", start-total, 0)
	if n := m.count("SELECT count(*) FROM accounting.ledger_entries"); n != int64(len(cases)) {
		t.Errorf("ledger entries: got %d, want one per settlement, %d", n, len(cases))
	}
	m.checkBooks()
}

func TestConcurrentSettlesChargeOnce(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 1000})
	id := m.reserve("req_1", "tenant-01")
	body := `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`

	var mu sync.Mutex
	results := map[string]int{}
	var counts map[int]int
	m.whileLocked(lockReservation, id, func() {
		counts = together(50, func(int) int {
			status, reply := m.post(settlePath(id), body)
			mu.Lock()
			results[fmt.Sprint(reply["ledger_entry_id"], " ", reply["charged"])]++
			mu.Unlock()
			return status
		})
	})
	if len(counts) != 1 || counts[http.StatusOK] != 50 {
		t.Errorf("statuses: got %v, want 50 x 200", counts)
	}
	if len(results) != 1 {
		t.Errorf("the callers saw different results: %v", results)
	}
	if n := m.count("SELECT count(*) FROM accounting.ledger_entries"); n != 1 {
		t.Errorf("ledger entries: got %d, want 1", n)
	}
	m.wantWallet("tenant-01", 1000-170, 0)
	m.checkBooks()
}

func TestNothingStaysOpenBetweenReserveAndSettle(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 1000, "tenant-02": 1000})
	m.reserve("req_1", "tenant-01")

	// The model would be streaming now. Metering holds no connection, no transaction
	// and no lock, and it asked access for the price before it took a connection.
	if busy := m.busyAtPrice.Load(); busy != 0 {
		t.Errorf("the price lookup ran while %d connections were in use", busy)
	}
	if acquired := m.pool.Stat().AcquiredConns(); acquired != 0 {
		t.Errorf("connections still in use after reserve: %d", acquired)
	}
	ctx := context.Background()
	var transactions, locks int64
	if err := m.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
			AND backend_type = 'client backend' AND xact_start IS NOT NULL`).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if err := m.admin.QueryRow(ctx, `SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE a.datname = current_database() AND l.pid <> pg_backend_pid()
			AND a.backend_type = 'client backend'`).Scan(&locks); err != nil {
		t.Fatal(err)
	}
	if transactions != 0 || locks != 0 {
		t.Errorf("after reserve: %d open transactions and %d locks, want none", transactions, locks)
	}

	m.reserve("req_2", "tenant-02")
	m.checkBooks()
}

func TestCallsThatCannotProceedChangeNothing(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 1000})
	if status, reply := m.send("another-token", reservePath, reserveBody("req_1", "tenant-01")); status != http.StatusUnauthorized || errorCode(reply) != "unauthorized" {
		t.Errorf("wrong token: got %d %v", status, reply)
	}
	m.accessDown.Store(true)
	if status, reply := m.post(reservePath, reserveBody("req_1", "tenant-01")); status != http.StatusServiceUnavailable || errorCode(reply) != "dependency_unavailable" {
		t.Errorf("access down: got %d %v", status, reply)
	}
	if n := m.count("SELECT count(*) FROM accounting.reservations"); n != 0 {
		t.Errorf("reservations: got %d, want 0", n)
	}
	m.wantWallet("tenant-01", 1000, 0)
	m.checkBooks()

	// Access is back, and the same ids reserve; then the database goes away.
	m.accessDown.Store(false)
	id := m.reserve("req_1", "tenant-01")
	m.pool.Close()
	if status, reply := m.post(settlePath(id), `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`); status != http.StatusServiceUnavailable || errorCode(reply) != "dependency_unavailable" {
		t.Errorf("settle without a database: got %d %v", status, reply)
	}
	if status, reply := m.post(reservePath, reserveBody("req_1", "tenant-01")); status != http.StatusServiceUnavailable || errorCode(reply) != "dependency_unavailable" {
		t.Errorf("reserve without a database: got %d %v", status, reply)
	}
}
