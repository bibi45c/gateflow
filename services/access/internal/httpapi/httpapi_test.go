package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/health"
	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/store"
)

const (
	testToken  = "test-internal-token"
	testPepper = "test-pepper"
	testKey    = "gf_test-key-0123456789"
)

var generatedRequestID = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// fakeStore answers from its fields and records what it was asked.
type fakeStore struct {
	key      store.VerifiedKey
	version  store.PriceVersion
	err      error
	panics   bool
	calls    int
	gotHMAC  []byte
	gotModel string
	gotID    string
}

func (f *fakeStore) VerifyKey(_ context.Context, keyHMAC []byte, model string) (store.VerifiedKey, error) {
	f.calls++
	f.gotHMAC, f.gotModel = keyHMAC, model
	if f.panics {
		panic("fake store panic")
	}
	return f.key, f.err
}

func (f *fakeStore) PriceVersion(_ context.Context, id string) (store.PriceVersion, error) {
	f.calls++
	f.gotID = id
	return f.version, f.err
}

// newTestServer returns the service's handler, as server.Main builds it, and the
// buffer its log lines go to.
func newTestServer(s Store, token string) (http.Handler, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler("access"))
	New(s, []byte(testPepper), token, slog.New(slog.NewJSONHandler(logs, nil))).Register(mux)
	return mux, logs
}

func call(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func authorized() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken}
}

func verifyBody(key, model string) string {
	body, _ := json.Marshal(map[string]string{"key": key, "model": model})
	return string(body)
}

// wantError checks the status and that the body is exactly the shared error body
// with the code's fixed message, and returns the request id.
func wantError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if recorder.Code != status {
		t.Errorf("status = %d, want %d", recorder.Code, status)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]map[string]string
	decoder := json.NewDecoder(recorder.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("error body: %v", err)
	}
	detail := body["error"]
	if len(body) != 1 || len(detail) != 3 {
		t.Errorf("error body = %v, want only error.code, error.message and error.request_id", body)
	}
	if detail["code"] != code {
		t.Errorf("error.code = %q, want %q", detail["code"], code)
	}
	if detail["message"] != errorKinds[code].message || detail["message"] == "" {
		t.Errorf("error.message = %q, want the fixed message of %s", detail["message"], code)
	}
	if detail["request_id"] == "" {
		t.Error("error.request_id is empty")
	}
	return detail["request_id"]
}

func TestInternalRoutesNeedTheToken(t *testing.T) {
	fake := &fakeStore{}
	handler, _ := newTestServer(fake, testToken)
	routes := []struct{ method, path, body string }{
		{"POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast")},
		{"GET", "/internal/v1/price-versions/pv-1", ""},
		{"GET", "/internal/v1/no-such-route", ""},
	}
	refused := map[string]map[string]string{
		"missing header":       {},
		"empty header":         {"Authorization": ""},
		"empty token":          {"Authorization": "Bearer "},
		"wrong token":          {"Authorization": "Bearer not-the-token"},
		"token prefix":         {"Authorization": "Bearer " + testToken[:len(testToken)-1]},
		"token with a suffix":  {"Authorization": "Bearer " + testToken + "x"},
		"no scheme":            {"Authorization": testToken},
		"other scheme":         {"Authorization": "Basic " + testToken},
		"lower-case scheme":    {"Authorization": "bearer " + testToken},
		"two spaces":           {"Authorization": "Bearer  " + testToken},
		"token in another key": {"X-Internal-Token": testToken},
	}
	for _, route := range routes {
		for name, headers := range refused {
			t.Run(route.path+"/"+name, func(t *testing.T) {
				wantError(t, call(handler, route.method, route.path, route.body, headers), http.StatusUnauthorized, CodeUnauthorized)
			})
		}
	}
	if fake.calls != 0 {
		t.Errorf("the store was called %d times without a valid token", fake.calls)
	}
}

func TestCorrectTokenIsAccepted(t *testing.T) {
	handler, _ := newTestServer(&fakeStore{}, testToken)
	if got := call(handler, "GET", "/internal/v1/price-versions/pv-1", "", authorized()); got.Code != http.StatusOK {
		t.Errorf("price version with the token: status %d, want 200", got.Code)
	}
	if got := call(handler, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), authorized()); got.Code != http.StatusOK {
		t.Errorf("verify with the token: status %d, want 200", got.Code)
	}
	if got := call(handler, "GET", "/internal/v1/no-such-route", "", authorized()); got.Code != http.StatusNotFound {
		t.Errorf("unknown route with the token: status %d, want 404", got.Code)
	}
}

