package anthropic

import (
	"bytes"
	"errors"
	"io"
	"strings"
)

// readSize is how many bytes Reader asks its source for at a time.
const readSize = 4096

// byteOrderMark may start a stream; the SSE rules ignore it.
var byteOrderMark = []byte("\xef\xbb\xbf")

// Event is one block of a server-sent event stream: its lines up to and including
// the blank line that ends it.
type Event struct {
	// Name is the value of the block's last event: field, or "" when it has none.
	Name string
	// Data is the values of the block's data: fields joined with "\n", or nil when
	// it has none. A block without data, such as a keep-alive comment, dispatches no
	// event under the SSE rules; Reader returns it so that its bytes can be relayed.
	Data []byte
	// Raw is the block exactly as read, with its comment lines and line endings.
	// Writing every Raw in order reproduces the stream byte for byte.
	Raw []byte
}

// Reader splits a server-sent event stream into blocks by the rules of the HTML
// Living Standard: a line ends with "\r\n", "\n" or "\r", a blank line ends a block,
// a line starting with ":" is a comment, and a field value loses one leading space.
// The id: and retry: fields and unknown fields are kept in Raw only.
type Reader struct {
	src     io.Reader
	chunk   []byte
	buf     []byte // read from src but not yet returned
	scanned int    // buf[:scanned] holds whole lines of the next block, none blank
	err     error  // the error that ended src, once it has
	started bool   // a block was returned, so a leading byte order mark is behind
}

// NewReader returns a Reader that reads a stream from src.
func NewReader(src io.Reader) *Reader {
	return &Reader{src: src}
}

// Next returns the next block of the stream, reading until one is complete, and
// io.EOF after the last one. When the stream ends inside a block, Next first returns
// that block with only Raw set, and io.ErrUnexpectedEOF or the error that ended the
// stream: an unfinished block dispatches nothing under the SSE rules, so callers may
// relay its bytes but must not act on them.
func (r *Reader) Next() (Event, error) {
	for {
		if n, ok := r.blockLen(); ok {
			return r.take(n), nil
		}
		if r.err != nil {
			return r.rest()
		}
		r.fill()
	}
}

// blockLen scans buf up to the blank line that ends the next block and returns the
// block's length. It reports false while more input is needed, which includes a buf
// ending in "\r" that a following "\n" would belong to.
func (r *Reader) blockLen() (int, bool) {
	for {
		rest := r.buf[r.scanned:]
		i := bytes.IndexAny(rest, "\r\n")
		if i < 0 || (rest[i] == '\r' && i+1 == len(rest) && r.err == nil) {
			return 0, false
		}
		r.scanned += i + lineEndLen(rest[i:])
		if i == 0 {
			return r.scanned, true
		}
	}
}

// take removes the first n bytes of buf, a complete block, and returns them as an
// Event.
func (r *Reader) take(n int) Event {
	raw := bytes.Clone(r.buf[:n])
	r.buf, r.scanned = r.buf[n:], 0
	fields := raw
	if !r.started {
		fields = bytes.TrimPrefix(raw, byteOrderMark)
		r.started = true
	}
	event := parseFields(fields)
	event.Raw = raw
	return event
}

// rest returns what is left of buf once src has ended, then the error that ended it.
func (r *Reader) rest() (Event, error) {
	if len(r.buf) == 0 {
		return Event{}, r.err
	}
	event := Event{Raw: bytes.Clone(r.buf)}
	r.buf, r.scanned = nil, 0
	if errors.Is(r.err, io.EOF) {
		return event, io.ErrUnexpectedEOF
	}
	return event, r.err
}

func (r *Reader) fill() {
	if r.chunk == nil {
		r.chunk = make([]byte, readSize)
	}
	n, err := r.src.Read(r.chunk)
	r.buf = append(r.buf, r.chunk[:n]...)
	if err != nil {
		r.err = err
	}
}

// parseFields reads the event: and data: fields of a complete block.
func parseFields(block []byte) Event {
	var event Event
	for len(block) > 0 {
		line, next := block, []byte(nil)
		if i := bytes.IndexAny(block, "\r\n"); i >= 0 {
			line, next = block[:i], block[i+lineEndLen(block[i:]):]
		}
		block = next
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(name) {
		case "event":
			event.Name = string(value)
		case "data":
			if event.Data == nil {
				event.Data = make([]byte, 0, len(value))
			} else {
				event.Data = append(event.Data, '\n')
			}
			event.Data = append(event.Data, value...)
		}
	}
	return event
}

// lineEndLen returns the length of the line ending that starts b.
func lineEndLen(b []byte) int {
	if len(b) > 1 && b[0] == '\r' && b[1] == '\n' {
		return 2
	}
	return 1
}

// Writer writes server-sent events in the framing that Reader reads. It does not
// flush: a caller writing to an http.ResponseWriter flushes after each write.
type Writer struct {
	dst io.Writer
}

// NewWriter returns a Writer that writes to dst.
func NewWriter(dst io.Writer) *Writer {
	return &Writer{dst: dst}
}

// WriteEvent writes one event in a single Write: an event: line unless name is
// empty, a data: line for each line of data, and the blank line that ends the event.
// The lines of data may end with "\n", "\r\n" or "\r"; Reader returns them joined
// with "\n". name must not hold a line break.
func (w *Writer) WriteEvent(name string, data []byte) error {
	if strings.ContainsAny(name, "\r\n") {
		return errors.New("anthropic: event name holds a line break")
	}
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	for {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			break
		}
		writeDataLine(&b, data[:i])
		data = data[i+lineEndLen(data[i:]):]
	}
	writeDataLine(&b, data)
	b.WriteByte('\n')
	_, err := w.dst.Write(b.Bytes())
	return err
}

// WriteComment writes a block holding one comment line, such as a keep-alive, which
// clients ignore. text must not hold a line break.
func (w *Writer) WriteComment(text string) error {
	if strings.ContainsAny(text, "\r\n") {
		return errors.New("anthropic: comment holds a line break")
	}
	_, err := io.WriteString(w.dst, ": "+text+"\n\n")
	return err
}

func writeDataLine(b *bytes.Buffer, line []byte) {
	b.WriteString("data: ")
	b.Write(line)
	b.WriteByte('\n')
}
