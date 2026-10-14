package anthropic

import (
	"io"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// Event is one block of a server-sent event stream; see sse.Event. Its Type method
// returns the "type" member of its data, which names a Messages event.
type Event = sse.Event

// Reader splits a server-sent event stream into blocks; see sse.Reader.
type Reader = sse.Reader

// Writer writes server-sent events in the framing that Reader reads; see sse.Writer.
type Writer = sse.Writer

// NewReader returns a Reader that reads a stream from src.
func NewReader(src io.Reader) *Reader {
	return sse.NewReader(src)
}

// NewWriter returns a Writer that writes to dst.
func NewWriter(dst io.Writer) *Writer {
	return sse.NewWriter(dst)
}
