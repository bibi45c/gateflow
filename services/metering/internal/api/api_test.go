package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bibi45c/gateflow/services/metering/internal/accessclient"
	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

const (
	token       = "metering-token"
	reservePath = "/internal/v1/reservations"
	// The shared fixture of the contract: prices 2 and 5, bounds 100 and 20, cost 300.
	fixtureReserve = `{"call_id":"req_1","attempt_id":1,"tenant_id":"tenant-01","price_version_id":"pv-1","max_input_tokens":100,"max_output_tokens":20}`
)

type reply struct {
	status int
	body   map[string]any
}

func (r reply) errorCode() string {
	fields, _ := r.body["error"].(map[string]any)
	code, _ := fields["code"].(string)
	return code
}

// service is the handler under test with its in-memory store and price table.
type service struct {
	mux    *http.ServeMux
	store  *fakeStore
	prices *fakePrices
}

func newService(wallets map[string]int64) *service {
	s := &service{
		mux:    http.NewServeMux(),
		store:  newFakeStore(wallets),
		prices: &fakePrices{versions: map[string]billing.Prices{"pv-1": {Input: 2, Output: 5}}},
	}
	New(s.store, s.prices, token, nil).Register(s.mux)
	return s
}

func (s *service) send(path, bearer, body string) reply {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if bearer != "" {
		request.Header.Set("Authorization", bearer)
	}
	request.Header.Set("X-Request-Id", "req_test")
	recorder := httptest.NewRecorder()
	s.mux.ServeHTTP(recorder, request)
	got := reply{status: recorder.Code}
	_ = json.Unmarshal(recorder.Body.Bytes(), &got.body)
	return got
}

func (s *service) post(path, body string) reply { return s.send(path, "Bearer "+token, body) }

// reserved makes the fixture reservation and returns its id.
func (s *service) reserved(t *testing.T) string {
	t.Helper()
	got := s.post(reservePath, fixtureReserve)
	if got.status != http.StatusCreated {
		t.Fatalf("reserve: got %d %v", got.status, got.body)
	}
	return got.body["reservation_id"].(string)
}

func settlePath(reservationID string) string {
	return reservePath + "/" + reservationID + "/settle"
}

func TestReserveHoldsTheMaximumCost(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	got := s.post(reservePath, fixtureReserve)

	if got.status != http.StatusCreated {
		t.Fatalf("status: got %d %v", got.status, got.body)
	}
	if got.body["max_cost"] != float64(300) || got.body["status"] != "reserved" || got.body["reservation_id"] == "" {
		t.Errorf("body: got %v", got.body)
	}
	if s.store.available["tenant-01"] != 700 {
		t.Errorf("available: got %d, want 700", s.store.available["tenant-01"])
	}
	if s.prices.lastID != "req_test" {
		t.Errorf("request id passed to access: got %q", s.prices.lastID)
	}
}

func TestReserveReplayReturnsTheRecordWithoutPriceOrBalance(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 300})
	id := s.reserved(t)

	// Key order and whitespace differ; the content is the same.
	reordered := `{ "max_output_tokens": 20, "max_input_tokens": 100, "price_version_id": "pv-1",
		"tenant_id": "tenant-01", "attempt_id": 1, "call_id": "req_1" }`
	got := s.post(reservePath, reordered)
	if got.status != http.StatusOK || got.body["reservation_id"] != id || got.body["max_cost"] != float64(300) {
		t.Errorf("replay: got %d %v", got.status, got.body)
	}

	// After settlement, with access down and the wallet empty, the answer is the same.
	if got := s.post(settlePath(id), `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`); got.status != http.StatusOK {
		t.Fatalf("settle: got %d %v", got.status, got.body)
	}
	s.prices.err = accessclient.ErrUnavailable
	s.store.available["tenant-01"] = 0
	got = s.post(reservePath, fixtureReserve)
	if got.status != http.StatusOK || got.body["reservation_id"] != id || got.body["status"] != "reserved" {
		t.Errorf("replay after settlement: got %d %v", got.status, got.body)
	}
	if s.prices.lookups != 1 {
		t.Errorf("price lookups: got %d, want 1", s.prices.lookups)
	}
	if len(s.store.reservations) != 1 {
		t.Errorf("reservations: got %d, want 1", len(s.store.reservations))
	}
}

