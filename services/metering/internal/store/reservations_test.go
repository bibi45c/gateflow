package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/metering/internal/accessclient"
	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
)

const (
	meteringToken = "metering-token-test"
	accessToken   = "access-token-test"
	reservePath   = "/internal/v1/reservations"
	// The shared fixture of the contract: prices 2 and 5 with these bounds cost 300.
	fixtureMaxCost = 300
)

// meter is the metering API served over HTTP on a real database, with a stand-in
// for the access service that knows one price version, pv-1.
type meter struct {
	t             *testing.T
	pool          *pgxpool.Pool // as the metering role, used by the service
	admin         *pgxpool.Pool // as the superuser, to look at other sessions
	server        *httptest.Server
	start         map[string]int64 // starting balance of each wallet
	priceRequests atomic.Int64
	accessDown    atomic.Bool
	busyAtPrice   atomic.Int32 // most connections in use when a price request arrived
}

func newMeter(t *testing.T, balances map[string]int64) *meter {
	t.Helper()
	db := pgtest.New(t)
	m := &meter{t: t, pool: db.Pool(t, "metering"), admin: db.Pool(t, ""), start: balances}
	ctx := context.Background()
	if _, err := store.Migrate(ctx, m.pool); err != nil {
		t.Fatal(err)
	}
	for tenant, balance := range balances {
		if _, err := m.pool.Exec(ctx, `INSERT INTO accounting.wallets (tenant_id, balance)
			VALUES ($1, $2)`, tenant, balance); err != nil {
			t.Fatal(err)
		}
	}

	access := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.priceRequests.Add(1)
		if busy := m.pool.Stat().AcquiredConns(); busy > m.busyAtPrice.Load() {
			m.busyAtPrice.Store(busy)
		}
		switch {
		case m.accessDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Header.Get("Authorization") != "Bearer "+accessToken:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path != "/internal/v1/price-versions/pv-1":
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = io.WriteString(w, `{"id":"pv-1","input_micro_per_token":2,"output_micro_per_token":5}`)
		}
	}))
	t.Cleanup(access.Close)

	mux := http.NewServeMux()
	api.New(store.NewReservations(m.pool), accessclient.New(access.URL, accessToken), meteringToken).Register(mux)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// send posts body to path with token and returns the status and the decoded reply.
// It is safe to call from several goroutines.
func (m *meter) send(token, path, body string) (int, map[string]any) {
	request, err := http.NewRequest(http.MethodPost, m.server.URL+path, strings.NewReader(body))
	if err != nil {
		m.t.Error(err)
		return 0, nil
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Request-Id", "req_test")
	response, err := m.server.Client().Do(request)
	if err != nil {
		m.t.Error(err)
		return 0, nil
	}
	defer response.Body.Close()
	var reply map[string]any
	_ = json.NewDecoder(response.Body).Decode(&reply)
	return response.StatusCode, reply
}

func (m *meter) post(path, body string) (int, map[string]any) {
	return m.send(meteringToken, path, body)
}

func reserveBody(callID, tenant string) string {
	return fmt.Sprintf(`{"call_id":%q,"attempt_id":1,"tenant_id":%q,"price_version_id":"pv-1",`+
		`"max_input_tokens":100,"max_output_tokens":20}`, callID, tenant)
}

func settlePath(reservationID string) string {
	return reservePath + "/" + reservationID + "/settle"
}

func errorCode(reply map[string]any) string {
	fields, _ := reply["error"].(map[string]any)
	code, _ := fields["code"].(string)
	return code
}

// reserve makes the fixture reservation for callID and returns its id.
func (m *meter) reserve(callID, tenant string) string {
	m.t.Helper()
	status, reply := m.post(reservePath, reserveBody(callID, tenant))
	if status != http.StatusCreated || reply["max_cost"] != float64(fixtureMaxCost) || reply["status"] != "reserved" {
		m.t.Fatalf("reserve %s: got %d %v", callID, status, reply)
	}
	return reply["reservation_id"].(string)
}

// count runs a query that returns one bigint.
func (m *meter) count(query string, args ...any) int64 {
	m.t.Helper()
	var n int64
	if err := m.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		m.t.Fatal(err)
	}
	return n
}

