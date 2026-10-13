package accessclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
)

const (
	token     = "access-token-canary-11aa"
	requestID = "req_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e34"
	key       = "gf_example_only_not_a_real_key"
)

// The credential-channel example of api/access.yaml.
const verifyExample = `{"tenant_id":"tenant-02","key_id":"key-tenant-02","price_version_id":"pv-seed-1",` +
	`"channel":{"id":"hosted","protocol":"anthropic-messages","base_url":"https://api.example.com",` +
	`"credential_env":"UPSTREAM_CREDENTIAL_HOSTED"}}`

func client(t *testing.T, handler http.HandlerFunc) *accessclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return accessclient.New(internalapi.NewCaller(server.URL, token))
}

func TestVerifySendsKeyAndModel(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/keys/verify" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Request-Id") != requestID {
			t.Error("missing the token or the request id")
		}
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 2 || body["key"] != key || body["model"] != "mock-small" {
			t.Errorf("body = %s", data)
		}
		_, _ = io.WriteString(w, verifyExample)
	})
	got, err := c.Verify(context.Background(), requestID, key, "mock-small")
	if err != nil {
		t.Fatal(err)
	}
	want := accessclient.Verified{
		TenantID:       "tenant-02",
		KeyID:          "key-tenant-02",
		PriceVersionID: "pv-seed-1",
		Channel: accessclient.Channel{
			ID:            "hosted",
			Protocol:      "anthropic-messages",
			BaseURL:       "https://api.example.com",
			CredentialEnv: "UPSTREAM_CREDENTIAL_HOSTED",
		},
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestVerifyReturnsTheErrorCode(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"model_not_allowed","message":"The key may not use this model.",`+
			`"request_id":"`+requestID+`"}}`)
	})
	_, err := c.Verify(context.Background(), requestID, key, "mock-large")
	if internalapi.Code(err) != accessclient.CodeModelNotAllowed {
		t.Errorf("err = %v, want code model_not_allowed", err)
	}
}

func TestVerifyRefusesAnAnswerWithoutTenantOrPriceVersion(t *testing.T) {
	for _, body := range []string{
		`{"tenant_id":"","price_version_id":"pv-seed-1","channel":{}}`,
		`{"tenant_id":"tenant-01","channel":{}}`,
	} {
		c := client(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		})
		if _, err := c.Verify(context.Background(), requestID, key, "mock-small"); !errors.Is(err, internalapi.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", body, err)
		}
	}
}
