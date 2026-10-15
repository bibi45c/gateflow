// Package health serves the liveness and readiness endpoints that every FlowGate
// process exposes.
package health

import (
	"context"
	"encoding/json"
	"net/http"
)

// Handler reports that the named service is running. It keeps answering 200 while
// the process drains, so a liveness probe does not restart it mid-drain.
func Handler(service string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, service, "ok")
	})
}

// ReadyHandler reports whether the named service takes new traffic: 200 until the
// stopping context ends, then 503, so load balancers stop routing to the process
// before it closes its listener.
func ReadyHandler(stopping context.Context, service string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if stopping.Err() != nil {
			writeStatus(w, http.StatusServiceUnavailable, service, "stopping")
			return
		}
		writeStatus(w, http.StatusOK, service, "ready")
	})
}

func writeStatus(w http.ResponseWriter, code int, service, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"service": service, "status": status})
}
