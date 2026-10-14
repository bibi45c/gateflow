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
)

// The framing rules themselves are tested in pkg/protocol/sse. The tests here read a
// recorded Messages stream through this package's names for that framing.

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
