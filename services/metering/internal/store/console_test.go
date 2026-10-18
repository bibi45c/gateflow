package store_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// get sends a GET to path with the token and decodes the reply.
func (m *meter) get(path string) (int, map[string]any) {
	m.t.Helper()
	request, err := http.NewRequest(http.MethodGet, m.server.URL+path, nil)
	if err != nil {
		m.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+meteringToken)
	response, err := m.server.Client().Do(request)
	if err != nil {
		m.t.Fatal(err)
	}
	defer response.Body.Close()
	var reply map[string]any
	_ = json.NewDecoder(response.Body).Decode(&reply)
	return response.StatusCode, reply
}

func withModel(body, model string) string {
	return strings.TrimSuffix(body, "}") + `,"model":"` + model + `"}`
}

func TestUsageShowsModelCostAndSummary(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 10 * fixtureMaxCost})
	status, reply := m.post(reservePath, withModel(reserveBody("req_model", "tenant-01"), "mock-fast"))
	if status != http.StatusCreated {
		t.Fatalf("reserve with model: %d %v", status, reply)
	}
	id := reply["reservation_id"].(string)
	if status, reply := m.post(settlePath(id), `{"input_tokens":10,"output_tokens":4,"outcome":"completed"}`); status != http.StatusOK {
		t.Fatalf("settle: %d %v", status, reply)
	}
	m.reserve("req_plain", "tenant-01") // no model, still open

	status, usage := m.get("/internal/v1/tenants/tenant-01/usage?since=2000-01-01T00:00:00Z&limit=10")
	if status != http.StatusOK {
		t.Fatalf("usage: %d %v", status, usage)
	}
	summary := usage["summary"].(map[string]any)
	if summary["requests"] != float64(2) || summary["charged"] != float64(10*2+4*5) || summary["output_tokens"] != float64(4) {
		t.Errorf("summary = %v", summary)
	}
	items := usage["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	newest, oldest := items[0].(map[string]any), items[1].(map[string]any)
	if newest["model"] != nil || newest["status"] != "reserved" || newest["charged"] != nil {
		t.Errorf("open call = %v", newest)
	}
	if oldest["model"] != "mock-fast" || oldest["outcome"] != "completed" || oldest["charged"] != float64(40) {
		t.Errorf("settled call = %v", oldest)
	}
	wallet := usage["wallet"].(map[string]any)
	if wallet["held"] != float64(fixtureMaxCost) || wallet["balance"] != float64(10*fixtureMaxCost-40) {
		t.Errorf("wallet = %v", wallet)
	}
	if status, reply := m.get("/internal/v1/tenants/nobody/usage"); status != http.StatusNotFound || errorCode(reply) != "wallet_not_found" {
		t.Errorf("unknown tenant: %d %v", status, reply)
	}
	if status, _ := m.get("/internal/v1/tenants/tenant-01/usage?limit=0"); status != http.StatusBadRequest {
		t.Errorf("limit 0: %d", status)
	}
	if status, reply := m.post(reservePath, withModel(reserveBody("req_bad", "tenant-01"), "")); status != http.StatusBadRequest {
		t.Errorf("empty model: %d %v", status, reply)
	}
	m.checkBooks()
}

func TestAdjustmentsTopUpOnceAndNeverOverdraw(t *testing.T) {
	m := newMeter(t, map[string]int64{"tenant-01": 1000})
	path := "/internal/v1/wallets/tenant-01/adjustments"
	var wg sync.WaitGroup
	statuses := make([]int, 20)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _ = m.post(path, `{"adjustment_id":"adj-1","amount":500,"note":"top-up"}`)
		}()
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		if s == http.StatusCreated {
			created++
		} else if s != http.StatusOK {
			t.Errorf("repeat status %d", s)
		}
	}
	if created != 1 {
		t.Errorf("%d adjustments created, want 1", created)
	}
	m.wantWallet("tenant-01", 1500, 0)

	if status, reply := m.post(path, `{"adjustment_id":"adj-1","amount":600}`); status != http.StatusUnprocessableEntity ||
		errorCode(reply) != "idempotency_conflict" {
		t.Errorf("reused id: %d %v", status, reply)
	}
	if status, reply := m.post(path, `{"adjustment_id":"adj-2","amount":-1501}`); status != http.StatusConflict ||
		errorCode(reply) != "insufficient_balance" {
		t.Errorf("overdraw: %d %v", status, reply)
	}
	status, reply := m.post(path, `{"adjustment_id":"adj-3","amount":-500,"note":"correction"}`)
	if status != http.StatusCreated || reply["balance_after"] != float64(1000) {
		t.Errorf("correction: %d %v", status, reply)
	}
	for _, body := range []string{`{"adjustment_id":"adj-4","amount":0}`, `{"amount":5}`, `{"adjustment_id":"adj-5"}`} {
		if status, _ := m.post(path, body); status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, status)
		}
	}
	// A top-up creates a missing wallet, which then lists.
	if status, reply := m.post("/internal/v1/wallets/tenant-new/adjustments", `{"adjustment_id":"adj-6","amount":700}`); status != http.StatusCreated {
		t.Fatalf("new wallet: %d %v", status, reply)
	}
	status, wallets := m.get("/internal/v1/wallets")
	items, _ := wallets["items"].([]any)
	if status != http.StatusOK || len(items) != 2 || items[1].(map[string]any)["available"] != float64(700) {
		t.Errorf("wallets: %d %v", status, wallets)
	}
	if n := m.count("SELECT count(*) FROM accounting.wallet_adjustments"); n != 3 {
		t.Errorf("adjustments: %d, want 3", n)
	}
}
