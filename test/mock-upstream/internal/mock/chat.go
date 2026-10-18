package mock

import "time"

// chatModel is the scenario for demos: a readable answer streamed at the pace of a
// real model. Unlike mock-load, its text and timing are for people to read, so load
// tests never depend on them.
const chatModel = "mock-chat"

// chatInterval is the wait between the words of mock-chat: about 28 words, and so
// output tokens, a second.
const chatInterval = 35 * time.Millisecond

// chatAnswer is what mock-chat says. block.parts streams it word by word; the
// newlines stay inside the words, so the paragraphs and the code block arrive intact.
const chatAnswer = "Hello! This answer comes from the FlowGate mock upstream, streamed " +
	"through the real gateway one word at a time.\n\n" +
	"FlowGate is an API gateway for AI coding tools. Every request carries a gateway " +
	"key. The gateway asks the access service whether the key may use the model, " +
	"asks the metering service to reserve the most the call can cost, relays the " +
	"stream from the model channel, and settles the real cost when the stream ends. " +
	"Your balance and usage log in the console change as soon as the call settles.\n\n" +
	"Point any Anthropic Messages client at the gateway:\n\n" +
	"```python\n" +
	"import anthropic\n\n" +
	"client = anthropic.Anthropic(base_url=\"http://localhost:8080\", api_key=\"gf_...\")\n" +
	"with client.messages.stream(model=\"mock-chat\", max_tokens=256,\n" +
	"        messages=[{\"role\": \"user\", \"content\": \"Hello\"}]) as stream:\n" +
	"    for text in stream.text_stream:\n" +
	"        print(text, end=\"\")\n" +
	"```\n\n" +
	"Under load, the gateway scales out on Prometheus signals, and every call is still " +
	"billed exactly once."

// chatTiming paces mock-chat at chatInterval from the start of the stream, so late
// timers do not slow it down.
func (t Timing) chatTiming() Timing {
	t.EventInterval = chatInterval
	t.fixedRate = true
	return t
}
