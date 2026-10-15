package relay

import (
	"errors"
	"io"
	"net/http"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// errEventTooLarge reports an upstream event, or non-streaming body, over the cap.
var errEventTooLarge = errors.New("relay: upstream event is larger than 1 MiB")

// errErrorBeforeOutput reports a stream whose first event is an error event. It
// never holds the event's content.
var errErrorBeforeOutput = errors.New("relay: upstream stream began with an error event")

// relayStream relays a 2xx event stream one event at a time, flushing after each, so
// that the client has every event before the next one is read. The response starts
// with the first whole event, so a failure before it still gets a 502, and so does
// an upstream error event that comes first. The stream ends after the protocol's end
// event, or after a later upstream error event, which is relayed unchanged.
func (x *exchange) relayStream(resp *http.Response) {
	events := newEventReader(resp.Body, maxEventBytes)
	started := false
	for {
		event, err := events.Next()
		if err != nil {
			x.endStream(err, started)
			return
		}
		role := x.observer.event(event)
		if !started && role == roleFailure {
			x.endStream(errErrorBeforeOutput, started)
			return
		}
		if !started {
			copyHeader(x.w.Header(), resp.Header, "Content-Type")
			x.w.WriteHeader(resp.StatusCode)
			started = true
		}
		if role == roleEnd {
			x.completed = true
		}
		if err := x.w.writeEvent(event.Raw); err != nil {
			x.clientGone = true
			return
		}
		if role != roleOutput {
			return
		}
	}
}

// endStream handles a stream that failed or stopped before its end event. The gateway
// answers an upstream failure with a 502 before any output, and with its own error
// event after it. An unfinished last event is dropped: its bytes would run into that
// error event.
func (x *exchange) endStream(err error, started bool) {
	if errors.Is(err, io.EOF) {
		err = x.h.proto.noEnd()
	}
	x.noteFailure("upstream stream failed", err)
	switch {
	case x.clientGone:
		return
	case !started:
		x.h.writeError(x.w, errUpstream)
	default:
		name, data := x.observer.errorEvent()
		if sse.NewWriter(x.w).WriteEvent(name, data) == nil {
			_ = x.w.flush()
		}
	}
}

// eventReader reads upstream events and fails with errEventTooLarge on one over its
// limit. sse.Reader holds an event in memory until the blank line that ends it,
// however long that is, so the source it reads from stops once the unfinished event
// is over the limit.
type eventReader struct {
	events *sse.Reader
	source *cappedSource
	limit  int64
}

func newEventReader(r io.Reader, limit int64) *eventReader {
	source := &cappedSource{src: r, limit: limit}
	return &eventReader{events: sse.NewReader(source), source: source, limit: limit}
}

// Next returns the next event, as sse.Reader.Next does, or errEventTooLarge.
func (e *eventReader) Next() (sse.Event, error) {
	event, err := e.events.Next()
	e.source.returned += int64(len(event.Raw))
	if err == nil && int64(len(event.Raw)) > e.limit {
		return sse.Event{}, errEventTooLarge
	}
	return event, err
}

// cappedSource refuses to read on while the bytes read but not yet returned in an
// event are over limit. sse.Reader only reads when what it holds is an unfinished
// event, so those bytes all belong to that event.
type cappedSource struct {
	src      io.Reader
	limit    int64
	read     int64 // bytes read from src
	returned int64 // bytes returned in events
}

func (s *cappedSource) Read(p []byte) (int, error) {
	if s.read-s.returned > s.limit {
		return 0, errEventTooLarge
	}
	n, err := s.src.Read(p)
	s.read += int64(n)
	return n, err
}
