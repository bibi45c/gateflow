# Error codes

This file is the single list of FlowGate error codes for S1, as agreed in issue #18
(revision 3.2): every internal code with the service and status that return it, the
public errors the gateway raises, and how the gateway maps internal errors to public
ones. The `ErrorCode` enum in [access.yaml](access.yaml) and in
[metering.yaml](metering.yaml) lists exactly the codes this file gives for that
service, and no file uses other codes; `tools/repo/tests/test_api_contracts.py` checks
this. Issue #61 adds the public errors of `POST /v1/responses` for S2; the internal
codes do not change.

## Internal errors

Access and metering answer every error with this body:

```json
{"error": {"code": "invalid_key", "message": "The gateway key is unknown or revoked.", "request_id": "req_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e34"}}
```

- `code` is one of the codes below. Callers act on `code`, never on the status alone.
- `message` is a fixed string per code and never echoes request input.
- `request_id` is the `X-Request-Id` of the request. Every internal caller, including
  metering when it calls access, sends `X-Request-Id` with the gateway request id.

| Service | Status | Code | Returned when |
|---|---|---|---|
| access | 400 | `invalid_request` | The verify body is not JSON or breaks the request schema. |
| access | 401 | `unauthorized` | The internal token is missing, empty or wrong. |
| access | 401 | `invalid_key` | The gateway key is unknown or revoked. |
| access | 403 | `model_not_allowed` | The gateway key is valid but none of its channels serves the requested model over the requested protocol. |
| access | 404 | `price_version_not_found` | No price version has the requested id. |
| access | 500 | `internal_error` | An unexpected failure. |
| access | 503 | `dependency_unavailable` | The access database is unavailable. |
| metering | 400 | `invalid_request` | The body is not JSON or breaks the request schema, a token count is negative, or at reserve `max_cost` overflows int64. |
| metering | 401 | `unauthorized` | The internal token is missing, empty or wrong. |
| metering | 404 | `wallet_not_found` | At reserve, the tenant has no wallet. |
| metering | 404 | `reservation_not_found` | At settle, no reservation has the id in the path. |
| metering | 409 | `insufficient_balance` | At reserve, the wallet's available balance is below `max_cost`. |
| metering | 422 | `idempotency_conflict` | An earlier reserve with the same (`call_id`, `attempt_id`), or an earlier settle of the same reservation, had different content. |
| metering | 500 | `internal_error` | An unexpected failure; or, at reserve, access answered the price lookup with 404 or 401, or the price version is outside 0 to 1,000,000 micro-units per token. |
| metering | 503 | `dependency_unavailable` | The metering database is unavailable; or, at reserve, access did not answer the price lookup within 1 s or answered it with a 5xx. |

## Public errors raised by the gateway

Public errors use the error body of the endpoint the client called. The status and
type that the tables below give are the same on both endpoints.

On `/v1/messages`, public errors use the Anthropic error body,
`pkg/protocol/anthropic.ErrorBody`:

```json
{"type": "error", "error": {"type": "api_error", "message": "Internal server error."}}
```

On `/v1/responses`, public errors use the Responses error body. `error.type` is the
type from the tables below; `param` and `code` are null unless a row names them:

```json
{"error": {"message": "Internal server error.", "type": "api_error", "param": null, "code": null}}
```

The gateway checks a request in this order: body size (413), gateway key present
(401), JSON and the body fields of the endpoint (400), verify with access, channel
check, reserve, dispatch.

| Situation | Status | Type |
|---|---|---|
| Body larger than 2 MiB | 413 | `request_too_large` |
| Missing gateway key | 401 | `authentication_error` |
| On `/v1/messages`: malformed JSON, missing or non-string `model`, missing or out-of-range `max_tokens` (an integer from 1 to 32,768) | 400 | `invalid_request_error` |
| On `/v1/responses`: malformed JSON, missing or non-string `model`, or a `max_output_tokens` that breaks the bounds of reserve in [metering.yaml](metering.yaml) | 400 | `invalid_request_error` |
| On `/v1/responses`: `previous_response_id` is present and not null | 400 | `invalid_request_error`, with `param` `"previous_response_id"` |
| Channel misconfigured (a `protocol` other than the endpoint's, bad `base_url` or `credential_env`, or its variable unset); logged, and no reservation is made | 500 | `api_error` |
| Upstream 400 or 429 | 400 or 429 | Passed through with the upstream's status and body |
| Any other upstream status, or a failure before any output | 502 | `api_error` |
| Failure after the stream has started | Already sent | An upstream error event is relayed unchanged: `event: error` on `/v1/messages`, and `error` or `response.failed` on `/v1/responses`. Otherwise the gateway sends its own error event with type `api_error` and a fixed message: `event: error` on `/v1/messages`, and the Responses `error` event below on `/v1/responses`. Then the gateway closes the stream. |

The gateway's own Responses `error` event carries the type in `code`.
`sequence_number` is one more than that of the last event the gateway relayed:

```text
event: error
data: {"type": "error", "code": "api_error", "message": "Internal server error.", "param": null, "sequence_number": 12}
```

- `/v1/responses` is stateless. The gateway keeps no response state, so it rejects
  `previous_response_id` as above, and it passes `store` upstream unchanged.
- Public 5xx messages carry no internal code or detail.
- The response header `request-id` is `req_<uuid>`, set before the first write. The
  gateway also passes back the upstream's `content-type` and `retry-after`.
- 409 and 429 are not used for balance, because clients retry them and each retry
  reserves again.

## Mapping internal errors to public errors

The gateway maps an internal error by its `error.code`, never by its status alone:

| Internal | Public status | Public type |
|---|---|---|
| access `invalid_key` | 401 | `authentication_error` |
| access `model_not_allowed` | 403 | `permission_error` |
| metering `insufficient_balance`, `wallet_not_found` | 402 | `billing_error` |
| access or metering unreachable, timed out, or `dependency_unavailable` | 503 | `api_error` |
| Any other code (`invalid_request`, `unauthorized`, `reservation_not_found`, `idempotency_conflict`, `internal_error`), or a body without a known code | 500 | `api_error`; the gateway logs it as an error |

Access returns `price_version_not_found` only to metering, which answers its own
caller 500 `internal_error` instead; the gateway never receives it.

Verify and reserve time out after 2 s each. If reserve times out, returns 5xx or fails
to connect, the gateway retries it once with the same ids; if it still has no
reservation, it answers 503 `api_error` and never calls the upstream.
