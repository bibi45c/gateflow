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
	"mock-error-event"

// reply is the message a scenario answers with.
type reply struct {
	id         string
	model      string
	stopReason string
	blocks     []block
}

// block is a content block: a tool_use when toolName is set, else text.
type block struct {
	text     string
	toolID   string
	toolName string
	input    []byte
}

// replyFor returns the reply of the scenario that req's model names, or the error
// that the scenario answers with instead.
func replyFor(req *request) (reply, *apiError) {
	switch req.model {
	case "mock-text":
		return textReply(req.model, textAnswer), nil
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

// messageID and toolID name step step of a scenario: msg_mock_<scenario>_<step> and
// toolu_mock_<scenario>_<step>, with the scenario's dashes as underscores.
func messageID(model string, step int) string {
	return "msg_mock_" + scenarioKey(model, step)
}

func toolID(model string, step int) string {
	return "toolu_mock_" + scenarioKey(model, step)
}

func scenarioKey(model string, step int) string {
	name := strings.ReplaceAll(strings.TrimPrefix(model, "mock-"), "-", "_")
	return name + "_" + strconv.Itoa(step)
}

func textReply(model, text string) reply {
	return reply{id: messageID(model, 0), model: model, stopReason: stopEndTurn,
		blocks: []block{{text: text}}}
}

// toolReply calls the first tool of the request.
func toolReply(req *request) (reply, *apiError) {
	if len(req.tools) == 0 {
		return reply{}, invalidRequest("tools: mock-tool needs at least one tool")
	}
	first := req.tools[0]
	return reply{id: messageID(req.model, 0), model: req.model, stopReason: stopToolUse,
		blocks: []block{{toolID: toolID(req.model, 0), toolName: first.Name,
			input: toolInput(first)}}}, nil
}

// codingStep is one tool call of the mock-coding script.
type codingStep struct {
	kind string // the tool to look for: read, edit or bash
	text string // what the step says before the call
}

// codingSteps fixes a bug in calc.py: read the file, edit it, run the tests. The
// step to play is the number of tool_result blocks in the request; once every step
// has a result, the script ends with codingAnswer.
var codingSteps = []codingStep{
	{"read", "I will read calc.py first."},
	{"edit", "add subtracts instead of adding, so I will fix it."},
	{"bash", "Now I will run the tests."},
}

func codingReply(req *request) (reply, *apiError) {
	step := min(req.toolResults, len(codingSteps))
	if step == len(codingSteps) {
		return reply{id: messageID(req.model, step), model: req.model, stopReason: stopEndTurn,
			blocks: []block{{text: codingAnswer}}}, nil
	}
	if len(req.tools) == 0 {
		// A client sends side requests without tools, such as one for a session title.
		return reply{id: messageID(req.model, step), model: req.model, stopReason: stopEndTurn,
			blocks: []block{{text: codingNoToolsAnswer}}}, nil
	}
	current := codingSteps[step]
	chosen := pickTool(req.tools, current.kind)
	return reply{id: messageID(req.model, step), model: req.model, stopReason: stopToolUse,
		blocks: []block{
			{text: current.text},
			{toolID: toolID(req.model, step), toolName: chosen.Name, input: toolInput(chosen)},
		}}, nil
}
