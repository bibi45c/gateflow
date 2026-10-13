// Package internalapi sends the gateway's calls to the internal APIs of access and
// metering: JSON over HTTP with an internal bearer token and the gateway request id,
// answered on error with the body of api/errors.md. It never logs, and its errors
// hold no token, key or body.
package internalapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// DefaultTimeout bounds one call. Issue #18 gives verify, reserve and settle 2 s each.
const DefaultTimeout = 2 * time.Second

// maxReplyBytes caps the reply read from an internal service; every reply of the
// contracts is far smaller.
const maxReplyBytes = 64 << 10

// CodeDependencyUnavailable is the error code access and metering both return when
// a dependency of theirs is unavailable.
const CodeDependencyUnavailable = "dependency_unavailable"

// ErrUnavailable reports a call that got no answer: the service could not be
// reached, did not answer in time, or its reply could not be read.
var ErrUnavailable = errors.New("internalapi: service unavailable")

// ErrMalformed reports a success reply whose body does not hold what the contract
// says.
var ErrMalformed = errors.New("internalapi: malformed success reply")

// Error is an error reply: its status and the error.code of its body, which is ""
// when the body has no readable code.
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("internalapi: status %d, code %q", e.Status, e.Code)
}

// Code returns the error.code of err, or "" when err is not an error reply.
func Code(err error) string {
	var reply *Error
	if errors.As(err, &reply) {
		return reply.Code
	}
	return ""
}

// Retryable reports whether a call that failed with err may be sent again
// unchanged: after a timeout, a connection error or a 5xx reply.
func Retryable(err error) bool {
	var reply *Error
	return errors.Is(err, ErrUnavailable) || errors.As(err, &reply) && reply.Status >= 500
}

// Caller sends calls to one internal service.
type Caller struct {
	// Timeout bounds each call, from sending the request to reading the reply.
	Timeout time.Duration

	baseURL string
	token   string
	client  *http.Client
}

// NewCaller returns a Caller of the service at baseURL (scheme://host[:port]) that
// sends token as its bearer token, with DefaultTimeout.
func NewCaller(baseURL, token string) *Caller {
	return &Caller{
		Timeout: DefaultTimeout,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  newClient(),
	}
}

// newClient returns a client that uses no proxy and follows no redirect, so the
// token only goes to the configured address.
func newClient() *http.Client {
	dialer := &net.Dialer{Timeout: DefaultTimeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Post sends in as JSON to path, with requestID as X-Request-Id, and decodes into
// out a reply whose status is one of ok. A reply with any other status is an *Error;
// no reply, or one that cannot be read, wraps ErrUnavailable; a success reply that
// does not decode is ErrMalformed.
func (c *Caller) Post(ctx context.Context, path, requestID string, in, out any, ok ...int) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("internalapi: encoding the request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("internalapi: building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Request-Id", requestID)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes+1))
	if err != nil {
		return fmt.Errorf("%w: reading the reply: %w", ErrUnavailable, err)
	}
	if !slices.Contains(ok, resp.StatusCode) {
		return &Error{Status: resp.StatusCode, Code: errorCode(data)}
	}
	if len(data) > maxReplyBytes || json.Unmarshal(data, out) != nil {
		return ErrMalformed
	}
	return nil
}

// errorCode returns the error.code of an error body, or "" when it has none.
func errorCode(data []byte) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	return body.Error.Code
}