func TestReserveRejectsTheSameKeyWithOtherContent(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000, "tenant-02": 1000})
	s.prices.versions["pv-2"] = billing.Prices{Input: 2, Output: 5}
	s.reserved(t)

	for name, body := range map[string]string{
		"tenant":        strings.Replace(fixtureReserve, "tenant-01", "tenant-02", 1),
		"price version": strings.Replace(fixtureReserve, "pv-1", "pv-2", 1),
		"max input":     strings.Replace(fixtureReserve, `"max_input_tokens":100`, `"max_input_tokens":101`, 1),
		"max output":    strings.Replace(fixtureReserve, `"max_output_tokens":20`, `"max_output_tokens":21`, 1),
	} {
		if got := s.post(reservePath, body); got.status != http.StatusUnprocessableEntity || got.errorCode() != "idempotency_conflict" {
			t.Errorf("%s: got %d %v", name, got.status, got.body)
		}
	}
	if s.store.available["tenant-01"] != 700 || s.store.available["tenant-02"] != 1000 || len(s.store.reservations) != 1 {
		t.Errorf("state changed: %v, %d reservations", s.store.available, len(s.store.reservations))
	}
	// Another attempt of the same call is another key.
	if got := s.post(reservePath, strings.Replace(fixtureReserve, `"attempt_id":1`, `"attempt_id":2`, 1)); got.status != http.StatusCreated {
		t.Errorf("second attempt: got %d %v", got.status, got.body)
	}
}

func TestReserveReportsBalanceAndWallet(t *testing.T) {
	s := newService(map[string]int64{"tenant-00": 0, "tenant-01": 299})
	if got := s.post(reservePath, fixtureReserve); got.status != http.StatusConflict || got.errorCode() != "insufficient_balance" {
		t.Errorf("one below the cost: got %d %v", got.status, got.body)
	}
	if got := s.post(reservePath, strings.Replace(fixtureReserve, "tenant-01", "tenant-00", 1)); got.errorCode() != "insufficient_balance" {
		t.Errorf("empty wallet: got %d %v", got.status, got.body)
	}
	if got := s.post(reservePath, strings.Replace(fixtureReserve, "tenant-01", "tenant-99", 1)); got.status != http.StatusNotFound || got.errorCode() != "wallet_not_found" {
		t.Errorf("unknown wallet: got %d %v", got.status, got.body)
	}
	if len(s.store.reservations) != 0 || s.store.available["tenant-01"] != 299 {
		t.Errorf("a rejected reserve left something: %v, %d reservations", s.store.available, len(s.store.reservations))
	}

	// Nothing was recorded, so the same key is judged again once the balance allows it.
	s.store.available["tenant-01"] = 300
	if got := s.post(reservePath, fixtureReserve); got.status != http.StatusCreated {
		t.Errorf("same key after the balance grew: got %d %v", got.status, got.body)
	}
}

func TestReserveRejectsMalformedRequestsBeforeAnyWork(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	edit := func(old, replacement string) string { return strings.Replace(fixtureReserve, old, replacement, 1) }
	for name, body := range map[string]string{
		"empty body":          ``,
		"not json":            `call_id=req_1`,
		"an array":            `[` + fixtureReserve + `]`,
		"trailing data":       fixtureReserve + `{}`,
		"missing call id":     edit(`"call_id":"req_1",`, ``),
		"empty call id":       edit(`"req_1"`, `""`),
		"long call id":        edit(`"req_1"`, `"`+strings.Repeat("a", 201)+`"`),
		"numeric tenant":      edit(`"tenant-01"`, `7`),
		"missing attempt":     edit(`"attempt_id":1,`, ``),
		"fractional attempt":  edit(`"attempt_id":1`, `"attempt_id":1.5`),
		"attempt zero":        edit(`"attempt_id":1`, `"attempt_id":0`),
		"negative attempt":    edit(`"attempt_id":1`, `"attempt_id":-1`),
		"attempt too large":   edit(`"attempt_id":1`, `"attempt_id":2147483648`),
		"unknown field":       edit(`"call_id"`, `"extra":true,"call_id"`),
		"null price version":  edit(`"pv-1"`, `null`),
		"negative max input":  edit(`"max_input_tokens":100`, `"max_input_tokens":-1`),
		"missing max output":  edit(`,"max_output_tokens":20`, ``),
		"max beyond int64":    edit(`"max_output_tokens":20`, `"max_output_tokens":9223372036854775808`),
		"max as a string":     edit(`"max_output_tokens":20`, `"max_output_tokens":"20"`),
		"body above the size": `{"call_id":"` + strings.Repeat("a", maxBodyBytes) + `"}`,
	} {
		if got := s.post(reservePath, body); got.status != http.StatusBadRequest || got.errorCode() != "invalid_request" {
			t.Errorf("%s: got %d %v", name, got.status, got.body)
		}
	}
	if s.store.finds != 0 || s.prices.lookups != 0 {
		t.Errorf("malformed requests reached the store %d times and access %d times", s.store.finds, s.prices.lookups)
	}
}

func TestReserveSortsPriceAndStoreFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		priceErr   error
		prices     billing.Prices
		storeErr   error
		body       string
		wantStatus int
		wantCode   string
	}{
		{"access unavailable", fmt.Errorf("lookup: %w", accessclient.ErrUnavailable), billing.Prices{}, nil, fixtureReserve, 503, "dependency_unavailable"},
		{"access rejected the lookup", fmt.Errorf("lookup: %w", accessclient.ErrRejected), billing.Prices{}, nil, fixtureReserve, 500, "internal_error"},
		{"price outside the range", nil, billing.Prices{Input: billing.MaxPrice + 1}, nil, fixtureReserve, 500, "internal_error"},
		{"cost does not fit", nil, billing.Prices{Input: billing.MaxPrice, Output: 1},
			nil, strings.Replace(fixtureReserve, `"max_input_tokens":100`, `"max_input_tokens":9223372036854775807`, 1), 400, "invalid_request"},
		{"database unavailable", nil, billing.Prices{Input: 2, Output: 5}, fmt.Errorf("begin: %w", ErrUnavailable), fixtureReserve, 503, "dependency_unavailable"},
		{"unexpected store error", nil, billing.Prices{Input: 2, Output: 5}, errors.New("boom"), fixtureReserve, 500, "internal_error"},
	} {
		s := newService(map[string]int64{"tenant-01": 1000})
		s.prices.err, s.prices.versions["pv-1"], s.store.fail = tc.priceErr, tc.prices, tc.storeErr
		got := s.post(reservePath, tc.body)
		if got.status != tc.wantStatus || got.errorCode() != tc.wantCode {
			t.Errorf("%s: got %d %v", tc.name, got.status, got.body)
		}
		if s.store.available["tenant-01"] != 1000 {
			t.Errorf("%s: the wallet changed", tc.name)
		}
	}
}

func TestSettleChargesTheSharedFixture(t *testing.T) {
	for _, tc := range []struct {
		body          string
		wantCharged   float64
		wantAvailable int64
	}{
		{`{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`, 170, 830},
		{`{"input_tokens":20,"output_tokens":4,"outcome":"client_cancelled"}`, 60, 940},
		{`{"input_tokens":20,"output_tokens":null,"outcome":"interrupted"}`, 140, 860},
		{`{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`, 300, 700},
		{`{"input_tokens":0,"output_tokens":0,"outcome":"upstream_error"}`, 0, 1000},
		{`{"input_tokens":0,"output_tokens":0,"outcome":"not_dispatched"}`, 0, 1000},
		{`{"input_tokens":1000,"output_tokens":100,"outcome":"completed"}`, 300, 700},
	} {
		s := newService(map[string]int64{"tenant-01": 1000})
		id := s.reserved(t)
		got := s.post(settlePath(id), tc.body)
		if got.status != http.StatusOK || got.body["charged"] != tc.wantCharged || got.body["status"] != "settled" || got.body["ledger_entry_id"] == "" {
			t.Errorf("%s: got %d %v", tc.body, got.status, got.body)
		}
		if s.store.available["tenant-01"] != tc.wantAvailable {
			t.Errorf("%s: available %d, want %d", tc.body, s.store.available["tenant-01"], tc.wantAvailable)
		}
	}
}

