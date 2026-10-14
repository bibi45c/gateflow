package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/channel"
	"github.com/bibi45c/gateflow/services/access/internal/store"
)

type verifyRequest struct {
	Key      *string `json:"key"`
	Model    *string `json:"model"`
	Protocol *string `json:"protocol"`
}

type verifyResponse struct {
	TenantID       string          `json:"tenant_id"`
	KeyID          string          `json:"key_id"`
	PriceVersionID string          `json:"price_version_id"`
	Channel        channelResponse `json:"channel"`
}

type channelResponse struct {
	ID            string `json:"id"`
	Protocol      string `json:"protocol"`
	BaseURL       string `json:"base_url"`
	CredentialEnv string `json:"credential_env"`
}

type priceVersionResponse struct {
	ID                  string `json:"id"`
	InputMicroPerToken  int64  `json:"input_micro_per_token"`
	OutputMicroPerToken int64  `json:"output_micro_per_token"`
}

// verifyKey serves POST /internal/v1/keys/verify.
func (a *API) verifyKey(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeVerifyRequest(w, r)
	if !ok {
		writeError(w, r, CodeInvalidRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	key, err := a.store.VerifyKey(ctx, apikey.HMAC(a.pepper, *request.Key), *request.Model, *request.Protocol)
	s := state(r)
	s.keyID, s.tenantID = key.KeyID, key.TenantID
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	writeJSON(w, http.StatusOK, verifyResponse{
		TenantID:       key.TenantID,
		KeyID:          key.KeyID,
		PriceVersionID: key.PriceVersionID,
		Channel: channelResponse{
			ID:            key.Channel.ID,
			Protocol:      key.Channel.Protocol,
			BaseURL:       key.Channel.BaseURL,
			CredentialEnv: key.Channel.CredentialEnv,
		},
	})
}

// decodeVerifyRequest reads {key, model, protocol} from a body of at most
// MaxBodyBytes. It reports false unless the body is one JSON object whose only
// members are key, model and an optional protocol, each once, by exactly those
// names; key and model must be non-empty strings, and protocol one a channel may
// speak. A missing protocol is anthropic-messages, so a gateway that does not send
// it still gets Messages channels.
func decodeVerifyRequest(w http.ResponseWriter, r *http.Request) (verifyRequest, bool) {
	var request verifyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return request, false
	}
	targets := map[string]**string{"key": &request.Key, "model": &request.Model, "protocol": &request.Protocol}
	seen := make(map[string]bool, len(targets))
	for decoder.More() {
		token, err := decoder.Token()
		name, isName := token.(string)
		// Names match exactly: JSON Schema names are case-sensitive.
		target, known := targets[name]
		if err != nil || !isName || !known || seen[name] {
			return request, false
		}
		seen[name] = true
		if err := decoder.Decode(target); err != nil {
			return request, false
		}
	}
	if _, err := decoder.Token(); err != nil {
		return request, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return request, false
	}
	if request.Key == nil || *request.Key == "" || request.Model == nil || *request.Model == "" {
		return request, false
	}
	if !seen["protocol"] {
		protocol := channel.Protocol
		request.Protocol = &protocol
	}
	// A protocol sent as null is no string, so it is refused like an unknown one.
	if request.Protocol == nil || channel.CheckProtocol(*request.Protocol) != nil {
		return request, false
	}
	return request, true
}

// priceVersion serves GET /internal/v1/price-versions/{id}.
func (a *API) priceVersion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Postgres refuses such text, and no stored id can equal it.
	if !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		writeError(w, r, CodePriceVersionNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	version, err := a.store.PriceVersion(ctx, id)
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	writeJSON(w, http.StatusOK, priceVersionResponse{
		ID:                  version.ID,
		InputMicroPerToken:  version.InputMicroPerToken,
		OutputMicroPerToken: version.OutputMicroPerToken,
	})
}

// storeErrorCode maps a store error to its error code. A database error or timeout
// is dependency_unavailable; anything unexpected is internal_error.
func storeErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrInvalidKey):
		return CodeInvalidKey
	case errors.Is(err, store.ErrModelNotAllowed):
		return CodeModelNotAllowed
	case errors.Is(err, store.ErrPriceVersionNotFound):
		return CodePriceVersionNotFound
	case errors.Is(err, store.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		return CodeDependencyUnavailable
	}
	return CodeInternalError
}
