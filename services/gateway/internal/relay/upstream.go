package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"sync/atomic"
	"time"
)

// errChannelURL reports a channel base URL that no request can be built from.
var errChannelURL = errors.New("relay: channel base URL is not a valid URL")

// NewTransport returns the upstream transport: 10 s to open a connection and no
// limit on the reply, so that a slow local model such as Ollama can still stream. It
// uses no proxy and asks for no compression, so the bytes relayed are those sent.
// Handlers given the same transport share its connections.
func NewTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: connectTimeout,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
	}
}

// exchange is one admitted request: the upstream call and its reply, and what the
// outcome rule needs to know about them.
type exchange struct {
	h  *handler
	w  *recorder
	r  *http.Request
	id string

	dispatched    atomic.Bool // the request was written upstream, or a reply came
	completed     bool        // the end event arrived, or a 2xx body read in full completes the call
	clientGone    bool        // the client disconnected before the reply was complete
	upstreamError bool        // the upstream answered non-2xx, before any output
	observer      observer
}

// relay sends an admitted request upstream and relays the reply. It calls
// admitted.Report exactly once, when it returns or panics, and not with the request's
// context, so the report also happens after the client has gone.
func (h *handler) relay(w *recorder, r *http.Request, body []byte, id string, admitted Admitted) {
	x := &exchange{h: h, w: w, r: r, id: id, observer: h.proto.newObserver()}
	if admitted.Report != nil {
		defer func() { admitted.Report(x.result()) }()
	}
	// The upstream request ends with the handler, and when the client disconnects,
	// because the server then cancels the request's context.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	resp, err := x.send(ctx, body, admitted.Channel)
	if err != nil {
		x.failSend(err)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header, "Retry-After")
	switch {
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		x.relayStatus(resp)
	case isEventStream(resp.Header):
		x.relayStream(resp)
	default:
		x.relayBody(resp)
	}
}

// send posts body to the channel, at the endpoint's path, with only the forwarded
// headers and the channel's credential. It never retries.
func (x *exchange) send(ctx context.Context, body []byte, channel Channel) (*http.Response, error) {
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				x.dispatched.Store(true)
			}
		},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	url := channel.BaseURL + x.h.proto.path()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, errChannelURL
	}
	for _, name := range x.h.proto.forwardedHeaders() {
		for _, value := range x.r.Header.Values(name) {
			req.Header.Add(name, value)
		}
	}
	if channel.CredentialEnv != "" {
		if credential := x.h.getenv(channel.CredentialEnv); credential != "" {
			x.h.proto.setCredential(req.Header, credential)
		}
	}
	resp, err := x.h.client.Do(req)
	if err == nil {
		x.dispatched.Store(true)
	}
	return resp, err
}

// failSend answers a request that got no reply: 500 when the channel's URL is
// unusable, otherwise 502.
func (x *exchange) failSend(err error) {
	if errors.Is(err, errChannelURL) {
		x.h.logger.Error("channel base URL is invalid", "request_id", x.id)
		x.h.writeError(x.w, errInternal)
		return
	}
	x.noteFailure("upstream request failed", err)
	x.h.writeError(x.w, errUpstream)
}

// noteFailure records an upstream call or read that failed: as the client's
// disconnect when the request's context has ended, otherwise as an upstream failure,
// which it logs. Transport errors hold no body, header or credential.
func (x *exchange) noteFailure(message string, err error) {
	if x.r.Context().Err() != nil {
		x.clientGone = true
		return
	}
	x.h.logger.Warn(message, "request_id", x.id, "err", err.Error())
}

// relayStatus answers a non-2xx reply, which comes before any output: 400 and 429
// pass through with their status and body, any other status becomes 502. The
// upstream body is never logged.
func (x *exchange) relayStatus(resp *http.Response) {
	x.upstreamError = true
	x.h.logger.Warn("upstream answered with an error status",
		"request_id", x.id, "status", resp.StatusCode)
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusTooManyRequests {
		x.h.writeError(x.w, errUpstream)
		return
	}
	body, err := readAtMost(resp.Body, maxEventBytes)
	if err != nil {
		x.h.writeError(x.w, errUpstream)
		return
	}
	x.passBack(resp, body)
}

// relayBody passes back a 2xx non-streaming reply once it is read in full, so that a
// failure while reading it still gets a 502. The body is one message or response, so
// it has the cap of one event.
func (x *exchange) relayBody(resp *http.Response) {
	body, err := readAtMost(resp.Body, maxEventBytes)
	if err != nil {
		x.noteFailure("reading the upstream reply failed", err)
		x.h.writeError(x.w, errUpstream)
		return
	}
	x.completed = x.observer.body(body)
	x.passBack(resp, body)
}

// passBack writes an upstream status and body, with the upstream's content type.
func (x *exchange) passBack(resp *http.Response, body []byte) {
	copyHeader(x.w.Header(), resp.Header, "Content-Type")
	x.w.WriteHeader(resp.StatusCode)
	_, _ = x.w.Write(body)
}

// readAtMost reads all of r, or fails with errEventTooLarge once it holds more than
// limit bytes.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errEventTooLarge
	}
	return data, nil
}

// copyHeader copies the values of one header, if src has any.
func copyHeader(dst, src http.Header, name string) {
	if values := src.Values(name); len(values) > 0 {
		dst[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
}

func isEventStream(header http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}