func TestSettleRepeatsAndConflicts(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	id := s.reserved(t)
	first := s.post(settlePath(id), `{"input_tokens":20,"output_tokens":null,"outcome":"interrupted"}`)

	repeat := s.post(settlePath(id), `{"outcome":"interrupted","output_tokens":null,"input_tokens":20}`)
	if repeat.status != http.StatusOK || repeat.body["charged"] != first.body["charged"] || repeat.body["ledger_entry_id"] != first.body["ledger_entry_id"] {
		t.Errorf("repeat: got %d %v, first %v", repeat.status, repeat.body, first.body)
	}
	for name, body := range map[string]string{
		"other count":       `{"input_tokens":21,"output_tokens":null,"outcome":"interrupted"}`,
		"zero for unknown":  `{"input_tokens":20,"output_tokens":0,"outcome":"interrupted"}`,
		"other outcome":     `{"input_tokens":20,"output_tokens":null,"outcome":"client_cancelled"}`,
		"unknown for known": `{"input_tokens":null,"output_tokens":null,"outcome":"interrupted"}`,
	} {
		if got := s.post(settlePath(id), body); got.status != http.StatusUnprocessableEntity || got.errorCode() != "idempotency_conflict" {
			t.Errorf("%s: got %d %v", name, got.status, got.body)
		}
	}
	if s.store.available["tenant-01"] != 860 {
		t.Errorf("available: got %d, want 860 after one charge of 140", s.store.available["tenant-01"])
	}
	if got := s.post(settlePath("res_missing"), `{"input_tokens":1,"output_tokens":1,"outcome":"completed"}`); got.status != http.StatusNotFound || got.errorCode() != "reservation_not_found" {
		t.Errorf("unknown reservation: got %d %v", got.status, got.body)
	}
}

func TestSettleRejectsMalformedRequests(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	id := s.reserved(t)
	for name, body := range map[string]string{
		"missing input":      `{"output_tokens":10,"outcome":"completed"}`,
		"missing output":     `{"input_tokens":60,"outcome":"completed"}`,
		"missing outcome":    `{"input_tokens":60,"output_tokens":10}`,
		"unknown outcome":    `{"input_tokens":60,"output_tokens":10,"outcome":"failed"}`,
		"negative count":     `{"input_tokens":-1,"output_tokens":10,"outcome":"completed"}`,
		"fractional count":   `{"input_tokens":60.5,"output_tokens":10,"outcome":"completed"}`,
		"count as a string":  `{"input_tokens":"60","output_tokens":10,"outcome":"completed"}`,
		"count beyond int64": `{"input_tokens":9223372036854775808,"output_tokens":10,"outcome":"completed"}`,
		"unknown field":      `{"input_tokens":60,"output_tokens":10,"outcome":"completed","extra":true}`,
		"not json":           `outcome=completed`,
	} {
		if got := s.post(settlePath(id), body); got.status != http.StatusBadRequest || got.errorCode() != "invalid_request" {
			t.Errorf("%s: got %d %v", name, got.status, got.body)
		}
	}
	// The path takes any id; one longer than metering gives out is simply unknown.
	if got := s.post(settlePath(strings.Repeat("a", 201)), `{"input_tokens":1,"output_tokens":1,"outcome":"completed"}`); got.status != http.StatusNotFound || got.errorCode() != "reservation_not_found" {
		t.Errorf("long reservation id: got %d %v", got.status, got.body)
	}
	if s.store.available["tenant-01"] != 700 {
		t.Errorf("a malformed settle changed the wallet: %d", s.store.available["tenant-01"])
	}
}

func TestEveryCallNeedsTheInternalToken(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	id := s.reserved(t)
	settleBody := `{"input_tokens":60,"output_tokens":10,"outcome":"completed"}`
	for name, bearer := range map[string]string{
		"no header":      "",
		"wrong token":    "Bearer metering-tokem",
		"token prefix":   "Bearer metering",
		"longer token":   "Bearer metering-token-and-more",
		"empty token":    "Bearer ",
		"another scheme": "Basic " + token,
		"bare token":     token,
	} {
		for _, path := range []string{reservePath, settlePath(id)} {
			body := settleBody
			if path == reservePath {
				body = strings.Replace(fixtureReserve, "req_1", "req_2", 1)
			}
			if got := s.send(path, bearer, body); got.status != http.StatusUnauthorized || got.errorCode() != "unauthorized" {
				t.Errorf("%s on %s: got %d %v", name, path, got.status, got.body)
			}
		}
	}
	if s.store.available["tenant-01"] != 700 || len(s.store.reservations) != 1 {
		t.Errorf("an unauthorized call changed state: %v", s.store.available)
	}

	// A service started without a token refuses everything, even an empty bearer.
	closed := http.NewServeMux()
	New(s.store, s.prices, "", nil).Register(closed)
	request := httptest.NewRequest(http.MethodPost, reservePath, strings.NewReader(fixtureReserve))
	request.Header.Set("Authorization", "Bearer ")
	recorder := httptest.NewRecorder()
	closed.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("handler without a token: got %d", recorder.Code)
	}
}

