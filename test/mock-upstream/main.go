// Command mock-upstream is a controllable fake model provider for load and failure
// tests. It serves POST /v1/messages in the Anthropic Messages protocol, with a fixed
// reply for each mock model name; package internal/mock lists the scenarios and the
// environment variables that pace the streams.
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/bibi45c/gateflow/pkg/server"
	"github.com/bibi45c/gateflow/test/mock-upstream/internal/mock"
)

func main() {
	server.Main("mock-upstream", ":8090", register)
}

func register(mux *http.ServeMux) {
	timing, err := mock.TimingFromEnv()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	mux.Handle("POST /v1/messages", mock.NewHandler(timing))
}
