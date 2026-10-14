package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// chunkReader returns at most size bytes per Read, so that blocks arrive split
// across reads, or several in one read.
type chunkReader struct {
	data []byte
	size int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.size, len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// readAll reads every block of a stream and fails the test on any error but io.EOF.
func readAll(t *testing.T, src io.Reader) []Event {
	t.Helper()
	reader := NewReader(src)
	var events []Event
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("block %d: %v", len(events), err)
		}
		events = append(events, event)
	}
}

func joinRaw(events []Event) []byte {
	var joined []byte
	for _, event := range events {
		joined = append(joined, event.Raw...)
	}
	return joined
}

func TestReaderSplitsLineEndingsAcrossReads(t *testing.T) {
	stream := "data: a\r\n\r\ndata: b\r\rdata: c\n\nevent: d\r\ndata: e\r\r\n"
	want := []string{"data: a\r\n\r\n", "data: b\r\r", "data: c\n\n", "event: d\r\ndata: e\r\r\n"}
	for size := 1; size <= len(stream); size++ {
		events := readAll(t, &chunkReader{data: []byte(stream), size: size})
		var got []string
		for _, event := range events {
			got = append(got, string(event.Raw))
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("size %d: got %q, want %q", size, got, want)
		}
		if string(events[3].Data) != "e" || events[3].Name != "d" {
			t.Fatalf("size %d: last block %q/%q", size, events[3].Name, events[3].Data)
		}
	}
}

func TestReaderFieldRules(t *testing.T) {
	stream := "data\n\n" +
		"data:x\n\n" +
		"data:  two spaces\n\n" +
		"event: first\nevent: second\nid: 7\nretry: 100\nfoo: bar\ndata: d\n\n" +
		": only a comment\n\n" +
		"event: no-data\n\n" +
		"data: one\ndata:\ndata: three\n\n"
	want := []struct {
		name string
		data []byte
	}{
		{"", []byte("")},
		{"", []byte("x")},
		{"", []byte(" two spaces")},
		{"second", []byte("d")},
		{"", nil},
		{"no-data", nil},
		{"", []byte("one\n\nthree")},
	}
	events := readAll(t, strings.NewReader(stream))
	if len(events) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(events), len(want))
	}
	for i, w := range want {
		got := events[i]
		if got.Name != w.name || !bytes.Equal(got.Data, w.data) || (got.Data == nil) != (w.data == nil) {
			t.Errorf("block %d: got %q/%q (nil %v), want %q/%q", i, got.Name, got.Data, got.Data == nil, w.name, w.data)
		}
	}
}

func TestReaderIgnoresLeadingByteOrderMark(t *testing.T) {
	events := readAll(t, strings.NewReader("\xef\xbb\xbfevent: a\ndata: x\n\nevent: b\ndata: y\n\n"))
	if len(events) != 2 || events[0].Name != "a" || string(events[0].Raw) != "\xef\xbb\xbfevent: a\ndata: x\n\n" {
		t.Errorf("got %+v", events)
	}
}

func TestReaderReturnsUnfinishedBlockAtTheEnd(t *testing.T) {
	reader := NewReader(strings.NewReader("data: a\n\ndata: {\"type\":\"message_stop\"}\n"))
	if event, err := reader.Next(); err != nil || string(event.Data) != "a" {
		t.Fatalf("first block: %q, %v", event.Data, err)
	}
	event, err := reader.Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(event.Raw) != "data: {\"type\":\"message_stop\"}\n" ||
		event.Data != nil || event.Name != "" || event.Type() != "" {
		t.Fatalf("unfinished block: %+v, %v", event, err)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the end: %v", err)
	}
}

func TestReaderPassesOnReadErrors(t *testing.T) {
	failure := errors.New("connection reset")
	reader := NewReader(io.MultiReader(strings.NewReader("data: a\n\ndata: b"), iotest.ErrReader(failure)))
	if event, err := reader.Next(); err != nil || string(event.Data) != "a" {
		t.Fatalf("first block: %q, %v", event.Data, err)
	}
	if event, err := reader.Next(); !errors.Is(err, failure) || string(event.Raw) != "data: b" {
		t.Fatalf("unfinished block: %q, %v", event.Raw, err)
	}
	if _, err := reader.Next(); !errors.Is(err, failure) {
		t.Fatalf("after the error: %v", err)
	}
}

func TestReaderOnEmptyStream(t *testing.T) {
	if _, err := NewReader(strings.NewReader("")).Next(); !errors.Is(err, io.EOF) {
		t.Errorf("got %v, want io.EOF", err)
	}
}

func TestEventType(t *testing.T) {
	cases := []struct{ data, want string }{
		{`{"type":"message_stop"}`, "message_stop"},
		{`{"sequence_number":3,"type" : "response.output_text.delta","delta":"OK"}`, "response.output_text.delta"},
		{`{"response":{"type":"inner"},"type":"response.completed"}`, "response.completed"},
		{`{"response":{"type":"inner"}}`, ""},
		{`{"Type":"error"}`, ""},
		{`{"type":5}`, ""},
		{`{"type":null}`, ""},
		{`not json`, ""},
		{``, ""},
		{`{"type":"error"`, ""},
		{`{"type":"error"} {}`, ""},
		{`{"type":"a","type":"error"}`, ""},
	}
	for _, c := range cases {
		if got := (Event{Data: []byte(c.data)}).Type(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.data, got, c.want)
		}
	}
	if got := (Event{}).Type(); got != "" {
		t.Errorf("block without data: got %q", got)
	}
}

func TestWriterFramesEvents(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteEvent("ping", []byte(`{"type": "ping"}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteEvent("", []byte("a\nb")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteComment("keep-alive"); err != nil {
		t.Fatal(err)
	}
	want := "event: ping\ndata: {\"type\": \"ping\"}\n\ndata: a\ndata: b\n\n: keep-alive\n\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestWriterRoundTripsThroughReader(t *testing.T) {
	cases := []struct{ name, data, read string }{
		{"message_start", `{"type":"message_start"}`, `{"type":"message_start"}`},
		{"", "one\r\ntwo\rthree\nfour", "one\ntwo\nthree\nfour"},
		{"trailing", "line\n", "line\n"},
		{"empty", "", ""},
		{"spaced", "  two leading spaces", "  two leading spaces"},
		{"colon", ": not a comment", ": not a comment"},
	}
	var out bytes.Buffer
	writer := NewWriter(&out)
	for _, c := range cases {
		if err := writer.WriteEvent(c.name, []byte(c.data)); err != nil {
			t.Fatal(err)
		}
	}
	events := readAll(t, bytes.NewReader(out.Bytes()))
	if !bytes.Equal(joinRaw(events), out.Bytes()) || len(events) != len(cases) {
		t.Fatalf("got %d blocks from %q", len(events), out.Bytes())
	}
	for i, c := range cases {
		if events[i].Name != c.name || events[i].Data == nil || string(events[i].Data) != c.read {
			t.Errorf("event %d: got %q/%q, want %q/%q", i, events[i].Name, events[i].Data, c.name, c.read)
		}
	}
}

func TestWriterRefusesLineBreaksInNamesAndComments(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteEvent("a\nb", []byte("x")); err == nil {
		t.Error("event name with a line break was written")
	}
	if err := writer.WriteComment("a\rb"); err == nil {
		t.Error("comment with a line break was written")
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q", out.Bytes())
	}
}
