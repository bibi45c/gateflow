// Command access is the access and channel management service: it owns tenants,
// gateway keys, channels and price versions, and verifies keys for the gateway.
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

	"github.com/bibi45c/gateflow/pkg/server"
	"github.com/bibi45c/gateflow/services/access/internal/config"
	"github.com/bibi45c/gateflow/services/access/internal/httpapi"
	"github.com/bibi45c/gateflow/services/access/internal/seed"
	"github.com/bibi45c/gateflow/services/access/internal/store"
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
	pool, err := store.Open(context.Background(), cfg.DatabaseURL)
	exitOn(err)
	defer pool.Close()
	server.Main("access", ":8081", func(mux *http.ServeMux) {
		// server.Main has set the default logger by the time it registers routes.
		httpapi.New(store.New(pool), cfg.KeyPepper, cfg.InternalToken, slog.Default()).Register(mux)
	})
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "access:", err)
		os.Exit(1)
	}
}
