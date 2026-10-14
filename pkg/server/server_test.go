package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeAnswersThenStopsWhenContextEnds(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, mux, Shutdown{Timeout: ShutdownTimeout}) }()

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "pong" {
		t.Errorf("body: got %q", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(ShutdownTimeout + time.Second):
		t.Fatal("server did not stop after its context ended")
	}
}

func TestServeDrainsThenWaitsForStreams(t *testing.T) {
	// The stream runs well past the drain delay, so it is still open when the
	// listener closes. Every bound below only fails when shutdown starts too early.
	const drainDelay = 1500 * time.Millisecond
	const chunks, interval = 8, 500 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var streamFinished atomic.Bool
	mux := newMux(ctx, "test", func(mux *http.ServeMux) {
		mux.Handle("GET /stream", slowStream(chunks, interval, started, &streamFinished))
	})
	type result struct {
		err            error
		streamFinished bool
	}
	done := make(chan result, 1)
	go func() {
		err := Serve(ctx, listener, mux, Shutdown{DrainDelay: drainDelay, Timeout: 30 * time.Second})
		done <- result{err, streamFinished.Load()}
	}()

	// Every request opens a new connection, so an answer proves the listener is open.
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if code, err := statusOf(client, base+"/readyz"); err != nil || code != http.StatusOK {
		t.Fatalf("readyz before stopping: got %d, %v", code, err)
	}
	streamed := bodyOf(client, base+"/stream")
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not start")
	}

	cancel()
	stopped := time.Now()
	if code, err := statusOf(client, base+"/readyz"); err != nil || code != http.StatusServiceUnavailable {
		t.Errorf("readyz while draining: got %d, %v", code, err)
	}
	if code, err := statusOf(client, base+"/healthz"); err != nil || code != http.StatusOK {
		t.Errorf("healthz while draining: got %d, %v", code, err)
	}

	if refused := waitUntilRefused(t, client, base+"/healthz", stopped, drainDelay+20*time.Second); refused < drainDelay {
		t.Errorf("new connections refused after %v, before the drain delay of %v", refused, drainDelay)
	}

	var want strings.Builder
	for i := range chunks {
		fmt.Fprintf(&want, "chunk %d\n", i)
	}
	select {
	case body := <-streamed:
		if body != want.String() {
			t.Errorf("stream body: got %q, want %q", body, want.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stream did not finish")
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Errorf("shutdown: %v", got.err)
		}
		if !got.streamFinished {
			t.Error("Serve returned before the stream finished")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not stop after the stream finished")
	}
}

func TestServeClosesStreamsThatOutlastTheTimeout(t *testing.T) {
	const timeout = 300 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Minute):
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, mux, Shutdown{Timeout: timeout}) }()
	go func() {
		client := &http.Client{Timeout: 2 * time.Minute}
		if response, err := client.Get("http://" + listener.Addr().String() + "/stream"); err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not start")
	}

	cancel()
	stopped := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("shutdown: got %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(stopped); elapsed < timeout {
			t.Errorf("returned after %v, before the timeout of %v", elapsed, timeout)
		}
	case <-time.After(timeout + 20*time.Second):
		t.Fatal("server did not stop after the shutdown timeout")
	}
}

func TestShutdownFromEnvUsesDefaultsOrValues(t *testing.T) {
	t.Setenv("SHUTDOWN_DRAIN_DELAY", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	got, err := shutdownFromEnv()
	if err != nil || got != (Shutdown{DrainDelay: 0, Timeout: ShutdownTimeout}) {
		t.Errorf("defaults: got %+v, %v", got, err)
	}

	t.Setenv("SHUTDOWN_DRAIN_DELAY", "5s")
	t.Setenv("SHUTDOWN_TIMEOUT", "75s")
	got, err = shutdownFromEnv()
	if err != nil || got != (Shutdown{DrainDelay: 5 * time.Second, Timeout: 75 * time.Second}) {
		t.Errorf("values: got %+v, %v", got, err)
	}
}

func TestShutdownFromEnvRejectsInvalidValuesWithoutEchoingThem(t *testing.T) {
	for _, key := range []string{"SHUTDOWN_DRAIN_DELAY", "SHUTDOWN_TIMEOUT"} {
		for _, value := range []string{"ten-seconds", "-1s"} {
			t.Setenv("SHUTDOWN_DRAIN_DELAY", "")
			t.Setenv("SHUTDOWN_TIMEOUT", "")
			t.Setenv(key, value)
			_, err := shutdownFromEnv()
			if err == nil {
				t.Errorf("%s=%q: no error", key, value)
				continue
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q: error does not name the variable: %v", key, value, err)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("%s=%q: error contains the value: %v", key, value, err)
			}
		}
	}
}

func TestCheckHealthReportsTheProcessOnThePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	mux := http.NewServeMux()
	var unhealthy atomic.Bool
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if unhealthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, listener, mux, Shutdown{Timeout: ShutdownTimeout}) }()

	if err := CheckHealth(":" + port); err != nil {
		t.Errorf("healthy process: %v", err)
	}
	unhealthy.Store(true)
	if err := CheckHealth("0.0.0.0:" + port); err == nil {
		t.Error("an unhealthy status passed")
	}
	if err := CheckHealth("no-port"); err == nil {
		t.Error("an address without a port passed")
	}
}

// slowStream writes chunks lines, interval apart, flushing each one. It closes
// started after the first line and sets finished after the last.
func slowStream(chunks int, interval time.Duration, started chan<- struct{}, finished *atomic.Bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := range chunks {
			if i > 0 {
				time.Sleep(interval)
			}
			fmt.Fprintf(w, "chunk %d\n", i)
			w.(http.Flusher).Flush()
			if i == 0 {
				close(started)
			}
		}
		finished.Store(true)
	})
}

// bodyOf sends a GET of url in the background and delivers the whole body, or a
// description of the failure.
func bodyOf(client *http.Client, url string) <-chan string {
	body := make(chan string, 1)
	go func() {
		response, err := client.Get(url)
		if err != nil {
			body <- "request failed: " + err.Error()
			return
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			body <- "read failed: " + err.Error()
			return
		}
		body <- string(data)
	}()
	return body
}

// waitUntilRefused polls url until a request fails and returns how long after
// since that was. It fails the test when requests still succeed after limit.
func waitUntilRefused(t *testing.T, client *http.Client, url string, since time.Time, limit time.Duration) time.Duration {
	t.Helper()
	for {
		if _, err := statusOf(client, url); err != nil {
			return time.Since(since)
		}
		if time.Since(since) > limit {
			t.Fatalf("requests still succeeded %v after stopping", limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// statusOf returns the status code of a GET of url.
func statusOf(client *http.Client, url string) (int, error) {
	response, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode, nil
}
