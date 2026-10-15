// Command metering is the quota and metering service: it owns wallets,
// reservations, settlements and the ledger, and answers usage queries.
//
// At startup it requires DATABASE_URL, ACCESS_URL, ACCESS_INTERNAL_TOKEN and
// METERING_INTERNAL_TOKEN, and applies its migrations before serving the reserve
// and settle operations under /internal/v1, and its Prometheus metrics on GET
// /metrics, for scrapes inside the cluster only. Run as
// `metering seed [--balance N] [--tenants N]`, it instead creates the missing test
// wallets and exits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/bibi45c/gateflow/pkg/server"
	"github.com/bibi45c/gateflow/services/metering/internal/accessclient"
	"github.com/bibi45c/gateflow/services/metering/internal/api"
	"github.com/bibi45c/gateflow/services/metering/internal/config"
	"github.com/bibi45c/gateflow/services/metering/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	name        = "metering"
	defaultAddr = ":8082"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			// Checks the running process only, without settings or a database.
			server.Main(name, defaultAddr, nil)
			return
		case "seed":
			exitOnError(seed(os.Args[2:]))
			return
		}
	}
	cfg, err := config.Load(os.Getenv)
	exitOnError(err)
	// One metrics registry for the process, never the global one. The helper adds the
	// service attribute itself, so its logger is a JSON logger on stdout like
	// server.Main's, without the attribute that server.Main adds to its own.
	observer, err := api.NewObserver(prometheus.NewRegistry(), slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	exitOnError(err)
	pool, err := store.Open(context.Background(), cfg.DatabaseURL)
	exitOnError(err)
	defer pool.Close()
	prices := accessclient.New(cfg.AccessURL, cfg.AccessInternalToken)
	handler := api.New(store.NewReservations(pool), prices, cfg.MeteringInternalToken, observer)
	server.Main(name, defaultAddr, routes(handler, observer))
}

// routes returns the function that adds the two operations and GET /metrics. Like
// /healthz, /metrics needs no token: only the cluster reaches the service, and no
// ingress may route to it.
func routes(handler *api.Handler, observer *observability.Observer) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		handler.Register(mux)
		mux.Handle("GET /metrics", observer.MetricsHandler())
	}
}

// seed runs the seed command with the arguments after "seed".
func seed(args []string) error {
	flags := flag.NewFlagSet(name+" seed", flag.ContinueOnError)
	balance := flags.Int64("balance", 100_000_000, "balance of each seeded wallet but tenant-00, in micro-units")
	tenants := flags.Int("tenants", 10, "seed tenant-00 to tenant-NN, where NN is this number")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("seed: unexpected arguments %q", flags.Args())
	}
	databaseURL, err := config.LoadSeed(os.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := store.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	created, err := store.Seed(ctx, pool, *balance, *tenants)
	if err != nil {
		return err
	}
	fmt.Printf("%s seed: created %d of %d wallets\n", name, created, *tenants+1)
	return nil
}

func exitOnError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}