// generatedID is the form of an id metering makes up itself: "req_" and a version 4 UUID.
var generatedID = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestErrorBodyIsFixedAndEchoesOnlyAPlainRequestID(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	send := func(requestID string) (*httptest.ResponseRecorder, map[string]map[string]string) {
		request := httptest.NewRequest(http.MethodPost, reservePath, strings.NewReader(`{"call_id":"<script>secret</script>"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		if requestID != "" {
			request.Header.Set("X-Request-Id", requestID)
		}
		recorder := httptest.NewRecorder()
		s.mux.ServeHTTP(recorder, request)
		var body map[string]map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return recorder, body
	}

	recorder, body := send("req_3f2a-9")
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type: got %q", got)
	}
	want := map[string]string{"code": "invalid_request", "message": "The request body is invalid.", "request_id": "req_3f2a-9"}
	if len(body) != 1 || fmt.Sprint(body["error"]) != fmt.Sprint(want) {
		t.Errorf("body: got %v, want error %v", body, want)
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Error("the error body repeats request input")
	}
	// Without a usable id, metering makes one up instead of repeating the header.
	for _, unusable := range []string{"", "req 1", `req"1`, "req\n1", strings.Repeat("a", 129), "<b>"} {
		if _, body := send(unusable); !generatedID.MatchString(body["error"]["request_id"]) {
			t.Errorf("request id %q: the error body has %q, want a generated id", unusable, body["error"]["request_id"])
		}
	}
}

func TestRequestWithoutAnIDUsesOneGeneratedIDThroughout(t *testing.T) {
	s := newService(map[string]int64{"tenant-01": 1000})
	s.prices.err = accessclient.ErrUnavailable
	send := func() string {
		request := httptest.NewRequest(http.MethodPost, reservePath, strings.NewReader(fixtureReserve))
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		s.mux.ServeHTTP(recorder, request)
		var body map[string]map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status: got %d %v", recorder.Code, body)
		}
		return body["error"]["request_id"]
	}

	first := send()
	if !generatedID.MatchString(first) {
		t.Errorf("request id in the error body: got %q, want a generated id", first)
	}
	if s.prices.lastID != first {
		t.Errorf("request id passed to access: got %q, want the %q of the error body", s.prices.lastID, first)
	}
	if second := send(); second == first {
		t.Errorf("two requests got the same generated id %q", first)
	}
}

func TestConcurrentReservesAgreeOnOneResult(t *testing.T) {
	const callers = 50
	run := func(s *service, body func(int) string) map[int]int {
		statuses := make(chan int, callers)
		var start, done sync.WaitGroup
		start.Add(1)
		for i := range callers {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				statuses <- s.post(reservePath, body(i)).status
			}()
		}
		start.Done()
		done.Wait()
		close(statuses)
		counts := map[int]int{}
		for status := range statuses {
			counts[status]++
		}
		return counts
	}

	// The same call 50 times: one hold, and every caller gets the reservation.
	same := newService(map[string]int64{"tenant-01": 300})
	counts := run(same, func(int) string { return fixtureReserve })
	if counts[http.StatusCreated] != 1 || counts[http.StatusOK] != callers-1 {
		t.Errorf("same key: got %v, want one 201 and %d x 200", counts, callers-1)
	}
	if same.store.available["tenant-01"] != 0 || len(same.store.reservations) != 1 {
		t.Errorf("same key: available %d, %d reservations", same.store.available["tenant-01"], len(same.store.reservations))
	}

	// 50 different calls against a wallet that covers 10 of them.
	different := newService(map[string]int64{"tenant-01": 3000})
	counts = run(different, func(i int) string {
		return strings.Replace(fixtureReserve, "req_1", fmt.Sprintf("req_%d", i), 1)
	})
	if counts[http.StatusCreated] != 10 || counts[http.StatusConflict] != callers-10 {
		t.Errorf("different keys: got %v, want 10 x 201 and %d x 409", counts, callers-10)
	}
	if different.store.available["tenant-01"] != 0 {
		t.Errorf("different keys: available %d, want 0", different.store.available["tenant-01"])
	}
}
