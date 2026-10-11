// Package server runs a FlowGate HTTP process: health endpoint, structured logs,
// and graceful shutdown on interrupt or SIGTERM.
package server

import (
	"context"
	"errors"
	"fmt"
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
// stop signal; requests still open then are closed. Container stop grace periods
// must be longer. Streaming services will need a longer, configured drain period.
const ShutdownTimeout = 10 * time.Second

// Main runs the named service on $ADDR (default defaultAddr) with GET /healthz and
// the routes that register adds. It logs JSON to stdout and exits with status 1 when
// the server fails; a drain that times out is logged as a warning.
func Main(name, defaultAddr string, register func(*http.ServeMux)) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore default signal handling once stopping begins, so a second signal
	// forces an exit instead of waiting for the drain.
	context.AfterFunc(ctx, stop)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", name)
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler(name))
	if register != nil {
		register(mux)
	}

	addr := config.Env("ADDR", defaultAddr)
	logger.Info("starting", "addr", addr)
	err := Run(ctx, addr, mux)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		logger.Warn("stopped after drain timeout", "err", err)
	case err != nil:
		logger.Error("stopped", "err", err)
		os.Exit(1)
	default:
		logger.Info("stopped")
	}
}

// Run serves handler on addr until ctx ends, then shuts down gracefully.
func Run(ctx context.Context, addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, listener, handler)
}

// Serve is Run with an existing listener, so tests can use a free port. When the
// drain exceeds ShutdownTimeout, it closes the remaining connections, which cancels
// their request contexts, and returns an error wrapping context.DeadlineExceeded.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}
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
		_ = srv.Close()
		return fmt.Errorf("drain: closed open connections: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
