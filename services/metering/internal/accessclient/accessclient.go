// Package accessclient fetches price versions from the access service's internal API.
package accessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bibi45c/gateflow/services/metering/internal/billing"
)

// Timeout bounds one price lookup, so a slow access never holds up a reserve for long.
const Timeout = time.Second

// ErrUnavailable means access gave no answer in time or failed: it timed out, could not
// be reached, or answered with a 5xx. The caller may retry.
var ErrUnavailable = errors.New("access is unavailable")

// ErrRejected means access answered, but the lookup cannot succeed as asked: any other
// status, such as 404 for an unknown price version or 401 for a refused internal
// token, or an answer that is not a valid price version.
var ErrRejected = errors.New("access rejected the price lookup")

// Client looks up price versions and remembers them. A price version never changes,
// so an entry is kept for the life of the process; there are only a few.
type Client struct {
	baseURL string
	token   string
	http    *http.Client

	mu     sync.Mutex
	prices map[string]billing.Prices
}

// New returns a client for the access service at baseURL, such as
// "http://access:8081", which sends token as its bearer token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: Timeout},
		prices:  map[string]billing.Prices{},
	}
}

// PriceVersion returns the prices of the price version id. requestID is passed on as
// X-Request-Id. The error wraps ErrUnavailable or ErrRejected.
func (c *Client) PriceVersion(ctx context.Context, id, requestID string) (billing.Prices, error) {
	c.mu.Lock()
	prices, cached := c.prices[id]
	c.mu.Unlock()
	if cached {
		return prices, nil
	}
	prices, err := c.fetch(ctx, id, requestID)
	if err != nil {
		return billing.Prices{}, err
	}
	c.mu.Lock()
	c.prices[id] = prices
	c.mu.Unlock()
	return prices, nil
}

func (c *Client) fetch(ctx context.Context, id, requestID string) (billing.Prices, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	target := c.baseURL + "/internal/v1/price-versions/" + url.PathEscape(id)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return billing.Prices{}, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if requestID != "" {
		request.Header.Set("X-Request-Id", requestID)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return billing.Prices{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()

	switch status := response.StatusCode; {
	case status == http.StatusOK:
		return decode(response.Body, id)
	case status >= 500:
		return billing.Prices{}, fmt.Errorf("%w: status %d", ErrUnavailable, status)
	default:
		return billing.Prices{}, fmt.Errorf("%w: status %d", ErrRejected, status)
	}
}

// decode reads a price version and checks it, so a wrong or partial answer is never
// used to compute a charge.
func decode(body io.Reader, id string) (billing.Prices, error) {
	var version struct {
		ID     *string `json:"id"`
		Input  *int64  `json:"input_micro_per_token"`
		Output *int64  `json:"output_micro_per_token"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&version); err != nil {
		return billing.Prices{}, fmt.Errorf("%w: unreadable body: %v", ErrRejected, err)
	}
	if version.ID == nil || *version.ID != id || version.Input == nil || version.Output == nil {
		return billing.Prices{}, fmt.Errorf("%w: incomplete price version", ErrRejected)
	}
	prices := billing.Prices{Input: *version.Input, Output: *version.Output}
	if !prices.Valid() {
		return billing.Prices{}, fmt.Errorf("%w: price outside 0 to %d", ErrRejected, billing.MaxPrice)
	}
	return prices, nil
}
