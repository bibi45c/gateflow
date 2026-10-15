package jsonobj

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errMalformed = errors.New("malformed")

// badObject is an object that a reader must refuse. variant is a name in it as
// decoded that equals a wanted name only under case folding, which no error may
// quote, or "" when it has none.
type badObject struct{ name, data, variant string }

// repeatedMaxTokens holds objects that give max_tokens twice, exactly or under
// another case, or once under another case only. The escaped names are JSON escapes,
// which the decoder resolves before matching. U+017F, the long s, folds to s and
// U+212A, the Kelvin sign, folds to k.
var repeatedMaxTokens = []badObject{
	{"exact name twice", `{"max_tokens":1,"max_tokens":2}`, ""},
	{"exact and escaped name", "{\"max_tokens\":1,\"max\\u005ftokens\":2}", ""},
	{"variant after the name", `{"max_tokens":16,"MAX_TOKENS":120}`, "MAX_TOKENS"},
	{"variant before the name", `{"MAX_TOKENS":120,"max_tokens":16}`, "MAX_TOKENS"},
	{"variant alone", `{"model":"m","Max_Tokens":120}`, "Max_Tokens"},
	{"escaped variant", "{\"max_tokens\":16,\"\\u004dAX_TOKENS\":120}", "MAX_TOKENS"},
	{"long s", "{\"max_tokens\":16,\"max_token\U0000017f\":120}", "max_token\U0000017f"},
	{"escaped long s", "{\"max_token\\u017f\":120,\"max_tokens\":16}", "max_token\U0000017f"},
	{"Kelvin sign", "{\"max_to\U0000212aens\":120,\"max_tokens\":16}", "max_to\U0000212aens"},
	{"escaped Kelvin sign", "{\"max_tokens\":16,\"max_to\\u212aens\":120}", "max_to\U0000212aens"},
}

// decode reads model and max_tokens from data and shows them, with nil as "absent".
func decode(data string) (string, error) {
	var model string
	var maxTokens *int64
	err := DecodeMembers([]byte(data), map[string]any{"model": &model, "max_tokens": &maxTokens}, errMalformed)
	if err != nil {
		return "", err
	}
	if maxTokens == nil {
		return fmt.Sprintf("%q absent", model), nil
	}
	return fmt.Sprintf("%q %d", model, *maxTokens), nil
}

func TestDecodeMembersMatchesNamesExactly(t *testing.T) {
	cases := []struct{ name, data, want string }{
		{"exact names", `{"model":"m","max_tokens":5}`, `"m" 5`},
		{"escaped names", "{\"mod\\u0065l\":\"m\",\"max\\u005ftokens\":5}", `"m" 5`},
		{"absent", `{}`, `"" absent`},
		{"unknown members", `{"models":"x","max_token":1,"max-tokens":2,"model":"m"}`, `"m" absent`},
		{"unknown members in other cases", `{"Metadata":{},"METADATA":[],"model":"m"}`, `"m" absent`},
		{"unknown member twice", `{"note":1,"note":2,"model":"m"}`, `"m" absent`},
		{"nested members in other cases", `{"meta":{"MODEL":"x","Max_Tokens":1},"model":"m"}`, `"m" absent`},
	}
	for _, c := range cases {
		got, err := decode(c.data)
		if err != nil || got != c.want {
			t.Errorf("%s: got %s, %v, want %s", c.name, got, err, c.want)
		}
	}
}

func TestDecodeMembersRejectsRepeatsAndCaseVariants(t *testing.T) {
	cases := append([]badObject{
		{"variant of a string member", `{"model":"m","mODEL":"x"}`, "mODEL"},
		{"variant of a null member", `{"Model":null}`, "Model"},
	}, repeatedMaxTokens...)
	for _, c := range cases {
		_, err := decode(c.data)
		if !errors.Is(err, errMalformed) {
			t.Errorf("%s: got %v, want errMalformed", c.name, err)
			continue
		}
		if c.variant != "" && strings.Contains(err.Error(), c.variant) {
			t.Errorf("%s: the error quotes the input: %v", c.name, err)
		}
	}
}

func TestFindMember(t *testing.T) {
	cases := []struct{ name, data, value string }{
		{"exact name", `{"model":"m","max_tokens":5}`, "5"},
		{"escaped name", "{\"max\\u005ftokens\" : null }", "null"},
		{"absent", `{"model":"m"}`, ""},
		{"unknown members in other cases", `{"model":"m","MODEL":"x","Metadata":1,"METADATA":2}`, ""},
		{"unknown member twice", `{"note":1,"note":2,"max_tokens":5}`, "5"},
		{"nested member in another case", `{"meta":{"MAX_TOKENS":1},"max_tokens":5}`, "5"},
	}
	for _, c := range cases {
		span, err := FindMember([]byte(c.data), "max_tokens", errMalformed)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if span.Found != (c.value != "") || span.Found && c.data[span.Start:span.End] != c.value {
			t.Errorf("%s: got %+v, want the value %q", c.name, span, c.value)
		}
	}
}

func TestFindMemberRejectsRepeatsAndCaseVariants(t *testing.T) {
	for _, c := range repeatedMaxTokens {
		span, err := FindMember([]byte(c.data), "max_tokens", errMalformed)
		if !errors.Is(err, errMalformed) || span != (Span{}) {
			t.Errorf("%s: got %+v, %v, want errMalformed", c.name, span, err)
			continue
		}
		if c.variant != "" && strings.Contains(err.Error(), c.variant) {
			t.Errorf("%s: the error quotes the input: %v", c.name, err)
		}
	}
}
