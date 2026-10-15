// Command access is the access and channel management service: it owns tenants,
// gateway keys, channels and price versions, and verifies keys for the gateway.
// GET /metrics serves its Prometheus metrics, for scrapes inside the cluster only.
//
// `access seed [--tenants n]` fills the database with test data and prints the keys;
// `access healthcheck` checks a running process without reading the database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/bibi45c/gateflow/pkg/server"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/httpapi"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
	"github.com/bibi45c/gateflow/services/access/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			server.Main("access", ":8081", nil)
			return
		case "seed":
			err := seed.Command(context.Background(), os.Args[2:], os.Getenv, os.Stdout, os.Stderr)
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			exitOn(err)
			return
		}
	}
	cfg, err := config.LoadService(os.Getenv)
	exitOn(err)
	// One metrics registry for the process, never the global one.
	observer, err := httpapi.NewObserver(prometheus.NewRegistry())
	exitOn(err)
	pool, err := store.Open(context.Background(), cfg.DatabaseURL)
	exitOn(err)
	defer pool.Close()
	server.Main("access", ":8081", routes(store.New(pool), cfg, observer))
}

// routes returns the function that adds the internal API and GET /metrics. Like
// /healthz, /metrics needs no token: only the cluster reaches the service, and no
// ingress may route to it.
func routes(s httpapi.Store, cfg config.Service, observer *observability.Observer) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		// server.Main has set the default logger by the time it registers routes.
		httpapi.New(s, cfg.KeyPepper, cfg.InternalToken, slog.Default(), observer).Register(mux)
		mux.Handle("GET /metrics", observer.MetricsHandler())
	}
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "access:", err)
		os.Exit(1)
	}
}
