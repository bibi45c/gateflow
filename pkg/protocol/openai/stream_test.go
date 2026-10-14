package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/bibi45c/gateflow/pkg/protocol/sse"
)

// chunkReader returns at most size bytes per Read, so that events arrive split
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

// readAll reads every event of a stream and fails the test on any error but io.EOF.
func readAll(t *testing.T, src io.Reader) []sse.Event {
	t.Helper()
	reader := sse.NewReader(src)
	var events []sse.Event
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("event %d: %v", len(events), err)
		}
		events = append(events, event)
	}
}

func joinRaw(events []sse.Event) []byte {
	var joined []byte
	for _, event := range events {
		joined = append(joined, event.Raw...)
	}
	return joined
}

// readFixture returns a file of testdata. The files end their lines with "\n" only,
// which .gitattributes keeps on every checkout.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(data, '\r') {
		t.Fatalf("testdata/%s must end its lines with \\n only", name)
	}
	return data
}

// count shows a token count, with nil as "absent", so that 0 and absent differ.
func count(n *int64) string {
	if n == nil {
		return "absent"
	}
	return fmt.Sprint(*n)
}

func describe(u Usage) string {
	return fmt.Sprintf("input=%s output=%s total=%s cached=%s reasoning=%s", count(u.InputTokens),
		count(u.OutputTokens), count(u.TotalTokens), count(u.CachedTokens), count(u.ReasoningTokens))
}

func TestFixturesAreKeptAsRecorded(t *testing.T) {
	for name, ending := range map[string]string{
		"ollama-tool-stream.sse":             "}\n\n",
		"ollama-text-stream.sse":             "}\n\n",
		"ollama-tool-stream.request.json":    "]}\n",
		"ollama-text.response.json":          "null}\n",
		"ollama-unknown-model.request.json":  `"hi"}`,
		"ollama-unknown-model.response.json": "null}}\n",
	} {
		if data := readFixture(t, name); !bytes.HasSuffix(data, []byte(ending)) {
			t.Errorf("%s: does not end with %q as recorded", name, ending)
		}
	}
}

func TestOllamaStreamsParseEventByEvent(t *testing.T) {
	cases := []struct {
		file  string
		types []string
		usage string
	}{
		{"ollama-tool-stream.sse", []string{EventCreated, EventInProgress, EventOutputItemAdded,
			EventFunctionCallArgumentsDelta, EventFunctionCallArgumentsDone, EventOutputItemDone, EventCompleted},
			"input=157 output=19 total=176 cached=0 reasoning=0"},
		{"ollama-text-stream.sse", []string{EventCreated, EventInProgress, EventOutputItemAdded,
			EventContentPartAdded, EventOutputTextDelta, EventOutputTextDelta, EventOutputTextDone,
			EventContentPartDone, EventOutputItemDone, EventCompleted},
			"input=11 output=3 total=14 cached=10 reasoning=0"},
	}
	for _, c := range cases {
		stream := readFixture(t, c.file)
		for _, size := range []int{1, 7, len(stream)} {
			t.Run(c.file+"/"+strconv.Itoa(size), func(t *testing.T) {
				events := readAll(t, &chunkReader{data: stream, size: size})
				if !bytes.Equal(joinRaw(events), stream) {
					t.Fatal("the events' Raw joined differ from the stream")
				}
				if len(events) != len(c.types) {
					t.Fatalf("got %d events, want %d", len(events), len(c.types))
				}
				for i, want := range c.types {
					event := events[i]
					if event.Name != want || event.Type() != want {
						t.Errorf("event %d: got %q/%q, want %q", i, event.Name, event.Type(), want)
					}
					kind, end := EndOf(event.Type())
					if last := i == len(c.types)-1; end != last || (last && kind != EndCompleted) {
						t.Errorf("event %d: EndOf gives %v, %v", i, kind, end)
					}
				}
				usage, err := EventUsage(events[len(events)-1].Data)
				if err != nil {
					t.Fatal(err)
				}
				if got := describe(usage); got != c.usage {
					t.Errorf("usage: got %s, want %s", got, c.usage)
				}
			})
		}
	}
}

func TestOllamaStreamsCarryTheirOutput(t *testing.T) {
	var deltas struct {
		Delta     string `json:"delta"`
		Arguments string `json:"arguments"`
		Text      string `json:"text"`
	}
	var arguments, text strings.Builder
	var done []string
	for _, file := range []string{"ollama-tool-stream.sse", "ollama-text-stream.sse"} {
		for _, event := range readAll(t, bytes.NewReader(readFixture(t, file))) {
			deltas.Delta, deltas.Arguments, deltas.Text = "", "", ""
			if err := json.Unmarshal(event.Data, &deltas); err != nil {
				t.Fatalf("%s: %v", event.Name, err)
			}
			switch event.Type() {
			case EventFunctionCallArgumentsDelta:
				arguments.WriteString(deltas.Delta)
			case EventOutputTextDelta:
				text.WriteString(deltas.Delta)
			case EventFunctionCallArgumentsDone:
				done = append(done, deltas.Arguments)
			case EventOutputTextDone:
				done = append(done, deltas.Text)
			}
		}
	}
	if arguments.String() != `{"path":"."}` || text.String() != "OK." {
		t.Errorf("deltas: arguments %q, text %q", arguments.String(), text.String())
	}
	if strings.Join(done, "|") != `{"path":"."}|OK.` {
		t.Errorf("done events: %q", done)
	}
}

