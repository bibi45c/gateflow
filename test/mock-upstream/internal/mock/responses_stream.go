package mock

import (
	"encoding/json"
	"fmt"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

// createdAt is the created_at of every mock response, 2026-01-01T00:00:00Z, so the
// same request always gets the same bytes.
const createdAt = 1767225600

// Statuses of a response and of its output items.
const (
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
)

// The JSON shapes below follow the OpenAI Responses API; struct field order fixes
// the order of members in the output.

// responseJSON is the response of response.created and response.in_progress, which
// has no output and no usage yet.
type responseJSON struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	Model     string `json:"model"`
	Output    []any  `json:"output"`
	Usage     any    `json:"usage"`
}

type messageItemJSON struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Status  string           `json:"status"`
	Role    string           `json:"role"`
	Content []outputTextJSON `json:"content"`
}

type outputTextJSON struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type functionCallJSON struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
}

type responseEventJSON struct {
	Type           string       `json:"type"`
	SequenceNumber int64        `json:"sequence_number"`
	Response       responseJSON `json:"response"`
}

type itemEventJSON struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
	OutputIndex    int    `json:"output_index"`
	Item           any    `json:"item"`
}

type partEventJSON struct {
	Type           string         `json:"type"`
	SequenceNumber int64          `json:"sequence_number"`
	ItemID         string         `json:"item_id"`
	OutputIndex    int            `json:"output_index"`
	ContentIndex   int            `json:"content_index"`
	Part           outputTextJSON `json:"part"`
}

type textDeltaEventJSON struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
}

type textDoneEventJSON struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Text           string `json:"text"`
}

type argumentsDeltaEventJSON struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Delta          string `json:"delta"`
}

type argumentsDoneEventJSON struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Arguments      string `json:"arguments"`
}

// The Responses ids of a reply: resp_mock_<key> for the response, msg_mock_<key>
// for its text item, and fc_mock_<key> and call_mock_<key> for its function call. A
// reply holds at most one text block and one tool call.
func (rep reply) responseID() string { return "resp_mock_" + rep.key }
func (rep reply) textItemID() string { return "msg_mock_" + rep.key }
func (rep reply) callItemID() string { return "fc_mock_" + rep.key }
func (rep reply) callID() string     { return "call_mock_" + rep.key }

// outputItem returns the output item of b: a message with one output_text, or a
// function_call. An item in progress has no text and no arguments yet.
func (rep reply) outputItem(b block, done bool) any {
	status, text := statusInProgress, ""
	if done {
		status, text = statusCompleted, b.text
	}
	if b.toolName != "" {
		if done {
			text = string(b.input)
		}
		return functionCallJSON{ID: rep.callItemID(), Type: "function_call", Status: status,
			Arguments: text, CallID: rep.callID(), Name: b.toolName}
	}
	content := []outputTextJSON{}
	if done {
		content = append(content, outputText(text))
	}
	return messageItemJSON{ID: rep.textItemID(), Type: "message", Status: status,
		Role: "assistant", Content: content}
}

func outputText(text string) outputTextJSON {
	return outputTextJSON{Type: "output_text", Text: text, Annotations: []any{}}
}

// completedEvent returns the data of the response.completed event of rep. Its usage
// follows the package formulas, and total_tokens is their sum.
func (rep reply) completedEvent(inputTokens, sequenceNumber int64) []byte {
	output := make([]json.RawMessage, len(rep.blocks))
	for i, b := range rep.blocks {
		output[i] = mustMarshal(rep.outputItem(b, true))
	}
	outputTokens, total, zero := rep.outputTokens(), inputTokens+rep.outputTokens(), int64(0)
	data, err := openai.EncodeCompletedEvent(openai.CompletedResponse{
		ID: rep.responseID(), CreatedAt: createdAt, Model: rep.model, Output: output,
		Usage: openai.Usage{InputTokens: &inputTokens, OutputTokens: &outputTokens,
			TotalTokens: &total, CachedTokens: &zero, ReasoningTokens: &zero},
	}, sequenceNumber)
	if err != nil {
		panic(fmt.Sprintf("mock: encode %s: %v", openai.EventCompleted, err))
	}
	return data
}

// responseBody returns the non-streaming body of rep: the response of its
// response.completed event, so both forms carry the same object.
func (responses) responseBody(rep reply, inputTokens int64) any {
	var completed struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(rep.completedEvent(inputTokens, 0), &completed); err != nil {
		panic(fmt.Sprintf("mock: decode %s: %v", openai.EventCompleted, err))
	}
	return completed.Response
}

// responseStream collects the events of a stream, numbering them from 0.
type responseStream struct {
	events []event
}

func (s *responseStream) next() int64 {
	return int64(len(s.events))
}

func (s *responseStream) add(name string, data any) {
	s.events = append(s.events, newEvent(name, data))
}

// streamEvents returns every event of the stream of rep, from response.created to
// response.completed.
func (responses) streamEvents(rep reply, inputTokens int64) []event {
	var s responseStream
	start := responseJSON{ID: rep.responseID(), Object: "response", CreatedAt: createdAt,
		Status: statusInProgress, Model: rep.model, Output: []any{}}
	s.add(openai.EventCreated, responseEventJSON{openai.EventCreated, s.next(), start})
	s.add(openai.EventInProgress, responseEventJSON{openai.EventInProgress, s.next(), start})
	for i, b := range rep.blocks {
		s.add(openai.EventOutputItemAdded, itemEventJSON{openai.EventOutputItemAdded, s.next(), i,
			rep.outputItem(b, false)})
		if b.toolName == "" {
			s.addText(rep.textItemID(), i, b)
		} else {
			s.addArguments(rep.callItemID(), i, b)
		}
		s.add(openai.EventOutputItemDone, itemEventJSON{openai.EventOutputItemDone, s.next(), i,
			rep.outputItem(b, true)})
	}
	s.events = append(s.events, event{name: openai.EventCompleted,
		data: rep.completedEvent(inputTokens, s.next())})
	return s.events
}

// addText adds the events of the one output_text part of a message item.
func (s *responseStream) addText(itemID string, index int, b block) {
	s.add(openai.EventContentPartAdded, partEventJSON{openai.EventContentPartAdded, s.next(),
		itemID, index, 0, outputText("")})
	for _, part := range b.parts() {
		s.add(openai.EventOutputTextDelta, textDeltaEventJSON{openai.EventOutputTextDelta,
			s.next(), itemID, index, 0, part})
	}
	s.add(openai.EventOutputTextDone, textDoneEventJSON{openai.EventOutputTextDone, s.next(),
		itemID, index, 0, b.text})
	s.add(openai.EventContentPartDone, partEventJSON{openai.EventContentPartDone, s.next(),
		itemID, index, 0, outputText(b.text)})
}

// addArguments adds the events of the arguments of a function_call item.
func (s *responseStream) addArguments(itemID string, index int, b block) {
	for _, part := range b.parts() {
		s.add(openai.EventFunctionCallArgumentsDelta, argumentsDeltaEventJSON{
			openai.EventFunctionCallArgumentsDelta, s.next(), itemID, index, part})
	}
	s.add(openai.EventFunctionCallArgumentsDone, argumentsDoneEventJSON{
		openai.EventFunctionCallArgumentsDone, s.next(), itemID, index, string(b.input)})
}
