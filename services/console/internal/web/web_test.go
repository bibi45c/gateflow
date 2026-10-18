package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/services/console/internal/backend"
	"github.com/bibi45c/gateflow/services/console/internal/session"
)

// fakeBackend records the tenants it is asked about.
type fakeBackend struct {
	mu       sync.Mutex
	asked    []string
	adjusted []string
	revoked  []string
}

func (f *fakeBackend) ask(tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, tenant)
}

func (f *fakeBackend) Login(_ context.Context, username, password string) (backend.User, error) {
	tenant := "tenant-demo"
	switch {
	case username == "demo" && password == "pw":
		return backend.User{ID: "user-demo", Username: "demo", Role: "user", TenantID: &tenant}, nil
	case username == "admin" && password == "pw":
		return backend.User{ID: "user-admin", Username: "admin", Role: "admin"}, nil
	}
	return backend.User{}, &backend.Error{Status: 401, Code: "invalid_credentials"}
}
func (f *fakeBackend) Tenants(context.Context) ([]backend.Tenant, error) {
	return []backend.Tenant{{ID: "tenant-01", Name: "tenant-01"}, {ID: "tenant-demo", Name: "Demo"}}, nil
}
func (f *fakeBackend) Keys(_ context.Context, tenant string) ([]backend.Key, error) {
	f.ask(tenant)
	return []backend.Key{{ID: "key_1", TenantID: tenant, Name: "laptop", Prefix: "gf_abcdefg"}}, nil
}
func (f *fakeBackend) CreateKey(_ context.Context, tenant, name string) (backend.Key, error) {
	f.ask(tenant)
	return backend.Key{ID: "key_2", TenantID: tenant, Name: name, Key: "gf_SECRETKEYVALUE"}, nil
}
func (f *fakeBackend) RevokeKey(_ context.Context, tenant, key string) error {
	f.ask(tenant)
	f.revoked = append(f.revoked, tenant+"/"+key)
	return nil
}
func (f *fakeBackend) Channels(context.Context) ([]backend.Channel, error) {
	return []backend.Channel{{ID: "mock", Protocol: "anthropic-messages", Models: []string{"mock-*"}}}, nil
}
func (f *fakeBackend) PriceVersions(context.Context) ([]backend.PriceVersion, error) {
	return []backend.PriceVersion{{ID: "pv-seed-1", InputMicroPerToken: 3, OutputMicroPerToken: 15}}, nil
}
func (f *fakeBackend) Wallets(context.Context) ([]backend.Wallet, error) {
	return []backend.Wallet{{TenantID: "tenant-01", Balance: 100_000_000, Available: 100_000_000}}, nil
}
func (f *fakeBackend) Usage(_ context.Context, tenant string, _ time.Time, _ int) (backend.Usage, error) {
	f.ask(tenant)
	model := "mock-small"
	charged := int64(1234)
	u := backend.Usage{Wallet: backend.Wallet{TenantID: tenant, Balance: 5_000_000, Available: 4_500_000}}
	u.Summary.Requests, u.Summary.Charged = 3, 1234
	u.Items = []backend.UsageItem{{CallID: "req_1", Model: &model, Status: "settled", Charged: &charged, CreatedAt: time.Now()}}
	return u, nil
}
func (f *fakeBackend) Adjust(_ context.Context, tenant, id string, amount int64, note string) (backend.Adjustment, error) {
	f.adjusted = append(f.adjusted, tenant)
	if amount < -100 {
		return backend.Adjustment{}, &backend.Error{Status: 409, Code: "insufficient_balance"}
	}
	return backend.Adjustment{ID: id, TenantID: tenant, Amount: amount, Note: note, BalanceAfter: 10_000_000 + amount}, nil
}

type harness struct {
	t        *testing.T
	server   *httptest.Server
	fake     *fakeBackend
	sessions *session.Manager
}

