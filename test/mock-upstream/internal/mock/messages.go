package mock

import (
	"encoding/json"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// messages is the Anthropic Messages protocol of POST /v1/messages.
type messages struct{}

// readRequest reads model, stream, tools and the tool_result blocks of a Messages
// request, which must set max_tokens.
func (messages) readRequest(body []byte) (*request, *apiError) {
	parsed, err := anthropic.ParseRequest(body)
	if err != nil {
		return nil, invalidRequest("request body is not valid JSON: " + err.Error())
	}
	if parsed.MaxTokens == nil {
		return nil, invalidRequest("max_tokens: field required")
	}
	var members struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, invalidRequest("messages or tools have the wrong shape")
	}
	req := &request{model: parsed.Model, stream: parsed.Stream, bodyBytes: len(body),
		tools: members.Tools}
	for _, message := range members.Messages {
		var blocks []struct {
			Type string `json:"type"`
		}
		// A content string holds no blocks, so a failed decode counts none.
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "tool_result" {
					req.toolResults++
				}
			}
		}
	}
	return req, nil
}

func (messages) errorBody(failure *apiError) any {
	return anthropic.NewErrorBody(failure.errorType, failure.message)
}

func (messages) responseBody(rep reply, inputTokens int64) any {
	return messageBody(rep, inputTokens)
}

func (messages) streamEvents(rep reply, inputTokens int64) []event {
	return streamEvents(rep, inputTokens)
}

func (messages) errorEvent(int) event {
	return newEvent(anthropic.EventError, anthropic.NewErrorBody(errorTypeOverloaded, overloadedMessage))
}
