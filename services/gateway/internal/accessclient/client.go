// Package accessclient calls the access service's internal API, as api/access.yaml
// gives it: the gateway verifies a gateway key for a model over a protocol.
package accessclient

import (
	"context"
	"net/http"

	"github.com/bibi45c/gateflow/services/gateway/internal/internalapi"
)

const verifyPath = "/internal/v1/keys/verify"

// defaultProtocol is the protocol that access assumes when a verify names none.
const defaultProtocol = "anthropic-messages"

// Error codes of access, from api/errors.md, that the gateway answers with their
// own public error. Every other code gets 500.
const (
	CodeInvalidKey      = "invalid_key"
	CodeModelNotAllowed = "model_not_allowed"
)

// Verified is the answer to a verify call.
type Verified struct {
	TenantID       string  `json:"tenant_id"`
	KeyID          string  `json:"key_id"`
	PriceVersionID string  `json:"price_version_id"`
	Channel        Channel `json:"channel"`
}

// Channel is the upstream access names for the call. The gateway checks it before
// it uses it.
type Channel struct {
	ID            string `json:"id"`
	Protocol      string `json:"protocol"`
	BaseURL       string `json:"base_url"`
	CredentialEnv string `json:"credential_env"`
}

type verifyRequest struct {
	Key      string `json:"key"`
	Model    string `json:"model"`
	Protocol string `json:"protocol,omitempty"`
}

// Client calls access.
type Client struct {
	api *internalapi.Caller
}

// New returns a client that sends its calls through api.
func New(api *internalapi.Caller) *Client {
	return &Client{api: api}
}

// Verify asks access whether key may use model over protocol, as api/access.yaml
// names it. A verify for anthropic-messages leaves protocol out, which access reads
// the same way, so that it is the request the gateway sent before Responses. Its
// errors are those of internalapi.Caller.Post; a reply without a tenant or price
// version is internalapi.ErrMalformed.
func (c *Client) Verify(ctx context.Context, requestID, key, model, protocol string) (Verified, error) {
	req := verifyRequest{Key: key, Model: model}
	if protocol != defaultProtocol {
		req.Protocol = protocol
	}
	var verified Verified
	err := c.api.Post(ctx, verifyPath, requestID, req, &verified, http.StatusOK)
	if err != nil {
		return Verified{}, err
	}
	if verified.TenantID == "" || verified.PriceVersionID == "" {
		return Verified{}, internalapi.ErrMalformed
	}
	return verified, nil
}
