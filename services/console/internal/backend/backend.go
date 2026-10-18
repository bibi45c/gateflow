// Package backend calls the internal APIs of access and metering for the console:
// JSON over HTTP with the internal bearer token, as api/access.yaml and
// api/metering.yaml describe. Its errors hold no token, password or key.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Timeout bounds one internal call. Sign-in hashes a password, so it gets longer.
const Timeout = 5 * time.Second

const maxReplyBytes = 1 << 20

// Error is an error reply of an internal API.
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("backend: status %d, code %q", e.Status, e.Code)
}

// Code returns the error code of err, or "" when err is no error reply.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// service calls one internal API.
type service struct {
	base   string
	token  string
	client *http.Client
}

func (s service) do(ctx context.Context, method, path string, body, reply any) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("backend: %s %s: unavailable", method, path)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxReplyBytes))
	if err != nil {
		return fmt.Errorf("backend: %s %s: reply unreadable", method, path)
	}
	if response.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &Error{Status: response.StatusCode, Code: e.Error.Code}
	}
	if reply == nil {
		return nil
	}
	if err := json.Unmarshal(raw, reply); err != nil {
		return fmt.Errorf("backend: %s %s: malformed reply", method, path)
	}
	return nil
}

// Client calls access and metering.
type Client struct {
	access   service
	metering service
}

// New returns a client of access at accessURL and metering at meteringURL.
func New(accessURL, accessToken, meteringURL, meteringToken string) *Client {
	client := &http.Client{}
	return &Client{
		access:   service{base: accessURL, token: accessToken, client: client},
		metering: service{base: meteringURL, token: meteringToken, client: client},
	}
}

// User is a signed-in console user.
type User struct {
	ID       string  `json:"user_id"`
	Username string  `json:"username"`
	Role     string  `json:"role"`
	TenantID *string `json:"tenant_id"`
}

// Tenant is a tenant as access lists it.
type Tenant struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	ActiveKeys int       `json:"active_keys"`
	CreatedAt  time.Time `json:"created_at"`
}

// Key is a gateway key; Key is set only in the answer that created it.
type Key struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	Name           string     `json:"name"`
	Prefix         string     `json:"prefix"`
	PriceVersionID string     `json:"price_version_id"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	Key            string     `json:"key"`
}

// Channel is an upstream channel and its model patterns.
type Channel struct {
	ID       string   `json:"id"`
	Protocol string   `json:"protocol"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
}

// PriceVersion is a pair of prices in micro-units per token.
type PriceVersion struct {
	ID                  string    `json:"id"`
	InputMicroPerToken  int64     `json:"input_micro_per_token"`
	OutputMicroPerToken int64     `json:"output_micro_per_token"`
	CreatedAt           time.Time `json:"created_at"`
}

// Wallet is a tenant's balance in micro-units.
type Wallet struct {
	TenantID  string    `json:"tenant_id"`
	Balance   int64     `json:"balance"`
	Held      int64     `json:"held"`
	Available int64     `json:"available"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Usage is a tenant's wallet, summary and latest calls.
type Usage struct {
	Wallet  Wallet `json:"wallet"`
	Summary struct {
		Since        time.Time `json:"since"`
		Requests     int64     `json:"requests"`
		Charged      int64     `json:"charged"`
		InputTokens  int64     `json:"input_tokens"`
		OutputTokens int64     `json:"output_tokens"`
	} `json:"summary"`
	Items []UsageItem `json:"items"`
}

// UsageItem is one call attempt.
type UsageItem struct {
	CallID       string     `json:"call_id"`
	Model        *string    `json:"model"`
	Status       string     `json:"status"`
	Outcome      *string    `json:"outcome"`
	InputTokens  *int64     `json:"input_tokens"`
	OutputTokens *int64     `json:"output_tokens"`
	MaxCost      int64      `json:"max_cost"`
	Charged      *int64     `json:"charged"`
	CreatedAt    time.Time  `json:"created_at"`
	SettledAt    *time.Time `json:"settled_at"`
}

// Adjustment is a recorded top-up or correction.
type Adjustment struct {
	ID           string `json:"adjustment_id"`
	TenantID     string `json:"tenant_id"`
	Amount       int64  `json:"amount"`
	Note         string `json:"note"`
	BalanceAfter int64  `json:"balance_after"`
}

type list[T any] struct {
	Items []T `json:"items"`
}

func esc(s string) string { return url.PathEscape(s) }

// Login checks a username and password.
func (c *Client) Login(ctx context.Context, username, password string) (User, error) {
	var u User
	err := c.access.do(ctx, http.MethodPost, "/internal/v1/console/login",
		map[string]string{"username": username, "password": password}, &u)
	return u, err
}

// Tenants lists every tenant.
func (c *Client) Tenants(ctx context.Context) ([]Tenant, error) {
	var l list[Tenant]
	err := c.access.do(ctx, http.MethodGet, "/internal/v1/tenants", nil, &l)
	return l.Items, err
}

// Keys lists a tenant's keys.
func (c *Client) Keys(ctx context.Context, tenantID string) ([]Key, error) {
	var l list[Key]
	err := c.access.do(ctx, http.MethodGet, "/internal/v1/tenants/"+esc(tenantID)+"/keys", nil, &l)
	return l.Items, err
}

// CreateKey creates a key; the answer holds the raw key.
func (c *Client) CreateKey(ctx context.Context, tenantID, name string) (Key, error) {
	var k Key
	err := c.access.do(ctx, http.MethodPost, "/internal/v1/tenants/"+esc(tenantID)+"/keys",
		map[string]string{"name": name}, &k)
	return k, err
}

// RevokeKey revokes a tenant's key.
func (c *Client) RevokeKey(ctx context.Context, tenantID, keyID string) error {
	return c.access.do(ctx, http.MethodPost,
		"/internal/v1/tenants/"+esc(tenantID)+"/keys/"+esc(keyID)+"/revoke", nil, nil)
}

// Channels lists every channel.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var l list[Channel]
	err := c.access.do(ctx, http.MethodGet, "/internal/v1/channels", nil, &l)
	return l.Items, err
}

// PriceVersions lists every price version, newest first.
func (c *Client) PriceVersions(ctx context.Context) ([]PriceVersion, error) {
	var l list[PriceVersion]
	err := c.access.do(ctx, http.MethodGet, "/internal/v1/price-versions", nil, &l)
	return l.Items, err
}

// Wallets lists every wallet.
func (c *Client) Wallets(ctx context.Context) ([]Wallet, error) {
	var l list[Wallet]
	err := c.metering.do(ctx, http.MethodGet, "/internal/v1/wallets", nil, &l)
	return l.Items, err
}

// Usage returns a tenant's usage since since, with at most limit calls.
func (c *Client) Usage(ctx context.Context, tenantID string, since time.Time, limit int) (Usage, error) {
	var u Usage
	query := url.Values{"since": {since.Format(time.RFC3339)}, "limit": {fmt.Sprint(limit)}}
	err := c.metering.do(ctx, http.MethodGet, "/internal/v1/tenants/"+esc(tenantID)+"/usage?"+query.Encode(), nil, &u)
	return u, err
}

// Adjust tops up (amount > 0) or corrects a tenant's balance once per id.
func (c *Client) Adjust(ctx context.Context, tenantID, id string, amount int64, note string) (Adjustment, error) {
	var a Adjustment
	err := c.metering.do(ctx, http.MethodPost, "/internal/v1/wallets/"+esc(tenantID)+"/adjustments",
		map[string]any{"adjustment_id": id, "amount": amount, "note": note}, &a)
	return a, err
}