func newHarness(t *testing.T, opts Options) *harness {
	fake := &fakeBackend{}
	sessions := session.New([]byte("test-secret-0123456789"), false)
	mux := http.NewServeMux()
	New(fake, sessions, opts, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &harness{t: t, server: server, fake: fake, sessions: sessions}
}

// as returns a cookie and CSRF token for u.
func (h *harness) as(u session.User) (*http.Cookie, string) {
	rec := httptest.NewRecorder()
	h.sessions.Start(rec, u)
	cookie := rec.Result().Cookies()[0]
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	read, _ := h.sessions.Read(r)
	return cookie, h.sessions.CSRFToken(read)
}

var (
	demo  = session.User{ID: "user-demo", Username: "demo", Role: session.RoleUser, TenantID: "tenant-demo"}
	admin = session.User{ID: "user-admin", Username: "admin", Role: session.RoleAdmin}
)

func (h *harness) do(method, path string, cookie *http.Cookie, form url.Values) (*http.Response, string) {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.server.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func TestEveryPageRendersForItsRole(t *testing.T) {
	h := newHarness(t, Options{PublicGatewayURL: "http://localhost:8080"})
	userCookie, _ := h.as(demo)
	for path, want := range map[string]string{
		"/": "$4.500000", "/keys": "gf_abcdefg", "/usage": "mock-small", "/models": "$15 / 1M tokens",
		"/quickstart": "http://localhost:8080/v1", "/playground": `value="mock-chat"`,
	} {
		resp, body := h.do("GET", path, userCookie, nil)
		if resp.StatusCode != 200 || !strings.Contains(body, want) {
			t.Errorf("user %s: %d, want %q in the page", path, resp.StatusCode, want)
		}
	}
	adminCookie, _ := h.as(admin)
	for path, want := range map[string]string{
		"/admin/tenants": "$100.000000", "/admin/catalog": "pv-seed-1", "/admin/dashboard": "Gateway replicas",
	} {
		resp, body := h.do("GET", path, adminCookie, nil)
		if resp.StatusCode != 200 || !strings.Contains(body, want) {
			t.Errorf("admin %s: %d, want %q in the page", path, resp.StatusCode, want)
		}
	}
	for _, f := range h.fake.asked {
		if f != "tenant-demo" {
			t.Errorf("asked about %s for the demo user", f)
		}
	}
}

func TestMockModelsAreListedWhenAChannelServesThem(t *testing.T) {
	h := newHarness(t, Options{})
	cookie, _ := h.as(demo)
	_, body := h.do("GET", "/models", cookie, nil)
	if !strings.Contains(body, "<code>mock-chat</code>") || !strings.Contains(body, "Mock models") {
		t.Error("the models page does not list mock-chat")
	}
	_, body = h.do("GET", "/playground", cookie, nil)
	if !strings.Contains(body, `<option value="mock-chat">`) {
		t.Error("the playground does not offer mock-chat")
	}
	if servedMocks([]modelRow{{Pattern: "qwen3:8b"}}) != nil {
		t.Error("mock models listed without a mock channel")
	}
}

func TestRolesAndSignIn(t *testing.T) {
	h := newHarness(t, Options{})
	if resp, _ := h.do("GET", "/keys", nil, nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	userCookie, _ := h.as(demo)
	if resp, _ := h.do("GET", "/admin/tenants", userCookie, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("user on admin page: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/admin/dashboard/data", userCookie, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("user on dashboard data: %d", resp.StatusCode)
	}
	resp, body := h.do("POST", "/login", nil, url.Values{"username": {"demo"}, "password": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "wrong") {
		t.Errorf("bad password: %d", resp.StatusCode)
	}
	resp, _ = h.do("POST", "/login", nil, url.Values{"username": {"demo"}, "password": {"pw"}})
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 || !resp.Cookies()[0].HttpOnly {
		t.Errorf("sign-in: %d %v", resp.StatusCode, resp.Cookies())
	}
}

func TestPostsNeedTheCSRFTokenAndStayInTheTenant(t *testing.T) {
	h := newHarness(t, Options{})
	cookie, token := h.as(demo)
	if resp, _ := h.do("POST", "/keys", cookie, url.Values{"name": {"x"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF: %d", resp.StatusCode)
	}
	resp, body := h.do("POST", "/keys", cookie, url.Values{"name": {"laptop"}, "csrf": {token}})
	if resp.StatusCode != 200 || !strings.Contains(body, "gf_SECRETKEYVALUE") || !strings.Contains(body, "shown only once") {
		t.Errorf("create: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/keys", cookie, nil); resp.StatusCode != 200 {
		t.Errorf("list: %d", resp.StatusCode)
	} else if _, body := h.do("GET", "/keys", cookie, nil); strings.Contains(body, "gf_SECRETKEYVALUE") {
		t.Error("the raw key shows again")
	}
	h.do("POST", "/keys/key_1/revoke", cookie, url.Values{"csrf": {token}})
	if len(h.fake.revoked) != 1 || h.fake.revoked[0] != "tenant-demo/key_1" {
		t.Errorf("revoked %v", h.fake.revoked)
	}
	// An admin token does not work for the user's session.
	_, adminToken := h.as(admin)
	if resp, _ := h.do("POST", "/keys", cookie, url.Values{"csrf": {adminToken}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign CSRF: %d", resp.StatusCode)
	}
}

func TestAdminAdjusts(t *testing.T) {
	h := newHarness(t, Options{})
	cookie, token := h.as(admin)
	form := url.Values{"csrf": {token}, "adjustment_id": {"adj_1"}, "amount": {"10.5"}}
	resp, _ := h.do("POST", "/admin/tenants/tenant-01/adjust", cookie, form)
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "done=") {
		t.Errorf("adjust: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for amount, status := range map[string]int{"abc": 400, "0": 400, "1.0000001": 400, "-5": 409} {
		form.Set("amount", amount)
		if resp, _ := h.do("POST", "/admin/tenants/tenant-01/adjust", cookie, form); resp.StatusCode != status {
			t.Errorf("amount %q: %d, want %d", amount, resp.StatusCode, status)
		}
	}
	form.Set("amount", "1")
	if resp, _ := h.do("POST", "/admin/tenants/nobody/adjust", cookie, form); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tenant: %d", resp.StatusCode)
	}
}

func TestParseUSD(t *testing.T) {
	for in, want := range map[string]int64{"10": 10_000_000, "-2.5": -2_500_000, "0.000001": 1, "$3": 3_000_000, ".5": 500_000} {
		if got, err := parseUSD(in); err != nil || got != want {
			t.Errorf("parseUSD(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "1.2.3", "1e3", "0.0000001", "99999999999999"} {
		if _, err := parseUSD(in); err == nil {
			t.Errorf("parseUSD(%q) accepted", in)
		}
	}
	if usd(-1_234_567) != "-$1.234567" {
		t.Error(usd(-1_234_567))
	}
}

func TestPlaygroundRelaysTheGatewayStream(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "gf_user" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
	}))
	defer gateway.Close()
	h := newHarness(t, Options{GatewayURL: gateway.URL})
	cookie, token := h.as(demo)
	send := func(body string, csrf string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", h.server.URL+"/playground/stream", strings.NewReader(body))
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, string(raw)
	}
	resp, body := send(`{"key":"gf_user","model":"mock-small","prompt":"hello"}`, token)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(body, `"text":"hi"`) {
		t.Errorf("stream: %d %q", resp.StatusCode, body)
	}
	if resp, body := send(`{"key":"gf_wrong","model":"mock-small","prompt":"hello"}`, token); resp.StatusCode != 401 || !strings.Contains(body, "authentication_error") {
		t.Errorf("bad key: %d %q", resp.StatusCode, body)
	}
	if resp, _ := send(`{"key":"gf_user","model":"mock-small","prompt":""}`, token); resp.StatusCode != 400 {
		t.Errorf("empty prompt: %d", resp.StatusCode)
	}
	if resp, _ := send(`{"key":"gf_user","model":"mock-small","prompt":"x"}`, "nope"); resp.StatusCode != 403 {
		t.Errorf("no CSRF: %d", resp.StatusCode)
	}
}

func TestDashboardDataSumsPrometheus(t *testing.T) {
	var queries []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("query"))
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"2.5"]},{"metric":{},"value":[1,"0.5"]}]}}`)
	}))
	defer prom.Close()
	h := newHarness(t, Options{PrometheusURL: prom.URL, GatewayJob: "gateflow-gateway"})
	cookie, _ := h.as(admin)
	resp, body := h.do("GET", "/admin/dashboard/data", cookie, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `"replicas":3`) || !strings.Contains(body, `"active_streams":3`) {
		t.Errorf("data: %d %s", resp.StatusCode, body)
	}
	if len(queries) != 3 || !strings.Contains(strings.Join(queries, " "), `job="gateflow-gateway"`) {
		t.Errorf("queries %v", queries)
	}
	h2 := newHarness(t, Options{})
	cookie2, _ := h2.as(admin)
	if _, body := h2.do("GET", "/admin/dashboard/data", cookie2, nil); !strings.Contains(body, "PROMETHEUS_URL") {
		t.Errorf("no Prometheus: %s", body)
	}
}