func TestEmptyConfiguredTokenRefusesEveryCall(t *testing.T) {
	handler, _ := newTestServer(&fakeStore{}, "")
	for _, header := range []string{"", "Bearer ", "Bearer x"} {
		got := call(handler, "GET", "/internal/v1/price-versions/pv-1", "", map[string]string{"Authorization": header})
		wantError(t, got, http.StatusUnauthorized, CodeUnauthorized)
	}
}

func TestHealthzStaysOpen(t *testing.T) {
	handler, _ := newTestServer(&fakeStore{}, testToken)
	if got := call(handler, "GET", "/healthz", "", nil); got.Code != http.StatusOK {
		t.Errorf("/healthz without a token: status %d, want 200", got.Code)
	}
}

func TestEveryCodeHasAStatusAndAFixedMessage(t *testing.T) {
	want := map[string]int{
		CodeInvalidRequest:        400,
		CodeUnauthorized:          401,
		CodeInvalidKey:            401,
		CodeModelNotAllowed:       403,
		CodePriceVersionNotFound:  404,
		CodeDependencyUnavailable: 503,
		CodeInternalError:         500,
	}
	if len(errorKinds) != len(want) {
		t.Errorf("%d error codes, want %d", len(errorKinds), len(want))
	}
	messages := map[string]bool{}
	for code, status := range want {
		recorder := httptest.NewRecorder()
		writeError(recorder, httptest.NewRequest("GET", "/", nil), code)
		// Outside a route there is no request id; only the rest is checked here.
		var body errorBody
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != status || body.Error.Code != code || body.Error.Message == "" {
			t.Errorf("%s: status %d, body %+v; want status %d and a message", code, recorder.Code, body, status)
		}
		if messages[body.Error.Message] {
			t.Errorf("%s shares its message with another code", code)
		}
		messages[body.Error.Message] = true
	}
	recorder := httptest.NewRecorder()
	writeError(recorder, httptest.NewRequest("GET", "/", nil), "no_such_code")
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), CodeInternalError) {
		t.Errorf("unknown code: status %d, body %s; want 500 internal_error", recorder.Code, recorder.Body)
	}
}

func TestRequestIDComesFromTheHeaderOrIsGenerated(t *testing.T) {
	handler, logs := newTestServer(&fakeStore{err: store.ErrPriceVersionNotFound}, testToken)

	headers := authorized()
	headers["X-Request-Id"] = "req_from-the-gateway"
	got := wantError(t, call(handler, "GET", "/internal/v1/price-versions/pv-1", "", headers), http.StatusNotFound, CodePriceVersionNotFound)
	if got != "req_from-the-gateway" {
		t.Errorf("request_id = %q, want the X-Request-Id header", got)
	}
	if !strings.Contains(logs.String(), `"request_id":"req_from-the-gateway"`) {
		t.Errorf("log line lacks the request id: %s", logs)
	}

	first := wantError(t, call(handler, "GET", "/internal/v1/price-versions/pv-1", "", authorized()), http.StatusNotFound, CodePriceVersionNotFound)
	second := wantError(t, call(handler, "GET", "/internal/v1/price-versions/pv-1", "", nil), http.StatusUnauthorized, CodeUnauthorized)
	for _, id := range []string{first, second} {
		if !generatedRequestID.MatchString(id) {
			t.Errorf("generated request_id = %q, want req_<uuid>", id)
		}
	}
	if first == second {
		t.Error("two requests got the same generated request_id")
	}
}

