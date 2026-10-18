package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/pgtest"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
	"github.com/bibi45c/gateflow/services/access/internal/store"
	"github.com/bibi45c/gateflow/services/access/migrations"
)

// consoleSeeded is the API with console routes on a seeded database that also holds
// the console users admin and demo.
func consoleSeeded(t *testing.T) http.Handler {
	t.Helper()
	pool := pgtest.Migrated(t, "access", store.Schema, migrations.FS)
	cfg := config.Seed{
		KeyPepper: []byte(testPepper), SeedSecret: []byte("test-seed-secret"),
		MockBaseURL: "http://mock-upstream:8090", OllamaBaseURL: "http://host.docker.internal:11434",
		OllamaModel: ollamaModel, AdminPassword: "admin-pass", DemoPassword: "demo-pass",
	}
	if _, err := seed.Run(context.Background(), pool, cfg, 2); err != nil {
		t.Fatal(err)
	}
	users, err := seed.Console(context.Background(), pool, cfg)
	if err != nil || len(users) != 2 {
		t.Fatalf("console seed: %v, %v", users, err)
	}
	// Seeding again keeps one user per name.
	if _, err := seed.Console(context.Background(), pool, cfg); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	mux := http.NewServeMux()
	New(s, []byte(testPepper), testToken, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil).WithConsole(s).Register(mux)
	return mux
}

func decode[T any](t *testing.T, got *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var body T
	if got.Code != status {
		t.Fatalf("status %d, want %d: %s", got.Code, status, got.Body)
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestConsoleLogin(t *testing.T) {
	h := consoleSeeded(t)
	demo := decode[userResponse](t, call(h, "POST", loginRoute, `{"username":"demo","password":"demo-pass"}`, authorized()), 200)
	if demo.Role != "user" || demo.TenantID == nil || *demo.TenantID != seed.DemoTenantID {
		t.Errorf("demo = %+v", demo)
	}
	admin := decode[userResponse](t, call(h, "POST", loginRoute, `{"username":"admin","password":"admin-pass"}`, authorized()), 200)
	if admin.Role != "admin" || admin.TenantID != nil {
		t.Errorf("admin = %+v", admin)
	}
	for _, body := range []string{`{"username":"demo","password":"wrong"}`, `{"username":"nobody","password":"demo-pass"}`} {
		wantError(t, call(h, "POST", loginRoute, body, authorized()), 401, CodeInvalidCredentials)
	}
	for _, body := range []string{`{}`, `{"username":"demo"}`, `{"username":"demo","password":"x","extra":1}`, `[]`} {
		wantError(t, call(h, "POST", loginRoute, body, authorized()), 400, CodeInvalidRequest)
	}
	wantError(t, call(h, "POST", loginRoute, `{"username":"demo","password":"demo-pass"}`, nil), 401, CodeUnauthorized)
}

func TestConsoleKeyLifecycle(t *testing.T) {
	h := consoleSeeded(t)
	path := "/internal/v1/tenants/" + seed.DemoTenantID + "/keys"
	created := decode[keyResponse](t, call(h, "POST", path, `{"name":"laptop"}`, authorized()), 201)
	if !strings.HasPrefix(created.Key, "gf_") || created.Prefix != created.Key[:keyPrefixLen] ||
		created.Name != "laptop" || created.PriceVersionID != seed.PriceVersionID || created.RevokedAt != nil {
		t.Fatalf("created = %+v", created)
	}
	// The new key verifies for the seeded channels.
	if got := call(h, "POST", verifyRoute, verifyBody(created.Key, "mock-fast"), authorized()); got.Code != 200 {
		t.Fatalf("verify new key: %d %s", got.Code, got.Body)
	}
	list := decode[struct{ Items []keyResponse }](t, call(h, "GET", path, "", authorized()), 200)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID || list.Items[0].Key != "" {
		t.Fatalf("list = %+v", list)
	}
	revoked := decode[keyResponse](t, call(h, "POST", path+"/"+created.ID+"/revoke", "", authorized()), 200)
	if revoked.RevokedAt == nil {
		t.Fatal("revoked key has no revoked_at")
	}
	again := decode[keyResponse](t, call(h, "POST", path+"/"+created.ID+"/revoke", "", authorized()), 200)
	if !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Error("a second revoke changed revoked_at")
	}
	wantError(t, call(h, "POST", verifyRoute, verifyBody(created.Key, "mock-fast"), authorized()), 401, CodeInvalidKey)
	// Another tenant's path cannot revoke it, and unknown tenants are refused.
	wantError(t, call(h, "POST", "/internal/v1/tenants/tenant-01/keys/"+created.ID+"/revoke", "", authorized()), 404, CodeKeyNotFound)
	wantError(t, call(h, "GET", "/internal/v1/tenants/nobody/keys", "", authorized()), 404, CodeTenantNotFound)
	wantError(t, call(h, "POST", "/internal/v1/tenants/nobody/keys", `{"name":""}`, authorized()), 404, CodeTenantNotFound)
	wantError(t, call(h, "POST", path, `{"name":"bad\nname"}`, authorized()), 400, CodeInvalidRequest)
}

func TestConsoleCatalogAndTenants(t *testing.T) {
	h := consoleSeeded(t)
	tenants := decode[struct{ Items []tenantResponse }](t, call(h, "GET", tenantsRoute, "", authorized()), 200)
	var ids []string
	for _, tn := range tenants.Items {
		ids = append(ids, tn.ID)
	}
	if strings.Join(ids, ",") != "tenant-00,tenant-01,tenant-02,tenant-demo" || tenants.Items[1].ActiveKeys != 1 {
		t.Errorf("tenants = %+v", tenants.Items)
	}
	channels := decode[struct{ Items []channelInfoResponse }](t, call(h, "GET", channelsRoute, "", authorized()), 200)
	if len(channels.Items) != 4 || channels.Items[0].ID != "mock" || channels.Items[0].Models[0] != "mock-*" {
		t.Errorf("channels = %+v", channels.Items)
	}
	prices := decode[struct{ Items []priceVersionInfoResponse }](t, call(h, "GET", priceVersionsRoute, "", authorized()), 200)
	if len(prices.Items) != 1 || prices.Items[0].ID != seed.PriceVersionID || prices.Items[0].OutputMicroPerToken != 15 {
		t.Errorf("price versions = %+v", prices.Items)
	}
	// The single price version route is still served.
	if got := call(h, "GET", "/internal/v1/price-versions/"+seed.PriceVersionID, "", authorized()); got.Code != 200 {
		t.Errorf("price version: %d", got.Code)
	}
}