// TestReadingAndReframingKeepBytes reads every typed field of a recorded stream, then
// writes each event again: the stream, with all the members this package does not
// read, comes out byte for byte.
func TestReadingAndReframingKeepBytes(t *testing.T) {
	stream := readFixture(t, "ollama-tool-stream.sse")
	events := readAll(t, &chunkReader{data: bytes.Clone(stream), size: 7})
	for _, event := range events {
		if _, end := EndOf(event.Type()); end {
			if _, err := EventUsage(event.Data); err != nil {
				t.Errorf("%s: %v", event.Name, err)
			}
		}
	}
	var out bytes.Buffer
	writer := sse.NewWriter(&out)
	for _, event := range events {
		if err := writer.WriteEvent(event.Name, event.Data); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(joinRaw(events), stream) || !bytes.Equal(out.Bytes(), stream) {
		t.Error("reading or reframing the events changed the bytes to forward")
	}
	completed := events[len(events)-1].Data
	for _, unknown := range []string{`"service_tier":"default"`, `"safety_identifier":null`,
		`"text":{"format":{"type":"text"}}`, `"arguments":"{\"path\":\".\"}"`} {
		if !bytes.Contains(completed, []byte(unknown)) {
			t.Errorf("response.completed lost %s", unknown)
		}
	}
}

func TestResponseUsageFromOllamaBody(t *testing.T) {
	usage, err := ResponseUsage(readFixture(t, "ollama-text.response.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := describe(usage); got != "input=11 output=12 total=23 cached=1 reasoning=0" {
		t.Errorf("got %s", got)
	}
}

func TestUsageTellsZeroFromAbsent(t *testing.T) {
	cases := []struct {
		name string
		read func([]byte) (Usage, error)
		data string
		want string
	}{
		{"completed with zeros", EventUsage, `{"type":"response.completed","response":{"usage":{"input_tokens":0,` +
			`"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`,
			"input=0 output=0 total=0 cached=0 reasoning=0"},
		{"incomplete without details", EventUsage, `{"type":"response.incomplete","response":{"usage":{"input_tokens":800,"output_tokens":100}}}`,
			"input=800 output=100 total=absent cached=absent reasoning=absent"},
		{"failed with null usage", EventUsage, `{"type":"response.failed","response":{"status":"failed","usage":null}}`,
			"input=absent output=absent total=absent cached=absent reasoning=absent"},
		{"completed without response", EventUsage, `{"type":"response.completed","sequence_number":3}`,
			"input=absent output=absent total=absent cached=absent reasoning=absent"},
		{"null response", EventUsage, `{"type":"response.failed","response":null}`,
			"input=absent output=absent total=absent cached=absent reasoning=absent"},
		{"null and empty details", ResponseUsage, `{"usage":{"input_tokens":5,"input_tokens_details":null,"output_tokens_details":{}}}`,
			"input=5 output=absent total=absent cached=absent reasoning=absent"},
		{"null counts", ResponseUsage, `{"usage":{"input_tokens":null,"input_tokens_details":{"cached_tokens":null},"total_tokens":7}}`,
			"input=absent output=absent total=7 cached=absent reasoning=absent"},
		{"body without usage", ResponseUsage, `{"id":"resp_1","status":"completed","output":[]}`,
			"input=absent output=absent total=absent cached=absent reasoning=absent"},
		{"case variants are other members", ResponseUsage, `{"Usage":{"input_tokens":9},"usage":{"Input_Tokens":9,"output_tokens":1}}`,
			"input=absent output=1 total=absent cached=absent reasoning=absent"},
	}
	for _, c := range cases {
		usage, err := c.read([]byte(c.data))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := describe(usage); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestUsageRejectsMalformedData(t *testing.T) {
	cases := []struct {
		name string
		read func([]byte) (Usage, error)
		data string
	}{
		{"created event", EventUsage, `{"type":"response.created","response":{"usage":null}}`},
		{"error event", EventUsage, `{"type":"error","code":"api_error","message":"failed"}`},
		{"no type", EventUsage, `{"response":{"usage":{"input_tokens":1}}}`},
		{"repeated type", EventUsage, `{"type":"response.created","type":"response.completed","response":{}}`},
		{"repeated response", EventUsage, `{"type":"response.completed","response":{},"response":{"usage":{"input_tokens":1}}}`},
		{"response not an object", EventUsage, `{"type":"response.completed","response":"done"}`},
		{"string count", ResponseUsage, `{"usage":{"output_tokens":"5"}}`},
		{"fractional count", ResponseUsage, `{"usage":{"input_tokens":1.5}}`},
		{"repeated count", ResponseUsage, `{"usage":{"output_tokens":1,"output_tokens":2}}`},
		{"repeated usage", ResponseUsage, `{"usage":{},"usage":{"input_tokens":1}}`},
		{"repeated cached count", ResponseUsage, `{"usage":{"input_tokens_details":{"cached_tokens":1,"cached_tokens":2}}}`},
		{"details not an object", ResponseUsage, `{"usage":{"input_tokens_details":[1]}}`},
		{"string detail count", ResponseUsage, `{"usage":{"output_tokens_details":{"reasoning_tokens":"1"}}}`},
		{"usage not an object", ResponseUsage, `{"usage":[1]}`},
		{"truncated", EventUsage, `{"type":"response.completed","response":{"usage":{"output_tokens":1}}`},
	}
	for _, c := range cases {
		if _, err := c.read([]byte(c.data)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", c.name, err)
		}
	}
}

func TestEndOf(t *testing.T) {
	cases := []struct {
		name string
		kind EndKind
		end  bool
	}{
		{EventCompleted, EndCompleted, true},
		{EventIncomplete, EndIncomplete, true},
		{EventFailed, EndFailed, true},
		{EventCreated, 0, false},
		{EventInProgress, 0, false},
		{EventOutputItemDone, 0, false},
		{EventError, 0, false},
		{"Response.Completed", 0, false},
		{"response.completed ", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		if kind, end := EndOf(c.name); kind != c.kind || end != c.end {
			t.Errorf("%q: got %v, %v, want %v, %v", c.name, kind, end, c.kind, c.end)
		}
	}
}

// largeRawSize is the size of the large event, just under 1 MiB, the gateway's limit
// on one event.
const largeRawSize = 1<<20 - 512

// largeCompletedEvent returns a response.completed event of largeRawSize bytes whose
// output holds a long text and whose response echoes 300 tools, and its data.
func largeCompletedEvent() (raw, data []byte) {
	tools := make([]string, 300)
	for i := range tools {
		tools[i] = fmt.Sprintf(`{"type":"function","name":"tool_%03d","description":"%s",`+
			`"parameters":{"type":"object","properties":{"path":{"type":"string","description":"%s"}},"required":["path"]}}`,
			i, strings.Repeat("Reads a file. ", 30), strings.Repeat("A path in the project. ", 20))
	}
	const format = `{"response":{"id":"resp_large","object":"response","status":"completed","model":"qwen3:4b-instruct-2507-q4_K_M",` +
		`"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool_007","arguments":"{\"path\":\".\"}","status":"completed"},` +
		`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","annotations":[],"text":"%s"}]}],` +
		`"tools":[%s],"usage":{"input_tokens":250000,"input_tokens_details":{"cached_tokens":4096},"output_tokens":30000,` +
		`"output_tokens_details":{"reasoning_tokens":1200},"total_tokens":280000}},"sequence_number":4242,"type":"response.completed"}`
	const head, tail = "event: response.completed\ndata: ", "\n\n"
	joined := strings.Join(tools, ",")
	free := largeRawSize - len(head) - len(fmt.Sprintf(format, "", joined)) - len(tail)
	// Each unit holds JSON escapes and multi-byte characters: fmt.Println("é 😊")
	// and a line break.
	unit := `fmt.Println(\"é 😊\")\n`
	text := strings.Repeat(unit, free/len(unit))
	text += strings.Repeat("x", free-len(text))
	data = []byte(fmt.Sprintf(format, text, joined))
	return []byte(head + string(data) + tail), data
}

func TestLargeCompletedEvent(t *testing.T) {
	raw, data := largeCompletedEvent()
	if len(raw) != largeRawSize || !json.Valid(data) {
		t.Fatalf("the large event has %d bytes, want %d of valid JSON data", len(raw), largeRawSize)
	}
	created := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n")
	stream := append(bytes.Clone(created), raw...)
	for _, chunk := range []int{1000, 4096, 1 << 16} {
		t.Run(strconv.Itoa(chunk), func(t *testing.T) {
			events := readAll(t, &chunkReader{data: stream, size: chunk})
			if len(events) != 2 || !bytes.Equal(joinRaw(events), stream) {
				t.Fatalf("got %d events; their Raw joined differ from the stream", len(events))
			}
			large := events[1]
			if !bytes.Equal(large.Raw, raw) || !bytes.Equal(large.Data, data) {
				t.Fatalf("large event: Raw of %d bytes, data of %d bytes", len(large.Raw), len(large.Data))
			}
			if kind, end := EndOf(large.Type()); large.Name != EventCompleted || kind != EndCompleted || !end {
				t.Errorf("large event: %q/%q gives %v, %v", large.Name, large.Type(), kind, end)
			}
			usage, err := EventUsage(large.Data)
			if err != nil {
				t.Fatal(err)
			}
			if got := describe(usage); got != "input=250000 output=30000 total=280000 cached=4096 reasoning=1200" {
				t.Errorf("usage: %s", got)
			}
			if !bytes.Equal(large.Raw, raw) || !bytes.Equal(large.Data, data) {
				t.Error("reading the usage changed the event")
			}
		})
	}
}
