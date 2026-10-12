package anthropic

import (
	"encoding/json"
	"testing"
)

func TestNewErrorBodyEncodesTheAnthropicShape(t *testing.T) {
	encoded, err := json.Marshal(NewErrorBody(ErrorTypeBilling, "Insufficient balance."))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"error","error":{"type":"billing_error","message":"Insufficient balance."}}`
	if string(encoded) != want {
		t.Errorf("got %s, want %s", encoded, want)
	}
}

func TestErrorBodyDecodesAnUpstreamError(t *testing.T) {
	var body ErrorBody
	data := `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_1"}`
	if err := json.Unmarshal([]byte(data), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Type != "overloaded_error" || body.Error.Message != "Overloaded" {
		t.Errorf("got %+v", body)
	}
}
