// Package server runs a FlowGate HTTP process: health endpoint, structured logs,
// and graceful shutdown on interrupt or SIGTERM.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bibi45c/gateflow/pkg/config"
	"github.com/bibi45c/gateflow/pkg/health"
)

// ShutdownTimeout bounds how long a process waits for in-flight requests after a
// stop signal. Streaming services will need a longer, configured drain period.
const ShutdownTimeout = 10 * time.Second

// Main runs the named service on $ADDR (default defaultAddr) with GET /healthz and
// the routes that register adds, and exits the process with status 1 on failure.
func Main(name, defaultAddr string, register func(*http.ServeMux)) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler(name))
	if register != nil {
		register(mux)
	}

	addr := config.Env("ADDR", defaultAddr)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", name)
	logger.Info("starting", "addr", addr)
	if err := Run(ctx, addr, mux); err != nil {
		logger.Error("stopped", "err", err)
		os.Exit(1)
	}
	logger.Info("stopped")
}

// Run serves handler on addr until ctx ends, then shuts down gracefully.
func Run(ctx context.Context, addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, listener, handler)
}

// Serve is Run with an existing listener, so tests can use a free port.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
