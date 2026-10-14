package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// describeID shows a previous_response_id, with nil as "absent", so that "" and
// absent differ.
func describeID(id *string) string {
	if id == nil {
		return "absent"
	}
	return strconv.Quote(*id)
}

// describeTools shows tools as type/name pairs, with nil as "absent" and an empty
// array as "[]".
func describeTools(tools []Tool) string {
	if tools == nil {
		return "absent"
	}
	pairs := make([]string, len(tools))
	for i, tool := range tools {
		pairs[i] = tool.Type + "/" + tool.Name
	}
	return "[" + strings.Join(pairs, " ") + "]"
}

func TestParseRequestOllamaFixtures(t *testing.T) {
	cases := []struct {
		file, model      string
		stream           bool
		maxOutput, tools string
	}{
		{"ollama-tool-stream.request.json", "qwen3:4b-instruct-2507-q4_K_M", true, "200", "[function/run_tests]"},
		{"ollama-text.request.json", "qwen3:4b-instruct-2507-q4_K_M", false, "40", "absent"},
		{"ollama-text-stream.request.json", "qwen3:4b-instruct-2507-q4_K_M", true, "40", "absent"},
		{"ollama-unknown-model.request.json", "no-such-model", true, "absent", "absent"},
	}
	for _, c := range cases {
		body := readFixture(t, c.file)
		original := bytes.Clone(body)
		req, err := ParseRequest(body)
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		if req.Model != c.model || req.Stream != c.stream || count(req.MaxOutputTokens) != c.maxOutput ||
			describeTools(req.Tools) != c.tools || req.PreviousResponseID != nil {
			t.Errorf("%s: got %q, stream %v, max_output_tokens %s, tools %s, previous_response_id %s", c.file, req.Model,
				req.Stream, count(req.MaxOutputTokens), describeTools(req.Tools), describeID(req.PreviousResponseID))
		}
		if !bytes.Equal(body, original) {
			t.Errorf("%s: ParseRequest changed the body", c.file)
		}
	}
}

