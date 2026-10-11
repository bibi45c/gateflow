// Package health serves the liveness endpoint that every FlowGate process exposes.
package health

import (
	"encoding/json"
	"net/http"
)

// Handler reports that the named service is running.
func Handler(service string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"service": service, "status": "ok"})
	})
}
