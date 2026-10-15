package openai

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CompletedResponse holds what an encoder sets in the response object of a
// response.completed event.
type CompletedResponse struct {
	// ID is the response id, such as "resp_1". Codex rejects the event without it.
	ID string
	// CreatedAt is when the response was created, in Unix seconds.
	CreatedAt int64
	// Model is the model that answered.
	Model string
	// Output holds the output items, each a JSON value; encoding compacts them. Nil
	// writes an empty array.
	Output []json.RawMessage
	// Usage must have every count set. Codex rejects the event without them.
	Usage Usage
}

// completedEvent is the data of a response.completed event.
type completedEvent struct {
	Type           string          `json:"type"`
	SequenceNumber int64           `json:"sequence_number"`
	Response       completedObject `json:"response"`
}

type completedObject struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []json.RawMessage `json:"output"`
	Usage     usageObject       `json:"usage"`
}

// usageObject is a usage object with every count written.
type usageObject struct {
	InputTokens         int64         `json:"input_tokens"`
	InputTokensDetails  inputDetails  `json:"input_tokens_details"`
	OutputTokens        int64         `json:"output_tokens"`
	OutputTokensDetails outputDetails `json:"output_tokens_details"`
	TotalTokens         int64         `json:"total_tokens"`
}

type inputDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type outputDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// EncodeCompletedEvent returns the data of a response.completed event with the given
// sequence number, whose response has object "response" and status "completed". Codex
// rejects the event when its response lacks id or any usage count, so
// EncodeCompletedEvent fails when response.ID is empty or a count of response.Usage is
// nil, and when an output item is not valid JSON. Write the data as an event named
// EventCompleted.
func EncodeCompletedEvent(response CompletedResponse, sequenceNumber int64) ([]byte, error) {
	if response.ID == "" {
		return nil, errors.New("openai: response.completed needs a response id")
	}
	usage, ok := fullUsage(response.Usage)
	if !ok {
		return nil, errors.New("openai: response.completed needs every usage count")
	}
	output := response.Output
	if output == nil {
		output = []json.RawMessage{}
	}
	data, err := json.Marshal(completedEvent{
		Type:           EventCompleted,
		SequenceNumber: sequenceNumber,
		Response: completedObject{
			ID:        response.ID,
			Object:    "response",
			CreatedAt: response.CreatedAt,
			Status:    "completed",
			Model:     response.Model,
			Output:    output,
			Usage:     usage,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("openai: response.completed output: %w", err)
	}
	return data, nil
}

// fullUsage returns u with every count written, or false when one is nil.
func fullUsage(u Usage) (usageObject, bool) {
	counts := []*int64{u.InputTokens, u.CachedTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens}
	for _, count := range counts {
		if count == nil {
			return usageObject{}, false
		}
	}
	return usageObject{
		InputTokens:         *u.InputTokens,
		InputTokensDetails:  inputDetails{CachedTokens: *u.CachedTokens},
		OutputTokens:        *u.OutputTokens,
		OutputTokensDetails: outputDetails{ReasoningTokens: *u.ReasoningTokens},
		TotalTokens:         *u.TotalTokens,
	}, true
}
