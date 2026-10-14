package anthropic

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
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
	return fmt.Sprintf("input=%s output=%s cache_read=%s cache_creation=%s", count(u.InputTokens),
		count(u.OutputTokens), count(u.CacheReadInputTokens), count(u.CacheCreationInputTokens))
}

func TestParseRequestReadsFieldsAndLeavesBodyAlone(t *testing.T) {
	body := readFixture(t, "request.json")
	original := bytes.Clone(body)
	req, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "mock-small" || count(req.MaxTokens) != "1024" || !req.Stream {
		t.Errorf("got model %q, max_tokens %s, stream %v", req.Model, count(req.MaxTokens), req.Stream)
	}
	if !bytes.Equal(body, original) {
		t.Error("ParseRequest changed the body")
	}
}

func TestParseRequestAbsentAndNullMembers(t *testing.T) {
	cases := []struct {
		body, model, maxTokens string
		stream                 bool
	}{
		{`{"model":"m"}`, "m", "absent", false},
		{`{"model":"m","max_tokens":0,"stream":false}`, "m", "0", false},
		{`{"model":"m","max_tokens":-5}`, "m", "-5", false},
		{`{"model":null,"max_tokens":null,"stream":null}`, "", "absent", false},
		{`{}`, "", "absent", false},
		{" \n{\"model\":\"m\",\"stream\":true}\n ", "m", "absent", true},
		{`{"Metadata":{},"METADATA":[],"model":"m","max_tokens_x":5}`, "m", "absent", false},
		{`{"model":"m","max_tokens":9223372036854775807}`, "m", "9223372036854775807", false},
	}
	for _, c := range cases {
		req, err := ParseRequest([]byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.body, err)
			continue
		}
		if req.Model != c.model || count(req.MaxTokens) != c.maxTokens || req.Stream != c.stream {
			t.Errorf("%s: got %q, %s, %v", c.body, req.Model, count(req.MaxTokens), req.Stream)
		}
	}
}

func TestParseRequestRejectsMalformedBodies(t *testing.T) {
	bodies := []string{
		``,
		`secret-value`,
		`[]`,
		`null`,
		`"secret-value"`,
		`{"model":"m"`,
		`{"model":"m"} {}`,
		`{"model":"m"}secret-value`,
		`{"model":"m",}`,
		`{"model":5}`,
		`{"model":["secret-value"]}`,
		`{"model":"m","max_tokens":1.5}`,
		`{"model":"m","max_tokens":"secret-value"}`,
		`{"model":"m","max_tokens":1e3}`,
		`{"model":"m","max_tokens":9223372036854775808}`,
		`{"model":"m","stream":"true"}`,
		`{"model":"secret-value","model":"m"}`,
		`{"model":"m","max_tokens":1,"max_tokens":2}`,
		`{"model":"m","stream":true,"stream":false}`,
		`{"model":"m","messages":[secret-value]}`,
		"{\"model\":\"m\xff\"}",
		"\xef\xbb\xbf{\"model\":\"m\"}",
		`{"Model":"x","MAX_TOKENS":5,"Stream":true,"model":"m"}`,
	}
	for _, body := range bodies {
		_, err := ParseRequest([]byte(body))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: got %v, want ErrMalformed", body, err)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error quotes the input: %v", body, err)
		}
	}
}

// A name that equals model, max_tokens or stream only under case folding makes the
// body malformed, before or after the exact name or alone: an upstream that matches
// names without case could read a value the gateway did not. U+212A, the Kelvin sign,
// folds to k and U+017F, the long s, to s.
func TestParseRequestRejectsCaseVariants(t *testing.T) {
	bodies := []string{
		`{"model":"m","Model":"secret-value"}`,
		`{"MODEL":"secret-value","model":"m","max_tokens":16}`,
		`{"mODEL":"secret-value","max_tokens":16}`,
		`{"model":"m","max_tokens":16,"MAX_TOKENS":120}`,
		`{"model":"m","Max_Tokens":120,"max_tokens":16}`,
		`{"model":"m","MAX_TOKENS":120}`,
		"{\"model\":\"m\",\"max_tokens\":16,\"MAX\\u005fTOKENS\":120}",
		"{\"model\":\"m\",\"max_tokens\":16,\"max_to\U0000212aens\":120}",
		"{\"model\":\"m\",\"max_to\\u212aens\":120,\"max_tokens\":16}",
		`{"model":"m","max_tokens":16,"stream":false,"Stream":true}`,
		`{"model":"m","STREAM":true,"stream":false,"max_tokens":16}`,
		`{"model":"m","max_tokens":16,"sTREAM":true}`,
		"{\"model\":\"m\",\"max_tokens\":16,\"\U0000017ftream\":true}",
		"{\"model\":\"m\",\"\\u017ftream\":true,\"stream\":false}",
	}
	for _, body := range bodies {
		_, err := ParseRequest([]byte(body))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: got %v, want ErrMalformed", body, err)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error quotes the input: %v", body, err)
		}
	}
}

