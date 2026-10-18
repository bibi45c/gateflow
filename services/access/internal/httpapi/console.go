package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bibi45c/gateflow/services/access/internal/apikey"
	"github.com/bibi45c/gateflow/services/access/internal/password"
	"github.com/bibi45c/gateflow/services/access/internal/store"
)

// ConsoleStore is what the console routes need from the database; *store.Store
// implements it.
type ConsoleStore interface {
	UserByName(ctx context.Context, username string) (store.ConsoleUser, error)
	Tenants(ctx context.Context) ([]store.Tenant, error)
	Keys(ctx context.Context, tenantID string) ([]store.Key, error)
	CreateKey(ctx context.Context, tenantID, name, prefix string, keyHMAC []byte) (store.Key, error)
	RevokeKey(ctx context.Context, tenantID, keyID string) (store.Key, error)
	Channels(ctx context.Context) ([]store.ChannelInfo, error)
	PriceVersions(ctx context.Context) ([]store.PriceVersionInfo, error)
}

// Limits of the console inputs.
const (
	maxUsernameBytes = 64
	maxPasswordBytes = 256
	maxKeyNameBytes  = 64
	keyPrefixLen     = 10
)

// loginTimeout covers one PBKDF2 check, which takes longer than a query.
const loginTimeout = 3 * time.Second

// absentUserHash is checked when no user has the name, so an unknown name takes as
// long as a wrong password.
var absentUserHash, _ = password.Hash("absent-user")

// WithConsole adds the console routes, served from cs, and returns a.
func (a *API) WithConsole(cs ConsoleStore) *API {
	a.console = cs
	return a
}

func (a *API) registerConsole(mux *http.ServeMux) {
	if a.console == nil {
		return
	}
	mux.Handle("POST "+loginRoute, a.route(loginRoute, a.login))
	mux.Handle("GET "+tenantsRoute, a.route(tenantsRoute, a.tenants))
	mux.Handle("GET "+keysRoute, a.route(keysRoute, a.keys))
	mux.Handle("POST "+keysRoute, a.route(keysRoute, a.createKey))
	mux.Handle("POST "+revokeRoute, a.route(revokeRoute, a.revokeKey))
	mux.Handle("GET "+channelsRoute, a.route(channelsRoute, a.channels))
	mux.Handle("GET "+priceVersionsRoute, a.route(priceVersionsRoute, a.priceVersions))
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	UserID   string  `json:"user_id"`
	Username string  `json:"username"`
	Role     string  `json:"role"`
	TenantID *string `json:"tenant_id"`
}

type tenantResponse struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	ActiveKeys int       `json:"active_keys"`
	CreatedAt  time.Time `json:"created_at"`
}

