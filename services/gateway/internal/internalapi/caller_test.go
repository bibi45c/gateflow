package internalapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testToken     = "internal-token-canary-5e1d"
	testRequestID = "req_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e34"
)

type pair struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// serve starts a stand-in service and returns a caller of it with a short timeout.
func serve(t *testing.T, handler http.HandlerFunc) *Caller {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	caller := NewCaller(server.URL+"/", testToken)
	caller.Timeout = 200 * time.Millisecond
	return caller
}

func TestNewCallerTimesOutAfterTwoSeconds(t *testing.T) {
	if got := NewCaller("http://access:8081", testToken).Timeout; got != 2*time.Second {
		t.Errorf("timeout = %v, want 2s", got)
	}
}

func TestPostSendsJSONWithTokenAndRequestID(t *testing.T) {
	caller := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/x" {
			t.Errorf("got %s %s, want POST /internal/v1/x", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Request-Id"); got != testRequestID {
			t.Errorf("X-Request-Id = %q, want %q", got, testRequestID)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"a":"in","b":1}` {
			t.Errorf("body = %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"a":"out","b":2,"extra":true}`)
	})
	var out pair
	if err := caller.Post(context.Background(), "/internal/v1/x", testRequestID, pair{"in", 1}, &out, http.StatusCreated); err != nil {
		t.Fatal(err)
	}
	if out != (pair{"out", 2}) {
		t.Errorf("out = %+v", out)
	}
}

func TestPostReturnsTheErrorCode(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		wantCode string
	}{
		{http.StatusUnauthorized, `{"error":{"code":"invalid_key","message":"m","request_id":"r"}}`, "invalid_key"},
		{http.StatusServiceUnavailable, `{"error":{"code":"dependency_unavailable","message":"m","request_id":"r"}}`, "dependency_unavailable"},
		{http.StatusInternalServerError, `not json`, ""},
		{http.StatusOK, `{"a":"ok"}`, ""}, // a status the call does not accept
		{http.StatusFound, ``, ""},        // a redirect is not followed
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			caller := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := caller.Post(context.Background(), "/x", testRequestID, pair{}, &pair{}, http.StatusCreated)
			var reply *Error
			if !errors.As(err, &reply) {
				t.Fatalf("err = %v, want an *Error", err)
			}
			if reply.Status != tc.status || reply.Code != tc.wantCode || Code(err) != tc.wantCode {
				t.Errorf("got status %d code %q, want %d %q", reply.Status, reply.Code, tc.status, tc.wantCode)
			}
			if Retryable(err) != (tc.status >= 500) {
				t.Errorf("Retryable = %v for status %d", Retryable(err), tc.status)
			}
		})
	}
}

func TestPostReportsAMalformedSuccessReply(t *testing.T) {
	caller := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"a":`)
	})
	err := caller.Post(context.Background(), "/x", testRequestID, pair{}, &pair{}, http.StatusOK)
	if !errors.Is(err, ErrMalformed) || Retryable(err) {
		t.Errorf("err = %v, want ErrMalformed, not retryable", err)
	}
}

func TestPostReportsNoAnswerAsUnavailable(t *testing.T) {
	cases := map[string]func(t *testing.T) *Caller{
		"timeout": func(t *testing.T) *Caller {
			return serve(t, func(w http.ResponseWriter, r *http.Request) {
				// The server sees the caller hang up only once the body is read.
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
			})
		},
		"connection refused": func(t *testing.T) *Caller {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			listener.Close()
			return NewCaller("http://"+addr, testToken)
		},
		"connection closed": func(t *testing.T) *Caller {
			return serve(t, func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					conn.Close()
				}
			})
		},
	}
	for name, caller := range cases {
		t.Run(name, func(t *testing.T) {
			err := caller(t).Post(context.Background(), "/x", testRequestID, pair{}, &pair{}, http.StatusOK)
			if !errors.Is(err, ErrUnavailable) || !Retryable(err) || Code(err) != "" {
				t.Errorf("err = %v, want a retryable ErrUnavailable", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("err %q holds the token", err)
			}
		})
	}
}

func TestPostReadsAtMostTheReplyCap(t *testing.T) {
	caller := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"a":"`+strings.Repeat("x", maxReplyBytes)+`"}`)
	})
	err := caller.Post(context.Background(), "/x", testRequestID, pair{}, &pair{}, http.StatusOK)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

func TestErrorNamesStatusAndCodeOnly(t *testing.T) {
	err := error(&Error{Status: 409, Code: "insufficient_balance"})
	if got, want := err.Error(), `internalapi: status 409, code "insufficient_balance"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if Code(errors.New("other")) != "" || Code(fmt.Errorf("wrapped: %w", err)) != "insufficient_balance" {
		t.Error("Code does not unwrap")
	}
}
