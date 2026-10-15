package relay

import "testing"

// A protocol is named as the internal contracts name it, and a Call that names none
// is a Messages call, as a verify without protocol is.
func TestProtocolNames(t *testing.T) {
	for protocol, want := range map[Protocol]string{
		ProtocolMessages:  "anthropic-messages",
		ProtocolResponses: "openai-responses",
		Protocol(7):       "Protocol(7)",
	} {
		if got := protocol.String(); got != want {
			t.Errorf("Protocol(%d).String() = %q, want %q", int(protocol), got, want)
		}
	}
	var call Call
	if call.Protocol != ProtocolMessages {
		t.Errorf("the zero Call has protocol %s", call.Protocol)
	}
}
