package relay

import (
	"net/http"
	"time"
)

// recorder keeps the status and byte count of a response for the access log.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the server's writer.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// writeEvent writes one event and flushes it, so the client has it at once.
func (r *recorder) writeEvent(raw []byte) error {
	if _, err := r.Write(raw); err != nil {
		return err
	}
	return r.flush()
}

func (r *recorder) flush() error {
	return http.NewResponseController(r.ResponseWriter).Flush()
}

// logAccess writes the access log line of a request. It records the method, route,
// status, duration, model, byte counts and request id, and never a body, a key, a
// credential or an upstream error body. This logging is interim, until issue #13.
func (h *handler) logAccess(r *http.Request, w *recorder, call *Call, start time.Time) {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	route := r.Pattern
	if route == "" {
		route = messagesPath
	}
	h.logger.Info("request",
		"method", r.Method,
		"route", route,
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
		"model", call.Model,
		"request_bytes", call.BodyBytes,
		"response_bytes", w.bytes,
		"request_id", call.RequestID,
	)
}
