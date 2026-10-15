// Command gateway is the model gateway service: it serves the native model APIs
// (/v1/messages, /v1/responses) to local coding tools, routes requests to approved
// channels and relays streamed output. GET /metrics serves its Prometheus metrics,
// for scrapes inside the cluster only.
//
// At startup it requires ACCESS_URL, METERING_URL, ACCESS_INTERNAL_TOKEN and
// METERING_INTERNAL_TOKEN, and reads MODEL_ALIASES and UPSTREAM_MAX_IDLE_CONNS when
// they are set. `gateway healthcheck` checks a running process without reading them.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/bibi45c/gateflow/pkg/server"
	"github.com/bibi45c/gateflow/services/gateway/internal/accessclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/admission"
	"github.com/bibi45c/gateflow/services/gateway/internal/config"
	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
	"github.com/bibi45c/gateflow/services/gateway/internal/meteringclient"
	"github.com/bibi45c/gateflow/services/gateway/internal/relay"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	name        = "gateway"
	defaultAddr = ":8080"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		// Checks the running process only, without settings.
		server.Main(name, defaultAddr, nil)
		return
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
	routes, err := register(cfg, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
	server.Main(name, defaultAddr, routes)
}

// register returns the function that adds the gateway's routes, which runs inside
// server.Main, after the service logger has become the default one. The request log
// lines go to logs, which is stdout in the service. The process has one metrics
// registry, never the global one, holding the observability helper's metrics only.
func register(cfg config.Config, logs io.Writer) (func(*http.ServeMux), error) {
	// The helper adds the service attribute itself, so its logger is a JSON logger
	// like server.Main's, without the attribute that server.Main adds to its own.
	registry := prometheus.NewRegistry()
	observer, err := relay.NewObserver(registry, slog.New(slog.NewJSONHandler(logs, nil)), cfg.ModelAliases)
	if err != nil {
		return nil, err
	}
	return func(mux *http.ServeMux) {
		logger := slog.Default()
		admit := admission.New(admission.Config{
			Access:   accessclient.New(internalapi.NewCaller(cfg.AccessURL, cfg.AccessInternalToken)),
			Metering: meteringclient.New(internalapi.NewCaller(cfg.MeteringURL, cfg.MeteringInternalToken)),
			Logger:   logger,
		})
		// The two endpoints share one upstream transport, and so its connections, and
		// one observer, and so its metrics.
		relayConfig := relay.Config{Transport: relay.NewTransport(cfg.UpstreamMaxIdleConns), Observer: observer}
		mux.Handle("POST /v1/messages", relay.New(relayConfig, admit, logger))
		mux.Handle("POST /v1/responses", relay.NewResponses(relayConfig, admit, logger))
		// Like /healthz, /metrics is for the cluster: the ingress exposes only the two
		// model endpoints, and must never route /metrics.
		mux.Handle("GET /metrics", observer.MetricsHandler())
	}, nil
}
