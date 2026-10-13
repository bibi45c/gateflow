package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
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

// recordedStream returns testdata/stream.sse with each "\n" replaced by ending. The
// file itself uses "\n", which .gitattributes keeps on every checkout, so the "\r\n"
// and "\r" versions are made here.
func recordedStream(t *testing.T, ending string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(data, '\r') {
		t.Fatal("testdata/stream.sse must end its lines with \\n only")
	}
	return bytes.ReplaceAll(data, []byte("\n"), []byte(ending))
}

// recordedBlocks lists the blocks of the recorded stream: event: field and data type.
var recordedBlocks = []struct{ name, eventType string }{
	{"", ""}, // a comment block
	{EventMessageStart, EventMessageStart},
	{EventPing, EventPing},
	{EventContentBlockStart, EventContentBlockStart},
	{EventContentBlockDelta, EventContentBlockDelta},
	{"", ""}, // keep-alive
	{EventContentBlockDelta, EventContentBlockDelta},
	{EventContentBlockStop, EventContentBlockStop},
	{EventContentBlockStart, EventContentBlockStart},
	{EventContentBlockDelta, EventContentBlockDelta},
	{EventContentBlockDelta, EventContentBlockDelta},
	{EventContentBlockDelta, EventContentBlockDelta}, // a comment line and three data lines
	{EventContentBlockStop, EventContentBlockStop},
	{EventMessageDelta, EventMessageDelta},
	{EventMessageStop, EventMessageStop},
}

func TestReaderRoundTripsRecordedStream(t *testing.T) {
	reference := readAll(t, bytes.NewReader(recordedStream(t, "\n")))
	for _, ending := range []struct{ label, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		stream := recordedStream(t, ending.value)
		for _, size := range []int{1, 7, len(stream)} {
			t.Run(ending.label+"/"+strconv.Itoa(size), func(t *testing.T) {
				events := readAll(t, &chunkReader{data: stream, size: size})
				if !bytes.Equal(joinRaw(events), stream) {
					t.Fatal("the blocks' Raw joined differ from the stream")
				}
				if len(events) != len(recordedBlocks) {
					t.Fatalf("got %d blocks, want %d", len(events), len(recordedBlocks))
				}
				for i, want := range recordedBlocks {
					if events[i].Name != want.name || events[i].Type() != want.eventType {
						t.Errorf("block %d: got %q/%q, want %q/%q", i, events[i].Name, events[i].Type(), want.name, want.eventType)
					}
					if !bytes.Equal(events[i].Data, reference[i].Data) || (events[i].Data == nil) != (reference[i].Data == nil) {
						t.Errorf("block %d: data %q, want %q", i, events[i].Data, reference[i].Data)
					}
				}
			})
		}
	}
}

func TestReaderKeepsCommentsAndUnknownFields(t *testing.T) {
	events := readAll(t, bytes.NewReader(recordedStream(t, "\n")))
	if got := string(events[5].Raw); got != ": keep-alive\n\n" || events[5].Data != nil {
		t.Errorf("keep-alive: Raw %q, Data %q", got, events[5].Data)
	}
	if !bytes.HasPrefix(events[11].Raw, []byte(": a comment line inside an event\nevent: ")) {
		t.Errorf("comment line inside an event lost: %q", events[11].Raw)
	}
	start := events[1]
	if !bytes.Equal(start.Raw, append(append([]byte("event: message_start\ndata: "), start.Data...), "\n\n"...)) {
		t.Errorf("message_start data is not its data line byte for byte: %q", start.Data)
	}
	for _, unknown := range []string{`"service_tier":"standard"`, `"x_future_field":{"nested":[1,2.50,"é"]}`} {
		if !bytes.Contains(start.Data, []byte(unknown)) {
			t.Errorf("message_start data lost %s", unknown)
		}
	}
}

func TestReaderJoinsMultiLineData(t *testing.T) {
	events := readAll(t, bytes.NewReader(recordedStream(t, "\r\n")))
	want := "{\"type\":\"content_block_delta\",\n\"index\":1,\n" +
		`"delta":{"type":"input_json_delta","partial_json":"is, France\"}"}}`
	if got := string(events[11].Data); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReaderGivesToolUseDeltasInOrder(t *testing.T) {
	var partial strings.Builder
	for _, event := range readAll(t, &chunkReader{data: recordedStream(t, "\n"), size: 7}) {
		var delta struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if event.Type() != EventContentBlockDelta {
			continue
		}
		if err := json.Unmarshal(event.Data, &delta); err != nil {
			t.Fatalf("%q: %v", event.Data, err)
		}
		if delta.Delta.Type == "input_json_delta" && delta.Index == 1 {
			partial.WriteString(delta.Delta.PartialJSON)
		}
	}
	if got := partial.String(); got != `{"location": "Paris, France"}` {
		t.Errorf("tool input: got %q", got)
	}
}

func TestReaderReadingFieldsKeepsBytes(t *testing.T) {
	stream := recordedStream(t, "\r\n")
	events := readAll(t, &chunkReader{data: bytes.Clone(stream), size: 7})
	var data [][]byte
	for _, event := range events {
		data = append(data, bytes.Clone(event.Data))
	}
	for _, event := range events {
		var err error
		switch event.Type() {
		case EventMessageStart:
			_, err = MessageStartUsage(event.Data)
		case EventMessageDelta:
			_, err = MessageDeltaUsage(event.Data)
		}
		if err != nil {
			t.Errorf("%s: %v", event.Name, err)
		}
	}
	if !bytes.Equal(joinRaw(events), stream) {
		t.Error("reading the typed fields changed the bytes to forward")
	}
	for i, event := range events {
		if !bytes.Equal(event.Data, data[i]) {
			t.Errorf("block %d: reading the typed fields changed its data", i)
		}
	}
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

func TestWriterFramesEvents(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	if err := writer.WriteEvent(EventPing, []byte(`{"type": "ping"}`)); err != nil {
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
		{EventMessageStart, `{"type":"message_start"}`, `{"type":"message_start"}`},
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

func TestWriterReframesRecordedStream(t *testing.T) {
	var recorded []Event
	for _, event := range readAll(t, bytes.NewReader(recordedStream(t, "\r\n"))) {
		if event.Data != nil {
			recorded = append(recorded, event)
		}
	}
	var out bytes.Buffer
	writer := NewWriter(&out)
	for _, event := range recorded {
		if err := writer.WriteEvent(event.Name, event.Data); err != nil {
			t.Fatal(err)
		}
	}
	again := readAll(t, &chunkReader{data: out.Bytes(), size: 7})
	if len(again) != len(recorded) {
		t.Fatalf("got %d events, want %d", len(again), len(recorded))
	}
	for i := range recorded {
		if again[i].Name != recorded[i].Name || !bytes.Equal(again[i].Data, recorded[i].Data) {
			t.Errorf("event %d: got %q/%q, want %q/%q", i, again[i].Name, again[i].Data, recorded[i].Name, recorded[i].Data)
		}
	}
	if start := string(again[0].Raw); !strings.HasPrefix(start, "event: message_start\ndata: {") || !strings.HasSuffix(start, "}}\n\n") {
		t.Errorf("message_start framing: %q", start)
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