type keyResponse struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	Name           string     `json:"name"`
	Prefix         string     `json:"prefix"`
	PriceVersionID string     `json:"price_version_id"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	Key            string     `json:"key,omitempty"`
}

type createKeyRequest struct {
	Name string `json:"name"`
}

type channelInfoResponse struct {
	ID       string   `json:"id"`
	Protocol string   `json:"protocol"`
	BaseURL  string   `json:"base_url"`
	Models   []string `json:"models"`
}

type priceVersionInfoResponse struct {
	priceVersionResponse
	CreatedAt time.Time `json:"created_at"`
}

// login serves POST /internal/v1/console/login.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var request loginRequest
	if !decodeStrict(w, r, &request) || request.Username == "" || request.Password == "" ||
		len(request.Username) > maxUsernameBytes || len(request.Password) > maxPasswordBytes {
		writeError(w, r, CodeInvalidRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	user, err := a.console.UserByName(ctx, request.Username)
	stored := user.PasswordHash
	if errors.Is(err, store.ErrUserNotFound) {
		stored = absentUserHash
	} else if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	ok, checkErr := password.Check(stored, request.Password)
	switch {
	case checkErr != nil:
		writeError(w, r, CodeInternalError)
	case err != nil || !ok:
		writeError(w, r, CodeInvalidCredentials)
	default:
		writeJSON(w, http.StatusOK, newUserResponse(user))
	}
}

func newUserResponse(u store.ConsoleUser) userResponse {
	response := userResponse{UserID: u.ID, Username: u.Username, Role: u.Role}
	if u.TenantID != "" {
		response.TenantID = &u.TenantID
	}
	return response
}

// tenants serves GET /internal/v1/tenants.
func (a *API) tenants(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	tenants, err := a.console.Tenants(ctx)
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	items := make([]tenantResponse, len(tenants))
	for i, t := range tenants {
		items[i] = tenantResponse(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// keys serves GET /internal/v1/tenants/{tenant_id}/keys.
func (a *API) keys(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := pathID(w, r, "tenant_id", CodeTenantNotFound)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	keys, err := a.console.Keys(ctx, tenantID)
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	items := make([]keyResponse, len(keys))
	for i, k := range keys {
		items[i] = newKeyResponse(k, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func newKeyResponse(k store.Key, raw string) keyResponse {
	return keyResponse{ID: k.ID, TenantID: k.TenantID, Name: k.Name, Prefix: k.Prefix,
		PriceVersionID: k.PriceVersionID, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt, Key: raw}
}

// createKey serves POST /internal/v1/tenants/{tenant_id}/keys. The raw key is in
// this answer only; the database keeps its HMAC.
func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := pathID(w, r, "tenant_id", CodeTenantNotFound)
	if !ok {
		return
	}
	var request createKeyRequest
	if !decodeStrict(w, r, &request) || len(request.Name) > maxKeyNameBytes || !printable(request.Name) {
		writeError(w, r, CodeInvalidRequest)
		return
	}
	raw := newRawKey()
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	key, err := a.console.CreateKey(ctx, tenantID, request.Name, raw[:keyPrefixLen], apikey.HMAC(a.pepper, raw))
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	s := state(r)
	s.keyID, s.tenantID = key.ID, key.TenantID
	writeJSON(w, http.StatusCreated, newKeyResponse(key, raw))
}

// revokeKey serves POST /internal/v1/tenants/{tenant_id}/keys/{key_id}/revoke.
func (a *API) revokeKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := pathID(w, r, "tenant_id", CodeKeyNotFound)
	if !ok {
		return
	}
	keyID, ok := pathID(w, r, "key_id", CodeKeyNotFound)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	key, err := a.console.RevokeKey(ctx, tenantID, keyID)
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	s := state(r)
	s.keyID, s.tenantID = key.ID, key.TenantID
	writeJSON(w, http.StatusOK, newKeyResponse(key, ""))
}

// channels serves GET /internal/v1/channels.
func (a *API) channels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	channels, err := a.console.Channels(ctx)
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	items := make([]channelInfoResponse, len(channels))
	for i, c := range channels {
		items[i] = channelInfoResponse(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// priceVersions serves GET /internal/v1/price-versions.
func (a *API) priceVersions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeout)
	defer cancel()
	versions, err := a.console.PriceVersions(ctx)
	if err != nil {
		writeError(w, r, storeErrorCode(err))
		return
	}
	items := make([]priceVersionInfoResponse, len(versions))
	for i, v := range versions {
		items[i] = priceVersionInfoResponse{
			priceVersionResponse: priceVersionResponse{ID: v.ID, InputMicroPerToken: v.InputMicroPerToken,
				OutputMicroPerToken: v.OutputMicroPerToken},
			CreatedAt: v.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// pathID reads a path value that Postgres can hold; otherwise it answers notFound,
// since no stored id can equal it.
func pathID(w http.ResponseWriter, r *http.Request, name, notFound string) (string, bool) {
	id := r.PathValue(name)
	if id == "" || len(id) > 200 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		writeError(w, r, notFound)
		return "", false
	}
	return id, true
}

// decodeStrict reads one JSON object of at most MaxBodyBytes into v, refusing
// unknown members and anything after the object.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return false
	}
	return !decoder.More()
}

// printable reports whether s is valid UTF-8 without control characters.
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// newRawKey returns gf_ and 43 base64url characters of 32 random bytes, the same
// shape as the seeded keys.
func newRawKey() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "gf_" + base64.RawURLEncoding.EncodeToString(b[:])
}
