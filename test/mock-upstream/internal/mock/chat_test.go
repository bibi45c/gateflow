package mock

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bibi45c/gateflow/pkg/protocol/openai"
)

// chatWords is the number of words, and so deltas and output tokens, of mock-chat.
var chatWords = len(strings.SplitAfter(chatAnswer, " "))

func TestChatStreamsItsAnswerWordByWordOverMessages(t *testing.T) {
	srv := newServer(t, DefaultTiming)
	start := time.Now()
	events, err := readEvents(t, post(t, srv, requestBody(t, chatModel, true, nil, userMessages), nil))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for _, event := range events {
		var data eventData
		if json.Unmarshal(event.Data, &data) == nil && data.Delta.Type == "text_delta" {
			deltas = append(deltas, data.Delta.Text)
		}
	}
	if strings.Join(deltas, "") != chatAnswer || len(deltas) != chatWords {
		t.Fatalf("%d deltas do not spell the answer", len(deltas))
	}
	got := assemble(t, events)
	if got.messageID != "msg_mock_chat_0" || got.stopReason != stopEndTurn ||
		count(got.usageDelta.OutputTokens) != int64(chatWords) {
		t.Errorf("id %q stop %q output tokens %d, want %d", got.messageID, got.stopReason,
			count(got.usageDelta.OutputTokens), chatWords)
	}
	// Paced like a real model: 20 to 40 tokens a second.
	if min, max := time.Duration(chatWords-1)*chatInterval, time.Duration(chatWords)*time.Second/20; elapsed < min || elapsed > max {
		t.Errorf("%d words took %v, want between %v and %v", chatWords, elapsed, min, max)
	}
}

func TestChatStreamsOverResponses(t *testing.T) {
	srv := newResponsesServer(t, DefaultTiming)
	body := responsesBody(t, chatModel, true, nil, userInput)
	events, err := readEvents(t, postResponses(t, srv, body, nil))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, event := range events {
		var data responseEventData
		if json.Unmarshal(event.Data, &data) == nil && data.Type == openai.EventOutputTextDelta {
			text.WriteString(data.Delta)
		}
	}
	if text.String() != chatAnswer {
		t.Fatalf("Responses text differs from the answer")
	}
	got := assembleResponse(t, events)
	if got.completed.ID != "resp_mock_chat_0" {
		t.Errorf("response id %q", got.completed.ID)
	}
	checkUsage(t, "stream", got.usage, body, int64(chatWords))
}

func TestChatAnswerIsReadable(t *testing.T) {
	if !strings.Contains(chatAnswer, "FlowGate") || !strings.Contains(chatAnswer, "```python\n") ||
		strings.Count(chatAnswer, "```") != 2 {
		t.Error("the answer lost its subject or its code block")
	}
	if chatWords < 60 || chatWords > 300 {
		t.Errorf("%d words; keep the demo answer between 60 and 300 words", chatWords)
	}
	if rate := float64(time.Second) / float64(chatInterval); rate < 20 || rate > 40 {
		t.Errorf("%.1f tokens a second, want 20 to 40", rate)
	}
	// mock-chat keeps its pace whatever EventInterval says; mock-load keeps its own.
	slow := Timing{EventInterval: 10 * time.Second, Load: DefaultLoad}
	if got := slow.forModel(chatModel); got.EventInterval != chatInterval || !got.fixedRate {
		t.Errorf("mock-chat timing %+v", got)
	}
	if got := slow.forModel(loadModel); got.EventInterval != DefaultLoad.Interval {
		t.Errorf("mock-load timing changed: %+v", got)
	}
}
