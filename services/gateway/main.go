// Command gateway is the model gateway service: it serves the native model APIs
// (/v1/messages, /v1/responses) to local coding tools, routes requests to approved
// channels and relays streamed output.
//
// At startup it requires ACCESS_URL, METERING_URL, ACCESS_INTERNAL_TOKEN and
// METERING_INTERNAL_TOKEN. `gateway healthcheck` checks a running process without
// reading them.
package main

import (
	"fmt"
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
	server.Main(name, defaultAddr, register(cfg))
}

// register returns the function that adds the gateway's routes. It runs inside
// server.Main, after the service logger has become the default one.
func register(cfg config.Config) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		logger := slog.Default()
		admit := admission.New(admission.Config{
			Access:   accessclient.New(internalapi.NewCaller(cfg.AccessURL, cfg.AccessInternalToken)),
			Metering: meteringclient.New(internalapi.NewCaller(cfg.MeteringURL, cfg.MeteringInternalToken)),
			Logger:   logger,
		})
		// The two endpoints share one upstream transport, and so its connections.
		relayConfig := relay.Config{Transport: relay.NewTransport()}
		mux.Handle("POST /v1/messages", relay.New(relayConfig, admit, logger))
		mux.Handle("POST /v1/responses", relay.NewResponses(relayConfig, admit, logger))
	}
}
