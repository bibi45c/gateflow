package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
