package meteringclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
	"github.com/bibi45c/gateflow/services/gateway/internal/meteringclient"
)

const (
	token     = "metering-token-canary-22bb"
	requestID = "req_0a9b8c7d-6e5f-4a3b-9c2d-1e0f2a3b4c5d"
)

// The shared-fixture reserve of api/metering.yaml.
var fixtureReserve = meteringclient.ReserveRequest{
	CallID:          requestID,
	AttemptID:       1,
	TenantID:        "tenant-01",
	PriceVersionID:  "pv-fixture-2-5",
	MaxInputTokens:  100,
	MaxOutputTokens: 20,
}

const fixtureReserveJSON = `{"call_id":"` + requestID + `","attempt_id":1,"tenant_id":"tenant-01",` +
	`"price_version_id":"pv-fixture-2-5","max_input_tokens":100,"max_output_tokens":20}`

// client returns a client of a stand-in that checks the path, token and request id,
// and the request body against wantBody, then answers status and reply.
func client(t *testing.T, wantPath, wantBody string, status int, reply string) *meteringclient.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != wantPath {
			t.Errorf("got %s %s, want POST %s", r.Method, r.URL.EscapedPath(), wantPath)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Request-Id") != requestID {
			t.Error("missing the token or the request id")
		}
		if body, _ := io.ReadAll(r.Body); string(body) != wantBody {
			t.Errorf("body = %s, want %s", body, wantBody)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return meteringclient.New(internalapi.NewCaller(server.URL, token))
}

func TestReserveAcceptsANewHoldAndAReplay(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		c := client(t, "/internal/v1/reservations", fixtureReserveJSON, status,
			`{"reservation_id":"rsv_f1","max_cost":300,"status":"reserved"}`)
		got, err := c.Reserve(context.Background(), requestID, fixtureReserve)
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if want := (meteringclient.Reservation{ReservationID: "rsv_f1", MaxCost: 300, Status: "reserved"}); got != want {
			t.Errorf("status %d: got %+v, want %+v", status, got, want)
		}
	}
}

func TestReserveRefusesAnAnswerThatConfirmsNoHold(t *testing.T) {
	for _, reply := range []string{
		`{"reservation_id":"","max_cost":300,"status":"reserved"}`,
		`{"reservation_id":"rsv_f1","max_cost":300,"status":"settled"}`,
	} {
		c := client(t, "/internal/v1/reservations", fixtureReserveJSON, http.StatusCreated, reply)
		if _, err := c.Reserve(context.Background(), requestID, fixtureReserve); !errors.Is(err, internalapi.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", reply, err)
		}
	}
}

func TestReserveReturnsTheErrorCode(t *testing.T) {
	c := client(t, "/internal/v1/reservations", fixtureReserveJSON, http.StatusConflict,
		`{"error":{"code":"insufficient_balance","message":"The available balance is below the reservation amount.",`+
			`"request_id":"`+requestID+`"}}`)
	_, err := c.Reserve(context.Background(), requestID, fixtureReserve)
	if internalapi.Code(err) != meteringclient.CodeInsufficientBalance {
		t.Errorf("err = %v, want code insufficient_balance", err)
	}
}

func TestSettleSendsUnknownCountsAsNull(t *testing.T) {
	input := int64(20)
	c := client(t, "/internal/v1/reservations/rsv_f3/settle",
		`{"input_tokens":20,"output_tokens":null,"outcome":"interrupted"}`, http.StatusOK,
		`{"charged":140,"ledger_entry_id":"led_f3","status":"settled"}`)
	got, err := c.Settle(context.Background(), requestID, "rsv_f3",
		meteringclient.SettleRequest{InputTokens: &input, Outcome: "interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (meteringclient.Settlement{Charged: 140, LedgerEntryID: "led_f3", Status: "settled"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSettleEscapesTheReservationID(t *testing.T) {
	c := client(t, "/internal/v1/reservations/rsv%2F1%3Fx/settle",
		`{"input_tokens":null,"output_tokens":null,"outcome":"client_cancelled"}`, http.StatusOK,
		`{"charged":300,"ledger_entry_id":"led_f4","status":"settled"}`)
	if _, err := c.Settle(context.Background(), requestID, "rsv/1?x",
		meteringclient.SettleRequest{Outcome: "client_cancelled"}); err != nil {
		t.Fatal(err)
	}
}
