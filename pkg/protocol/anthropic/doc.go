// Package anthropic holds the Anthropic Messages wire types that FlowGate reads and
// writes: the request members the gateway reads, token usage, the error body and
// server-sent event framing. It only decodes and encodes. Pricing, bounds, the choice
// of which usage counts to charge, model checks and relay rules belong to the
// services; see api/metering.yaml and api/errors.md.
package anthropic
