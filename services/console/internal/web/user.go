package web

import (
	"net/http"
	"sort"
	"strings"

	"github.com/bibi45c/gateflow/services/console/internal/backend"
	"github.com/bibi45c/gateflow/services/console/internal/session"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, err := s.sessions.Read(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	username, password := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("password")
	if username == "" || password == "" {
		s.render(w, r, http.StatusBadRequest, "login", "Sign in", "Enter a username and a password.")
		return
	}
	u, err := s.backend.Login(r.Context(), username, password)
	switch {
	case backend.Code(err) == "invalid_credentials" || backend.Code(err) == "invalid_request":
		s.render(w, r, http.StatusUnauthorized, "login", "Sign in", "The username or password is wrong.")
		return
	case err != nil:
		s.fail(w, r, "sign in", err)
		return
	}
	user := session.User{ID: u.ID, Username: u.Username, Role: u.Role}
	if u.TenantID != nil {
		user.TenantID = *u.TenantID
	}
	s.sessions.Start(w, user)
	s.logger.Info("console sign-in", "user", u.Username, "role", u.Role)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.sessions.End(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	u := current(r)
	if u.Admin() {
		http.Redirect(w, r, "/admin/tenants", http.StatusSeeOther)
		return
	}
	usage, err := s.backend.Usage(r.Context(), u.TenantID, s.startOfDay(), 5)
	if err != nil {
		if backend.Code(err) == "wallet_not_found" {
			s.render(w, r, http.StatusOK, "home", "Home", nil)
			return
		}
		s.fail(w, r, "load your balance", err)
		return
	}
	s.render(w, r, http.StatusOK, "home", "Home", usage)
}

type keysData struct {
	Keys    []backend.Key
	Created *backend.Key
	Error   string
}

func (s *Server) keys(w http.ResponseWriter, r *http.Request) {
	s.renderKeys(w, r, http.StatusOK, keysData{})
}

func (s *Server) renderKeys(w http.ResponseWriter, r *http.Request, status int, data keysData) {
	keys, err := s.backend.Keys(r.Context(), current(r).TenantID)
	if err != nil {
		s.fail(w, r, "list your keys", err)
		return
	}
	data.Keys = keys
	s.render(w, r, status, "keys", "API keys", data)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if len(name) > 64 || strings.ContainsAny(name, "\r\n\t") {
		s.renderKeys(w, r, http.StatusBadRequest, keysData{Error: "A key name has at most 64 characters on one line."})
		return
	}
	key, err := s.backend.CreateKey(r.Context(), current(r).TenantID, name)
	if err != nil {
		s.fail(w, r, "create a key", err)
		return
	}
	s.logger.Info("console key created", "user", current(r).Username, "key_id", key.ID)
	// The raw key is shown in this answer only; it is never stored by the console.
	s.renderKeys(w, r, http.StatusOK, keysData{Created: &key})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	err := s.backend.RevokeKey(r.Context(), current(r).TenantID, r.PathValue("key_id"))
	if err != nil && backend.Code(err) != "key_not_found" {
		s.fail(w, r, "revoke the key", err)
		return
	}
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.backend.Usage(r.Context(), current(r).TenantID, s.startOfDay(), 100)
	if err != nil && backend.Code(err) != "wallet_not_found" {
		s.fail(w, r, "load your usage", err)
		return
	}
	s.render(w, r, http.StatusOK, "usage", "Usage", usage)
}

type modelsData struct {
	Models []modelRow
	Mock   []mockModel
	Prices []backend.PriceVersion
}

// mockModel is a model the mock upstream serves, as a channel pattern such as
// mock-* cannot name it.
type mockModel struct {
	Name string
	What string
}

// mockModels are the mock scenarios worth trying by hand; the first is the
// playground's default.
var mockModels = []mockModel{
	{"mock-chat", "A readable answer about FlowGate, streamed word by word at about 28 tokens a second"},
	{"mock-text", "One short sentence, at once"},
	{"mock-coding", "A scripted coding session with tool calls, for Codex CLI and OpenCode"},
	{"mock-load", "The load-test stream: 200 deltas of repeated letters over about 20 seconds"},
}

// servedMocks returns mockModels when a channel serves the mock-* pattern, else nil.
func servedMocks(rows []modelRow) []mockModel {
	for _, row := range rows {
		if row.Pattern == "mock-*" {
			return mockModels
		}
	}
	return nil
}

type modelRow struct {
	Pattern   string
	Protocols []string
}

// catalogModels groups the channels' model patterns with the protocols that serve
// them.
func catalogModels(channels []backend.Channel) []modelRow {
	byPattern := map[string][]string{}
	for _, c := range channels {
		for _, m := range c.Models {
			byPattern[m] = append(byPattern[m], c.Protocol)
		}
	}
	rows := make([]modelRow, 0, len(byPattern))
	for pattern, protocols := range byPattern {
		sort.Strings(protocols)
		rows = append(rows, modelRow{Pattern: pattern, Protocols: protocols})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Pattern < rows[j].Pattern })
	return rows
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	channels, err := s.backend.Channels(r.Context())
	if err != nil {
		s.fail(w, r, "list the models", err)
		return
	}
	prices, err := s.backend.PriceVersions(r.Context())
	if err != nil {
		s.fail(w, r, "list the prices", err)
		return
	}
	rows := catalogModels(channels)
	s.render(w, r, http.StatusOK, "models", "Models and prices", modelsData{Models: rows, Mock: servedMocks(rows), Prices: prices})
}

func (s *Server) quickstart(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "quickstart", "Quick start", map[string]string{"BaseURL": s.opts.PublicGatewayURL})
}
