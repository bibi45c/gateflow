package relay

import (
	"net/http"
)

// recorder keeps the status and byte count of a response, which the request's
// observation reports. The observability helper wraps no writer, so the relay counts.
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
