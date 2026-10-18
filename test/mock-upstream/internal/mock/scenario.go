package mock

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// Stop reasons of a reply.
const (
	stopEndTurn = "end_turn"
	stopToolUse = "tool_use"
)

// Fixed texts of the scenarios.
const (
	textAnswer        = "Hello from the mock upstream. This answer is the same every time."
	codingAnswer      = "I changed add in calc.py to return a + b, and the tests now pass."
	overloadedMessage = "Overloaded"
	// codingNoToolsAnswer is what mock-coding says to a request without tools.
	codingNoToolsAnswer = "Mock coding session"
	// longAnswer has many words, so that the failure scenarios cut it mid-stream.
	longAnswer = "This is a long answer from the mock upstream. It streams one word at a " +
		"time, so that a client can be cut off in the middle of it. Every run sends " +
		"the same words in the same order."
)

// failure says how a streaming scenario ends early.
type failure int

const (
	failNone       failure = iota
	failDrop               // close the connection after K events
	failErrorEvent         // send event: error after K events
)

var failureModes = map[string]failure{
	"mock-drop":        failDrop,
	"mock-error-event": failErrorEvent,
}

// scenarioNames lists the model names the mock serves, for the not-found message.
const scenarioNames = "mock-text, mock-tool, mock-coding, mock-429, mock-500, mock-drop, " +
	"mock-error-event, mock-load, mock-chat"

// reply is the message a scenario answers with.
type reply struct {
	// key names the reply's ids, as <scenario>_<step> with the scenario's dashes as
	// underscores; each protocol puts its own prefixes in front.
	key        string
	model      string
	stopReason string
	blocks     []block
}

// block is a content block: a tool call when toolName is set, else text.
type block struct {
	text     string
	toolName string
	input    []byte
}

// replyFor returns the reply of the scenario that req's model names, or the error
// that the scenario answers with instead. load shapes the reply of mock-load.
func replyFor(req *request, load Load) (reply, *apiError) {
	switch req.model {
	case "mock-text":
		return textReply(req.model, textAnswer), nil
	case loadModel:
		return loadReply(req.model, load), nil
	case chatModel:
		return textReply(req.model, chatAnswer), nil
	case "mock-drop", "mock-error-event":
		return textReply(req.model, longAnswer), nil
	case "mock-tool":
		return toolReply(req)
	case "mock-coding":
		return codingReply(req)
	case "mock-429":
		return reply{}, &apiError{http.StatusTooManyRequests, errorTypeRateLimit,
			"Number of requests has exceeded your rate limit"}
	case "mock-500":
		return reply{}, &apiError{http.StatusInternalServerError, anthropic.ErrorTypeAPI,
			"Internal server error"}
	}
	return reply{}, &apiError{http.StatusNotFound, errorTypeNotFound,
		"model: unknown mock model; use one of " + scenarioNames}
}

// messageID and toolID are the Messages ids of rep: msg_mock_<key> and
// toolu_mock_<key>. A reply holds at most one tool call.
func (rep reply) messageID() string {
	return "msg_mock_" + rep.key
}

func (rep reply) toolID() string {
	return "toolu_mock_" + rep.key
}

func scenarioKey(model string, step int) string {
	name := strings.ReplaceAll(strings.TrimPrefix(model, "mock-"), "-", "_")
	return name + "_" + strconv.Itoa(step)
}

func textReply(model, text string) reply {
	return reply{key: scenarioKey(model, 0), model: model, stopReason: stopEndTurn,
		blocks: []block{{text: text}}}
}

// toolReply calls the first tool of the request.
func toolReply(req *request) (reply, *apiError) {
	if len(req.tools) == 0 {
		return reply{}, invalidRequest("tools: mock-tool needs at least one tool")
	}
	first := req.tools[0]
	return reply{key: scenarioKey(req.model, 0), model: req.model, stopReason: stopToolUse,
		blocks: []block{{toolName: first.Name, input: toolInput(first, "")}}}, nil
}

// codingStep is one tool call of the mock-coding script.
type codingStep struct {
	kind string // the tool to look for: read, edit or bash
	text string // what the step says before the call
}

// codingSteps fixes a bug in calc.py: read the file, edit it, run the tests. The
// step to play is the number of tool results in the request: tool_result blocks in a
// Messages request, function_call_output items in a Responses request. Once every
// step has a result, the script ends with codingAnswer.
var codingSteps = []codingStep{
	{"read", "I will read calc.py first."},
	{"edit", "add subtracts instead of adding, so I will fix it."},
	{"bash", "Now I will run the tests."},
}

func codingReply(req *request) (reply, *apiError) {
	step := min(req.toolResults, len(codingSteps))
	if step == len(codingSteps) {
		return reply{key: scenarioKey(req.model, step), model: req.model, stopReason: stopEndTurn,
			blocks: []block{{text: codingAnswer}}}, nil
	}
	if len(req.tools) == 0 {
		// A client sends side requests without tools, such as one for a session title.
		return reply{key: scenarioKey(req.model, step), model: req.model, stopReason: stopEndTurn,
			blocks: []block{{text: codingNoToolsAnswer}}}, nil
	}
	current := codingSteps[step]
	chosen := pickTool(req.tools, current.kind)
	return reply{key: scenarioKey(req.model, step), model: req.model, stopReason: stopToolUse,
		blocks: []block{
			{text: current.text},
			{toolName: chosen.Name, input: toolInput(chosen, current.kind)},
		}}, nil
}
