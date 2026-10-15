package relay

import (
	"math"

	"github.com/bibi45c/gateflow/pkg/protocol/anthropic"
)

// result returns the outcome and usage the exchange saw, by the rules of #18.
func (x *exchange) result() Result {
	outcome := x.outcome()
	if outcome == OutcomeNotDispatched || outcome == OutcomeUpstreamError {
		// Metering charges 0 for these and ignores the counts; #18 has the gateway
		// send both as 0.
		return Result{Outcome: outcome, InputTokens: new(int64), OutputTokens: new(int64)}
	}
	input, output := x.observer.counts()
	return Result{Outcome: outcome, InputTokens: input, OutputTokens: output}
}

// outcome applies the first-match rule of #18.
func (x *exchange) outcome() string {
	switch {
	case !x.dispatched.Load():
		return OutcomeNotDispatched
	case x.completed:
		return OutcomeCompleted
	case x.clientGone:
		return OutcomeClientCancelled
	case x.upstreamError:
		return OutcomeUpstreamError
	default:
		return OutcomeInterrupted
	}
}

// usage collects the token counts of a Messages reply by the usage source of #18. The
// input total is input_tokens plus the two cache counts, from message_start, replaced
// by a later message_delta whose input total is above 0. The output count is the
// output_tokens of the last message_delta. A non-streaming body's usage counts the
// same way. A negative count is never reported: a total holding one is unknown.
type usage struct {
	input  *int64
	output *int64
}

// observe reads the usage of one stream event.
func (u *usage) observe(eventType string, data []byte) {
	switch eventType {
	case anthropic.EventMessageStart:
		if reported, err := anthropic.MessageStartUsage(data); err == nil {
			u.input = inputTotal(reported)
		}
	case anthropic.EventMessageDelta:
		reported, err := anthropic.MessageDeltaUsage(data)
		if err != nil {
			u.output = nil // the last message_delta has no readable output count
			return
		}
		if total := inputTotal(reported); total != nil && *total > 0 {
			u.input = total
		}
		u.output = knownCount(reported.OutputTokens)
	}
}

// observeBody reads the usage of a non-streaming reply.
func (u *usage) observeBody(body []byte) {
	reported, err := anthropic.ResponseUsage(body)
	if err != nil {
		return
	}
	u.input = inputTotal(reported)
	u.output = knownCount(reported.OutputTokens)
}

// counts returns the counts to report, by reportedCounts.
func (u *usage) counts() (input, output *int64) {
	return reportedCounts(u.input, u.output)
}

// reportedCounts returns the counts a reply reported as the relay reports them, on
// either protocol. An input total of 0 is unknown, because a non-empty request cannot
// use 0 input tokens and some upstreams report 0 before the real count. An output
// count of 0 that the upstream reported is known.
func reportedCounts(input, output *int64) (*int64, *int64) {
	if input != nil && *input == 0 {
		return nil, output
	}
	return input, output
}

// inputTotal adds the input counts that are present, or returns nil when none is, or
// one is negative. A sum past the int64 range stays at its maximum.
func inputTotal(reported anthropic.Usage) *int64 {
	var total int64
	seen := false
	for _, count := range []*int64{
		reported.InputTokens,
		reported.CacheReadInputTokens,
		reported.CacheCreationInputTokens,
	} {
		switch {
		case count == nil:
			continue
		case *count < 0:
			return nil
		case total > math.MaxInt64-*count:
			total = math.MaxInt64
		default:
			total += *count
		}
		seen = true
	}
	if !seen {
		return nil
	}
	return &total
}

// knownCount returns a copy of count, or nil when it is absent or negative.
func knownCount(count *int64) *int64 {
	if count == nil || *count < 0 {
		return nil
	}
	known := *count
	return &known
}