func (m *meter) wallet(tenant string) (balance, held int64) {
	m.t.Helper()
	err := m.pool.QueryRow(context.Background(), `SELECT balance, held FROM accounting.wallets
		WHERE tenant_id = $1`, tenant).Scan(&balance, &held)
	if err != nil {
		m.t.Fatal(err)
	}
	return balance, held
}

func (m *meter) wantWallet(tenant string, wantBalance, wantHeld int64) {
	m.t.Helper()
	if balance, held := m.wallet(tenant); balance != wantBalance || held != wantHeld {
		m.t.Errorf("%s: balance %d and held %d, want %d and %d", tenant, balance, held, wantBalance, wantHeld)
	}
}

// checkBooks checks what must hold for every wallet whatever happened: the hold is
// the sum of its open reservations, the balance is the starting balance minus the
// ledger, and every settled reservation has exactly one ledger entry.
func (m *meter) checkBooks() {
	m.t.Helper()
	for tenant, start := range m.start {
		balance, held := m.wallet(tenant)
		open := m.count(`SELECT COALESCE(SUM(max_cost), 0)::bigint FROM accounting.reservations
			WHERE tenant_id = $1 AND status = 'reserved'`, tenant)
		charged := m.count(`SELECT COALESCE(SUM(amount), 0)::bigint FROM accounting.ledger_entries
			WHERE tenant_id = $1`, tenant)
		if held != open || balance != start-charged || balance-held < 0 {
			m.t.Errorf("%s: balance %d, held %d; open reservations %d, ledger %d, start %d",
				tenant, balance, held, open, charged, start)
		}
	}
	unmatched := m.count(`SELECT count(*) FROM accounting.reservations r
		FULL JOIN accounting.ledger_entries l USING (reservation_id)
		WHERE (r.status = 'settled') IS DISTINCT FROM (l.ledger_entry_id IS NOT NULL)
			OR r.charged IS DISTINCT FROM l.amount`)
	if unmatched != 0 {
		m.t.Errorf("%d reservations and ledger entries do not match one to one", unmatched)
	}
}

// whileLocked runs calls while another transaction holds the row lock that the lock
// query takes, and lets go once at least two of the calls are waiting for it. The
// calls are then certain to overlap inside their transactions, which a plain burst of
// requests only does by chance.
func (m *meter) whileLocked(lock, id string, calls func()) {
	m.t.Helper()
	ctx := context.Background()
	// Open the connections the waiting calls and the watcher below will use, so that
	// the time to connect, which can be long on a busy machine, is not part of the wait.
	var warm [3]*pgxpool.Conn
	for i := range warm {
		conn, err := m.pool.Acquire(ctx)
		if err != nil {
			m.t.Fatal(err)
		}
		warm[i] = conn
	}
	for _, conn := range warm {
		conn.Release()
	}
	holder, err := m.admin.Begin(ctx)
	if err != nil {
		m.t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, lock, id); err != nil {
		m.t.Fatal(err)
	}
	if err := m.admin.Ping(ctx); err != nil { // the watcher's own connection
		m.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		calls()
	}()

	// Well inside store.OperationTimeout, so no call gives up while it waits.
	var waiting int64
	for deadline := time.Now().Add(store.OperationTimeout / 2); waiting < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		if err := m.admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND backend_type = 'client backend'
				AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			m.t.Error(err)
			break
		}
	}
	if err := holder.Rollback(ctx); err != nil {
		m.t.Error(err)
	}
	<-done
	if waiting < 2 {
		m.t.Errorf("only %d calls were waiting for the lock, so the test forced no overlap", waiting)
	}
}

const (
	lockWallet      = `SELECT 1 FROM accounting.wallets WHERE tenant_id = $1 FOR NO KEY UPDATE`
	lockReservation = `SELECT 1 FROM accounting.reservations WHERE reservation_id = $1 FOR UPDATE`
)

// together runs call n times at once and counts the statuses it returned.
func together(n int, call func(i int) int) map[int]int {
	statuses := make([]int, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			statuses[i] = call(i)
		}()
	}
	start.Done()
	done.Wait()
	counts := map[int]int{}
	for _, status := range statuses {
		counts[status]++
	}
	return counts
}

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
