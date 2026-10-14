// Package openai holds the OpenAI Responses wire types that FlowGate reads and
// writes: the request members the gateway reads and the one it may set, token usage,
// the events that end a stream with the final response, the error body and error
// event, and a response.completed encoder. Streams use the framing of
// pkg/protocol/sse. It only decodes and encodes. Bounds and their default, the choice
// of which usage counts to charge, outcomes, model checks and relay rules belong to
// the services; see api/metering.yaml and api/errors.md.
package openai
