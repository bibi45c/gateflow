package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/bibi45c/gateflow/pkg/observability"
	"github.com/bibi45c/gateflow/pkg/server"
)

const (
	metadataFrame = "event: metadata\ndata: {\"type\":\"start\"}\n\n"
	outputFrame   = "event: model\ndata: {\"delta\":\"x\"}\n\n"
	terminalFrame = "event: done\ndata: {\"status\":\"completed\"}\n\n"
	errorFrame    = "event: error\ndata: {\"type\":\"probe_error\"}\n\n"
	plainResponse = "{\"output\":\"x\"}\n"
)

// These are test-only, protocol-neutral probes, not model APIs or authentication.
// No service main is changed. Real protocol classification remains a service task.
type probeOptions struct {
	stream         bool
	delay          time.Duration
	afterFirst     <-chan struct{}
	afterMetadata  <-chan struct{}
	capacityReject bool
	streamError    bool
	errorOutput    string
}

func probeHandler(observer *observability.Observer, route string, opts probeOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		observed, err := observer.Begin(observability.RequestInfo{Route: route, Method: request.Method,
			RequestID: request.Header.Get("X-Request-Id"), ModelAlias: "probe"})
		if err != nil {
			http.Error(w, "probe configuration error", 500)
			return
		}
		defer observed.Finish(observability.Summary{Result: observability.Interrupted})
		x := &exchange{w: w, request: request, observed: observed}
		if x.read, err = io.Copy(io.Discard, request.Body); err != nil {
			x.finish(observability.ClientCancelled, 0)
			return
		}
		// All load-probe requests are scripted as eligible. This is not key checking.
		observed.SetVerifiedTenantSlot("tenant-01")
		observed.MarkEligible()
		if opts.capacityReject {
			w.WriteHeader(503)
			x.finish(observability.CapacityRejected, 503)
			return
		}
		observed.MarkAdmitted()
		if opts.stream {
			x.stream(opts)
		} else {
			x.plain()
		}
	})
}

// exchange is one probe request being answered: its writer, its observed handle and
// the byte counts it reports when it finishes.
type exchange struct {
	w        http.ResponseWriter
	request  *http.Request
	observed *observability.Request
	read     int64
	written  int64
}

// finish reports the terminal result with the bytes counted so far.
func (x *exchange) finish(result observability.Result, status int) {
	x.observed.Finish(observability.Summary{Result: result, HTTPStatus: status, RequestBytes: x.read, ResponseBytes: x.written})
}

// send writes data and flushes it. When either fails it finishes the request as
// interrupted and returns false.
func (x *exchange) send(data string) bool {
	n, err := io.WriteString(x.w, data)
	x.written += int64(n)
	if err == nil {
		err = http.NewResponseController(x.w).Flush()
	}
	if err != nil {
		x.finish(observability.Interrupted, 200)
		return false
	}
	return true
}

// wait blocks until gate is closed; a nil gate does not block. When the client
// leaves first it finishes the request as cancelled and returns false.
func (x *exchange) wait(gate <-chan struct{}) bool {
	if gate == nil {
		return true
	}
	select {
	case <-gate:
		return true
	case <-x.request.Context().Done():
		x.finish(observability.ClientCancelled, 200)
		return false
	}
}

// pause waits for delay, with the same rule as wait when the client leaves.
func (x *exchange) pause(delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-x.request.Context().Done():
		x.finish(observability.ClientCancelled, 200)
		return false
	}
}

// plain answers with one complete body.
func (x *exchange) plain() {
	x.w.Header().Set("Content-Type", "application/json")
	if !x.send(plainResponse) {
		return
	}
	x.observed.MarkFirstOutput()
	x.finish(observability.Completed, 200)
}

// stream answers with a metadata frame, one output frame and a terminal or error
// frame, stopping at the gates and the delay that opts sets.
func (x *exchange) stream(opts probeOptions) {
	x.w.Header().Set("Content-Type", "text/event-stream")
	if !x.send(metadataFrame) || !x.wait(opts.afterMetadata) || !x.send(outputFrame) {
		return
	}
	x.observed.MarkFirstOutput()
	if !x.wait(opts.afterFirst) || !x.pause(opts.delay) {
		return
	}
	if opts.streamError {
		data := opts.errorOutput
		if data == "" {
			data = errorFrame
		}
		if x.send(data) {
			x.finish(observability.UpstreamError, 200)
		}
		return
	}
	if x.send(terminalFrame) {
		x.finish(observability.Completed, 200)
	}
}

func startProbe(handler http.Handler) (string, func() error, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener, handler) }()
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(server.ShutdownTimeout + time.Second):
			return fmt.Errorf("probe shutdown exceeded skeleton drain")
		}
	}
	return "http://" + listener.Addr().String(), stop, nil
}
