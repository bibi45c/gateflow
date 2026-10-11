package server

import (
	"context"
	"io"
	"net"
	"net/http"
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
	go func() { done <- Serve(ctx, listener, mux) }()

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
	go func() { _ = Serve(ctx, listener, mux) }()

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