func TestUsageFromRecordedStream(t *testing.T) {
	events := readAll(t, bytes.NewReader(recordedStream(t, "\n")))
	start, err := MessageStartUsage(events[1].Data)
	if err != nil {
		t.Fatal(err)
	}
	if got := describe(start); got != "input=472 output=1 cache_read=128 cache_creation=0" {
		t.Errorf("message_start: %s", got)
	}
	delta, err := MessageDeltaUsage(events[13].Data)
	if err != nil {
		t.Fatal(err)
	}
	if got := describe(delta); got != "input=472 output=89 cache_read=128 cache_creation=0" {
		t.Errorf("message_delta: %s", got)
	}
}

func TestUsageTellsZeroFromAbsent(t *testing.T) {
	cases := []struct {
		name string
		read func([]byte) (Usage, error)
		data string
		want string
	}{
		{"response", ResponseUsage, string(readFixture(t, "message.json")),
			"input=0 output=0 cache_read=absent cache_creation=12"},
		{"response without usage", ResponseUsage, `{"type":"message","content":[]}`,
			"input=absent output=absent cache_read=absent cache_creation=absent"},
		{"null usage", ResponseUsage, `{"type":"message","usage":null}`,
			"input=absent output=absent cache_read=absent cache_creation=absent"},
		{"delta with output only", MessageDeltaUsage, `{"type":"message_delta","delta":{},"usage":{"output_tokens":0}}`,
			"input=absent output=0 cache_read=absent cache_creation=absent"},
		{"delta with cache counts", MessageDeltaUsage,
			`{"type":"message_delta","usage":{"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":null}}`,
			"input=absent output=5 cache_read=0 cache_creation=absent"},
		{"delta without usage", MessageDeltaUsage, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			"input=absent output=absent cache_read=absent cache_creation=absent"},
		{"start with zero input", MessageStartUsage,
			`{"type":"message_start","message":{"usage":{"input_tokens":0,"cache_creation_input_tokens":0}}}`,
			"input=0 output=absent cache_read=absent cache_creation=0"},
		{"start without message", MessageStartUsage, `{"type":"message_start"}`,
			"input=absent output=absent cache_read=absent cache_creation=absent"},
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
		{"start reader on a delta", MessageStartUsage, `{"type":"message_delta","usage":{"output_tokens":1}}`},
		{"delta reader on a start", MessageDeltaUsage, `{"type":"message_start","message":{}}`},
		{"string count", MessageDeltaUsage, `{"type":"message_delta","usage":{"output_tokens":"5"}}`},
		{"fractional count", ResponseUsage, `{"usage":{"input_tokens":1.5}}`},
		{"repeated count", MessageDeltaUsage, `{"type":"message_delta","usage":{"output_tokens":1,"output_tokens":2}}`},
		{"repeated usage", ResponseUsage, `{"usage":{},"usage":{"input_tokens":1}}`},
		{"usage not an object", ResponseUsage, `{"usage":[1]}`},
		{"message not an object", MessageStartUsage, `{"type":"message_start","message":"text"}`},
		{"truncated", MessageDeltaUsage, `{"type":"message_delta","usage":{"output_tokens":1}`},
	}
	for _, c := range cases {
		if _, err := c.read([]byte(c.data)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", c.name, err)
		}
	}
}

func TestEventType(t *testing.T) {
	cases := []struct{ data, want string }{
		{`{"type":"message_stop"}`, EventMessageStop},
		{`{"index":0, "type" : "ping"}`, EventPing},
		{`{"Type":"message_stop"}`, ""},
		{`{"type":5}`, ""},
		{`not json`, ""},
		{``, ""},
		{`{"type":"message_stop"`, ""},
		{`{"type":"a","type":"message_stop"}`, ""},
	}
	for _, c := range cases {
		if got := (Event{Data: []byte(c.data)}).Type(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.data, got, c.want)
		}
	}
}