// TestParseRequestCodexStyleRequest reads testdata/codex-style.request.json. That file
// is written by hand after the public Responses format, not recorded from Codex; a
// request recorded from Codex 0.162.1 replaces it in issue #66.
func TestParseRequestCodexStyleRequest(t *testing.T) {
	body := readFixture(t, "codex-style.request.json")
	var shape struct {
		Instructions string                     `json:"instructions"`
		Input        []struct{ Type string }    `json:"input"`
		Store        *bool                      `json:"store"`
		Include      []string                   `json:"include"`
		Reasoning    map[string]any             `json:"reasoning"`
		Members      map[string]json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(body, &shape); err != nil || json.Unmarshal(body, &shape.Members) != nil {
		t.Fatalf("the fixture is not a JSON object: %v", err)
	}
	var itemTypes []string
	for _, item := range shape.Input {
		itemTypes = append(itemTypes, item.Type)
	}
	_, hasMax := shape.Members["max_output_tokens"]
	for _, want := range []string{"message", "function_call", "function_call_output"} {
		if !slices.Contains(itemTypes, want) {
			t.Errorf("the fixture's input has no %s item: %v", want, itemTypes)
		}
	}
	if shape.Instructions == "" || shape.Store == nil || *shape.Store || len(shape.Include) == 0 || shape.Reasoning == nil || hasMax {
		t.Error("the fixture lacks instructions, store false, include or reasoning, or has max_output_tokens")
	}

	req, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "qwen3:4b-instruct-2507-q4_K_M" || !req.Stream || req.MaxOutputTokens != nil || req.PreviousResponseID != nil {
		t.Errorf("got %q, stream %v, max_output_tokens %s, previous_response_id %s",
			req.Model, req.Stream, count(req.MaxOutputTokens), describeID(req.PreviousResponseID))
	}
	if got := describeTools(req.Tools); got != "[function/shell function/update_plan]" {
		t.Errorf("tools: %s", got)
	}
	for _, tool := range req.Tools {
		if !bytes.Contains(body, tool.Raw) || !bytes.HasPrefix(tool.Raw, []byte(`{"type":"function"`)) ||
			!bytes.Contains(tool.Raw, []byte(`"parameters":{"type":"object"`)) {
			t.Errorf("%s: Raw is not the tool's bytes in the body: %s", tool.Name, tool.Raw)
		}
	}
}

func TestParseRequestPreviousResponseID(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"model":"m"}`, "absent"},
		{`{"model":"m","previous_response_id":null}`, "absent"},
		{`{"model":"m","previous_response_id":"resp_100792"}`, `"resp_100792"`},
		{`{"model":"m","previous_response_id":""}`, `""`},
		{`{"model":"m","Previous_Response_ID":"resp_1"}`, "absent"},
	}
	for _, c := range cases {
		req, err := ParseRequest([]byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.body, err)
			continue
		}
		if got := describeID(req.PreviousResponseID); got != c.want {
			t.Errorf("%s: got %s, want %s", c.body, got, c.want)
		}
	}
}

func TestParseRequestAbsentAndNullMembers(t *testing.T) {
	cases := []struct {
		body, model      string
		stream           bool
		maxOutput, tools string
	}{
		{`{"model":"m"}`, "m", false, "absent", "absent"},
		{`{"model":"m","stream":false,"max_output_tokens":0,"tools":[]}`, "m", false, "0", "[]"},
		{`{"model":"m","max_output_tokens":-5}`, "m", false, "-5", "absent"},
		{`{"model":null,"stream":null,"max_output_tokens":null,"previous_response_id":null,"tools":null}`, "", false, "absent", "absent"},
		{`{}`, "", false, "absent", "absent"},
		{" \n{\"model\":\"m\",\"stream\":true}\n ", "m", true, "absent", "absent"},
		{`{"Model":"x","MAX_OUTPUT_TOKENS":5,"Stream":true,"Tools":[1],"model":"m"}`, "m", false, "absent", "absent"},
		{`{"model":"m","max_output_tokens":9223372036854775807}`, "m", false, "9223372036854775807", "absent"},
		{`{"model":"m","text":{"max_output_tokens":"x"},"max_output_tokens":7}`, "m", false, "7", "absent"},
		{`{"model":"m","tools":[{"type":"web_search"},{"type":"function","name":"f","parameters":{"name":5}},{}]}`,
			"m", false, "absent", "[web_search/ function/f /]"},
	}
	for _, c := range cases {
		req, err := ParseRequest([]byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.body, err)
			continue
		}
		if req.Model != c.model || req.Stream != c.stream || count(req.MaxOutputTokens) != c.maxOutput ||
			describeTools(req.Tools) != c.tools || req.PreviousResponseID != nil {
			t.Errorf("%s: got %q, %v, %s, %s, %s", c.body, req.Model, req.Stream, count(req.MaxOutputTokens),
				describeTools(req.Tools), describeID(req.PreviousResponseID))
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
		`{"model":"m","stream":"true"}`,
		`{"model":"m","max_output_tokens":1.5}`,
		`{"model":"m","max_output_tokens":"secret-value"}`,
		`{"model":"m","max_output_tokens":1e3}`,
		`{"model":"m","max_output_tokens":9223372036854775808}`,
		`{"model":"m","previous_response_id":5}`,
		`{"model":"m","previous_response_id":{"id":"secret-value"}}`,
		`{"model":"m","tools":{}}`,
		`{"model":"m","tools":"secret-value"}`,
		`{"model":"m","tools":[null]}`,
		`{"model":"m","tools":["secret-value"]}`,
		`{"model":"m","tools":[{"type":5}]}`,
		`{"model":"m","tools":[{"type":"function","name":["secret-value"]}]}`,
		`{"model":"m","tools":[{"type":"function","type":"custom"}]}`,
		`{"model":"secret-value","model":"m"}`,
		`{"model":"m","stream":true,"stream":false}`,
		`{"model":"m","max_output_tokens":1,"max_output_tokens":2}`,
		`{"model":"m","max_output_tokens":null,"max_output_tokens":2}`,
		`{"model":"m","previous_response_id":null,"previous_response_id":"secret-value"}`,
		`{"model":"m","tools":[],"tools":[]}`,
		`{"model":"m","input":[secret-value]}`,
		"{\"model\":\"m\xff\"}",
		"\xef\xbb\xbf{\"model\":\"m\"}",
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

func TestSetMaxOutputTokens(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"absent", `{"model":"m","stream":true}`, `{"model":"m","stream":true,"max_output_tokens":4096}`},
		{"null", `{"model":"m","max_output_tokens":null,"stream":true}`, `{"model":"m","max_output_tokens":4096,"stream":true}`},
		{"value", `{"model":"m","max_output_tokens":200}`, `{"model":"m","max_output_tokens":4096}`},
		{"absent, spaced", "{\n  \"model\": \"m\"\n}\n", "{\n  \"model\": \"m\",\"max_output_tokens\":4096\n}\n"},
		{"null, spaced", "{ \"max_output_tokens\" :  null ,\n \"model\":\"m\" }", "{ \"max_output_tokens\" :  4096 ,\n \"model\":\"m\" }"},
		{"value last", `{"input":"hi","max_output_tokens":1}`, `{"input":"hi","max_output_tokens":4096}`},
		{"other value", `{"max_output_tokens":{"a":[1]},"model":"m"}`, `{"max_output_tokens":4096,"model":"m"}`},
		{"empty object", `{}`, `{"max_output_tokens":4096}`},
		{"empty, spaced", "{ }", `{"max_output_tokens":4096 }`},
		{"nested member", `{"text":{"max_output_tokens":1},"model":"m"}`,
			`{"text":{"max_output_tokens":1},"model":"m","max_output_tokens":4096}`},
		{"case variant", `{"MAX_OUTPUT_TOKENS":1}`, `{"MAX_OUTPUT_TOKENS":1,"max_output_tokens":4096}`},
		{"escaped name", `{"max_output_tokens":null}`, `{"max_output_tokens":4096}`},
	}
	for _, c := range cases {
		body := []byte(c.body)
		got, err := SetMaxOutputTokens(body, 4096)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if string(got) != c.want || string(body) != c.body {
			t.Errorf("%s: got %s, want %s; body now %s", c.name, got, c.want, body)
		}
		if req, err := ParseRequest(got); err != nil || count(req.MaxOutputTokens) != "4096" {
			t.Errorf("%s: ParseRequest reads %s, %v", c.name, count(req.MaxOutputTokens), err)
		}
	}
}

// TestSetMaxOutputTokensKeepsOtherBytes sets max_output_tokens on recorded and
// hand-written requests, and on unknown members that a re-encoding would change: each
// result is the body with only that member's value changed or the member added.
func TestSetMaxOutputTokensKeepsOtherBytes(t *testing.T) {
	future := `{"model":"m","x_future_field":{"nested":[1,2.50,"é",1E2]},"input":"é😀 <tag> &amp;",` +
		`"metadata":{"max_output_tokens":"keep"} , "max_output_tokens" : null }`
	cases := []struct{ name, body, old, new string }{
		{"codex-style, absent", string(readFixture(t, "codex-style.request.json")),
			`"prompt_cache_key":"0199e6a3-7c1d-7b52-9f3e-5a8c2d4e6f10"}`,
			`"prompt_cache_key":"0199e6a3-7c1d-7b52-9f3e-5a8c2d4e6f10","max_output_tokens":4096}`},
		{"ollama, value", string(readFixture(t, "ollama-tool-stream.request.json")),
			`"max_output_tokens":200`, `"max_output_tokens":4096`},
		{"unknown members, null", future, `"max_output_tokens" : null }`, `"max_output_tokens" : 4096 }`},
	}
	for _, c := range cases {
		if strings.Count(c.body, c.old) != 1 {
			t.Fatalf("%s: the body must hold %s once", c.name, c.old)
		}
		got, err := SetMaxOutputTokens([]byte(c.body), 4096)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := strings.Replace(c.body, c.old, c.new, 1); string(got) != want {
			t.Errorf("%s: got\n%s\nwant\n%s", c.name, got, want)
		}
	}
}

func TestSetMaxOutputTokensRejectsMalformedBodies(t *testing.T) {
	bodies := []string{
		``,
		`[]`,
		`null`,
		`"secret-value"`,
		`{"model":"m"`,
		`{"model":"m"} secret-value`,
		`{"model":"m",}`,
		`{"model":[secret-value]}`,
		`{"max_output_tokens":1,"max_output_tokens":null}`,
		"{\"model\":\"m\xff\"}",
		"\xef\xbb\xbf{\"model\":\"m\"}",
	}
	for _, body := range bodies {
		got, err := SetMaxOutputTokens([]byte(body), 4096)
		if !errors.Is(err, ErrMalformed) || got != nil {
			t.Errorf("%q: got %q, %v, want ErrMalformed", body, got, err)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error quotes the input: %v", body, err)
		}
	}
}
