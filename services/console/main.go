// Command console is FlowGate's web console: a user portal (balance, gateway keys,
// usage, models, quick start and a streaming playground) and an admin area
// (tenants and top-ups, channels and prices, and a live system dashboard).
//
// It keeps no data of its own: it calls access and metering over their internal
// APIs, the gateway for playground calls, and Prometheus for the dashboard. It
// requires ACCESS_URL, ACCESS_INTERNAL_TOKEN, METERING_URL, METERING_INTERNAL_TOKEN,
// GATEWAY_URL and CONSOLE_SESSION_SECRET; see internal/config for the optional
// settings. `console healthcheck` checks a running process.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/bibi45c/gateflow/pkg/server"
	"github.com/bibi45c/gateflow/services/console/internal/backend"
	"github.com/bibi45c/gateflow/services/console/internal/config"
	"github.com/bibi45c/gateflow/services/console/internal/session"
	"github.com/bibi45c/gateflow/services/console/internal/web"
)

const (
	name        = "console"
	defaultAddr = ":8070"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		server.Main(name, defaultAddr, nil)
		return
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}
	server.Main(name, defaultAddr, func(mux *http.ServeMux) {
		// server.Main has set the default logger by the time it registers routes.
		client := backend.New(cfg.AccessURL, cfg.AccessInternalToken, cfg.MeteringURL, cfg.MeteringInternalToken)
		web.New(client, session.New(cfg.SessionSecret, cfg.SecureCookie), web.Options{
			GatewayURL:       cfg.GatewayURL,
			PublicGatewayURL: cfg.PublicGatewayURL,
			PrometheusURL:    cfg.PrometheusURL,
			GrafanaURL:       cfg.GrafanaURL,
			GatewayJob:       cfg.GatewayJob,
			Location:         cfg.Location,
		}, slog.Default()).Register(mux)
	})
}