func TestVerifyRejectsBadRequests(t *testing.T) {
	const secret = "gf_secret-input-value"
	padding := strings.Repeat("a", MaxBodyBytes)
	bodies := map[string]string{
		"empty body":          "",
		"malformed JSON":      `{"key":"` + secret + `","model":`,
		"not an object":       `["` + secret + `","mock-fast"]`,
		"null":                `null`,
		"string":              `"` + secret + `"`,
		"missing key":         `{"model":"mock-fast"}`,
		"missing model":       `{"key":"` + secret + `"}`,
		"empty key":           `{"key":"","model":"mock-fast"}`,
		"empty model":         `{"key":"` + secret + `","model":""}`,
		"null key":            `{"key":null,"model":"mock-fast"}`,
		"null model":          `{"key":"` + secret + `","model":null}`,
		"number key":          `{"key":12345,"model":"mock-fast"}`,
		"number model":        `{"key":"` + secret + `","model":7}`,
		"object model":        `{"key":"` + secret + `","model":{"name":"mock-fast"}}`,
		"array key":           `{"key":["` + secret + `"],"model":"mock-fast"}`,
		"trailing data":       verifyBody(secret, "mock-fast") + `{}`,
		"larger than the cap": `{"key":"` + secret + `","model":"mock-fast","padding":"` + padding + `"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			fake := &fakeStore{}
			handler, logs := newTestServer(fake, testToken)
			got := call(handler, "POST", "/internal/v1/keys/verify", body, authorized())
			raw := got.Body.String()
			wantError(t, got, http.StatusBadRequest, CodeInvalidRequest)
			if fake.calls != 0 {
				t.Error("the store was called for a bad request")
			}
			if strings.Contains(raw, secret) || strings.Contains(logs.String(), secret) {
				t.Error("the response or the log echoes request input")
			}
		})
	}
}

func TestVerifyAcceptsABodyAtTheCap(t *testing.T) {
	fake := &fakeStore{}
	handler, _ := newTestServer(fake, testToken)
	body := verifyBody(testKey, "mock-fast")
	body += strings.Repeat(" ", MaxBodyBytes-len(body))
	if got := call(handler, "POST", "/internal/v1/keys/verify", body, authorized()); got.Code != http.StatusOK {
		t.Errorf("body of exactly %d bytes: status %d, want 200", MaxBodyBytes, got.Code)
	}
}

func TestVerifyReturnsTheKeyAndChannel(t *testing.T) {
	fake := &fakeStore{key: store.VerifiedKey{
		TenantID:       "tenant-01",
		KeyID:          "key-tenant-01",
		PriceVersionID: "pv-seed-1",
		Channel:        store.Channel{ID: "mock", Protocol: "anthropic-messages", BaseURL: "http://mock-upstream:8090"},
	}}
	handler, _ := newTestServer(fake, testToken)
	got := call(handler, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), authorized())
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", got.Code, got.Body)
	}
	const want = `{"tenant_id":"tenant-01","key_id":"key-tenant-01","price_version_id":"pv-seed-1",` +
		`"channel":{"id":"mock","protocol":"anthropic-messages","base_url":"http://mock-upstream:8090","credential_env":""}}`
	if body := strings.TrimSpace(got.Body.String()); body != want {
		t.Errorf("body = %s\nwant   %s", body, want)
	}
	if !bytes.Equal(fake.gotHMAC, apikey.HMAC([]byte(testPepper), testKey)) || fake.gotModel != "mock-fast" {
		t.Error("the store was not asked for HMAC(pepper, key) and the model")
	}
}

func TestVerifyMapsStoreErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unknown or revoked key", store.ErrInvalidKey, http.StatusUnauthorized, CodeInvalidKey},
		{"model not allowed", store.ErrModelNotAllowed, http.StatusForbidden, CodeModelNotAllowed},
		{"database error", fmt.Errorf("%w: %w", store.ErrUnavailable, errors.New("connection refused")), http.StatusServiceUnavailable, CodeDependencyUnavailable},
		{"timeout", context.DeadlineExceeded, http.StatusServiceUnavailable, CodeDependencyUnavailable},
		{"anything else", errors.New("unexpected"), http.StatusInternalServerError, CodeInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, _ := newTestServer(&fakeStore{err: tt.err}, testToken)
			got := call(handler, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), authorized())
			wantError(t, got, tt.status, tt.code)
		})
	}
}

func TestPanicGivesInternalError(t *testing.T) {
	handler, logs := newTestServer(&fakeStore{panics: true}, testToken)
	got := call(handler, "POST", "/internal/v1/keys/verify", verifyBody(testKey, "mock-fast"), authorized())
	wantError(t, got, http.StatusInternalServerError, CodeInternalError)
	if !strings.Contains(logs.String(), `"status":500`) {
		t.Errorf("no log line with status 500: %s", logs)
	}
	if strings.Contains(logs.String(), testKey) {
		t.Error("the panic log holds the key")
	}
}

func TestPriceVersion(t *testing.T) {
	fake := &fakeStore{version: store.PriceVersion{ID: "pv-seed-1", InputMicroPerToken: 3, OutputMicroPerToken: 15}}
	handler, _ := newTestServer(fake, testToken)
	got := call(handler, "GET", "/internal/v1/price-versions/pv-seed-1", "", authorized())
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", got.Code, got.Body)
	}
	const want = `{"id":"pv-seed-1","input_micro_per_token":3,"output_micro_per_token":15}`
	if body := strings.TrimSpace(got.Body.String()); body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	if fake.gotID != "pv-seed-1" {
		t.Errorf("the store was asked for %q", fake.gotID)
	}
}

func TestPriceVersionMapsStoreErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", store.ErrPriceVersionNotFound, http.StatusNotFound, CodePriceVersionNotFound},
		{"database error", fmt.Errorf("%w: %w", store.ErrUnavailable, errors.New("closed pool")), http.StatusServiceUnavailable, CodeDependencyUnavailable},
		{"anything else", errors.New("unexpected"), http.StatusInternalServerError, CodeInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, _ := newTestServer(&fakeStore{err: tt.err}, testToken)
			wantError(t, call(handler, "GET", "/internal/v1/price-versions/pv-1", "", authorized()), tt.status, tt.code)
		})
	}
}

func TestPriceVersionIDPostgresCannotHoldIsNotFound(t *testing.T) {
	fake := &fakeStore{}
	handler, _ := newTestServer(fake, testToken)
	for _, path := range []string{"/internal/v1/price-versions/pv%00", "/internal/v1/price-versions/pv%ff"} {
		wantError(t, call(handler, "GET", path, "", authorized()), http.StatusNotFound, CodePriceVersionNotFound)
	}
	if fake.calls != 0 {
		t.Error("the store was asked for an id Postgres cannot hold")
	}
}

// secretsOf lists the forms in which a verify call's secrets could reach a log.
func secretsOf(key string, pepper []byte, token string) []string {
	mac := apikey.HMAC(pepper, key)
	return []string{
		key,
		token,
		hex.EncodeToString(mac),
		base64.StdEncoding.EncodeToString(mac),
		base64.RawURLEncoding.EncodeToString(mac),
	}
}

func TestLogLineHoldsTheFieldsAndNoSecret(t *testing.T) {
	const model = "mock-model-in-the-body"
	tests := []struct {
		name    string
		store   *fakeStore
		headers map[string]string
		status  int
		ids     bool
	}{
		{"verified", &fakeStore{key: store.VerifiedKey{TenantID: "tenant-01", KeyID: "key-tenant-01"}}, authorized(), 200, true},
		{"model not allowed", &fakeStore{key: store.VerifiedKey{TenantID: "tenant-01", KeyID: "key-tenant-01"}, err: store.ErrModelNotAllowed}, authorized(), 403, true},
		{"unknown key", &fakeStore{err: store.ErrInvalidKey}, authorized(), 401, false},
		{"database error", &fakeStore{err: store.ErrUnavailable}, authorized(), 503, false},
		{"wrong token", &fakeStore{}, map[string]string{"Authorization": "Bearer wrong-" + testToken}, 401, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, logs := newTestServer(tt.store, testToken)
			tt.headers["X-Request-Id"] = "req_log-test"
			call(handler, "POST", "/internal/v1/keys/verify?debug="+testKey, verifyBody(testKey, model), tt.headers)

			var line map[string]any
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
				t.Fatalf("want one JSON log line, got %q: %v", logs, err)
			}
			want := map[string]any{
				"request_id": "req_log-test",
				"method":     "POST",
				"route":      "POST /internal/v1/keys/verify",
				"status":     float64(tt.status),
			}
			for name, value := range want {
				if line[name] != value {
					t.Errorf("log %s = %v, want %v", name, line[name], value)
				}
			}
			if _, ok := line["duration_ms"].(float64); !ok {
				t.Errorf("log duration_ms = %v, want a number", line["duration_ms"])
			}
			_, hasKey := line["key_id"]
			_, hasTenant := line["tenant_id"]
			if hasKey != tt.ids || hasTenant != tt.ids {
				t.Errorf("log has key_id %v and tenant_id %v, want both %v", hasKey, hasTenant, tt.ids)
			}
			if tt.ids && (line["key_id"] != "key-tenant-01" || line["tenant_id"] != "tenant-01") {
				t.Errorf("log key_id %v, tenant_id %v", line["key_id"], line["tenant_id"])
			}
			for _, secret := range append(secretsOf(testKey, []byte(testPepper), testToken), model, "Bearer") {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("the log holds %q: %s", secret, logs)
				}
			}
		})
	}
}
