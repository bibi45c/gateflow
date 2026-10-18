// Package web serves the console's pages: server-rendered html/template pages with a
// little vanilla JavaScript for the playground stream and the dashboard polling.
//
// Every page but sign-in needs a session; admin pages need the admin role, and a
// user's pages only ever ask access and metering about the user's own tenant. Every
// POST must carry the session's CSRF token. Nothing logs a password or gateway key.
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/bibi45c/gateflow/services/console/internal/backend"
	"github.com/bibi45c/gateflow/services/console/internal/session"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Backend is what the pages need from access and metering; *backend.Client
// implements it.
type Backend interface {
	Login(ctx context.Context, username, password string) (backend.User, error)
	Tenants(ctx context.Context) ([]backend.Tenant, error)
	Keys(ctx context.Context, tenantID string) ([]backend.Key, error)
	CreateKey(ctx context.Context, tenantID, name string) (backend.Key, error)
	RevokeKey(ctx context.Context, tenantID, keyID string) error
	Channels(ctx context.Context) ([]backend.Channel, error)
	PriceVersions(ctx context.Context) ([]backend.PriceVersion, error)
	Wallets(ctx context.Context) ([]backend.Wallet, error)
	Usage(ctx context.Context, tenantID string, since time.Time, limit int) (backend.Usage, error)
	Adjust(ctx context.Context, tenantID, id string, amount int64, note string) (backend.Adjustment, error)
}

// Options are the settings of the pages.
type Options struct {
	// GatewayURL is where playground calls go, inside the network.
	GatewayURL string
	// PublicGatewayURL is the base URL that quick start shows.
	PublicGatewayURL string
	PrometheusURL    string
	GrafanaURL       string
	GatewayJob       string
	Location         *time.Location
}

// Server serves the pages.
type Server struct {
	backend  Backend
	sessions *session.Manager
	opts     Options
	pages    map[string]*template.Template
	logger   *slog.Logger
	client   *http.Client
}

// New returns the server. It panics when a template does not parse, which the tests
// catch.
func New(b Backend, sessions *session.Manager, opts Options, logger *slog.Logger) *Server {
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	return &Server{backend: b, sessions: sessions, opts: opts, pages: parsePages(opts.Location), logger: logger,
		client: &http.Client{}}
}

var pageNames = []string{"login", "home", "keys", "usage", "models", "quickstart", "playground",
	"tenants", "catalog", "dashboard", "error"}

func parsePages(loc *time.Location) map[string]*template.Template {
	all := template.FuncMap{"when": timeIn(loc), "whenPtr": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return timeIn(loc)(*t)
	}}
	for name, f := range funcs {
		all[name] = f
	}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		pages[name] = template.Must(template.New("layout.html").Funcs(all).
			ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/"+name+".html"))
	}
	return pages
}

// Register adds the console routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.signedIn(s.logout))

	mux.HandleFunc("GET /{$}", s.signedIn(s.home))
	mux.HandleFunc("GET /keys", s.user(s.keys))
	mux.HandleFunc("POST /keys", s.user(s.createKey))
	mux.HandleFunc("POST /keys/{key_id}/revoke", s.user(s.revokeKey))
	mux.HandleFunc("GET /usage", s.user(s.usage))
	mux.HandleFunc("GET /models", s.signedIn(s.models))
	mux.HandleFunc("GET /quickstart", s.signedIn(s.quickstart))
	mux.HandleFunc("GET /playground", s.signedIn(s.playground))
	mux.HandleFunc("POST /playground/stream", s.signedIn(s.playgroundStream))

	mux.HandleFunc("GET /admin/tenants", s.admin(s.tenants))
	mux.HandleFunc("POST /admin/tenants/{tenant_id}/adjust", s.admin(s.adjust))
	mux.HandleFunc("GET /admin/catalog", s.admin(s.catalog))
	mux.HandleFunc("GET /admin/dashboard", s.admin(s.dashboard))
	mux.HandleFunc("GET /admin/dashboard/data", s.admin(s.dashboardData))
}

type userKey struct{}

func current(r *http.Request) session.User {
	u, _ := r.Context().Value(userKey{}).(session.User)
	return u
}

// signedIn runs next for a signed-in user, and checks the CSRF token of a POST.
// Others are sent to sign in.
func (s *Server) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.sessions.Read(r)
		if err != nil {
			if r.Method == http.MethodGet && r.URL.Path != "/admin/dashboard/data" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf")
			}
			if !s.sessions.CheckCSRF(u, token) {
				http.Error(w, "bad or missing CSRF token; reload the page", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	}
}

// user runs next only for a tenant user; an admin has no tenant of its own.
func (s *Server) user(next http.HandlerFunc) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if current(r).Admin() {
			http.Redirect(w, r, "/admin/tenants", http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

// admin runs next only for an admin.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if !current(r).Admin() {
			s.render(w, r, http.StatusForbidden, "error", "Forbidden", "This page is for admins.")
			return
		}
		next(w, r)
	})
}

// page is what every template gets.
type page struct {
	Title string
	User  session.User
	CSRF  string
	Path  string
	Data  any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	u := current(r)
	p := page{Title: title, User: u, Path: r.URL.Path, Data: data}
	if u.ID != "" {
		p.CSRF = s.sessions.CSRFToken(u)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'")
	w.WriteHeader(status)
	if err := s.pages[name].Execute(w, p); err != nil {
		s.logger.Error("render failed", "page", name, "err", err.Error())
	}
}

// fail renders the error page for a backend failure, without its details.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.logger.Error("backend call failed", "what", what, "path", r.URL.Path, "err", err.Error())
	s.render(w, r, http.StatusBadGateway, "error", "Unavailable",
		"Could not "+what+": a FlowGate service did not answer. Try again in a moment.")
}

// startOfDay is the start of today in the console's time zone.
func (s *Server) startOfDay() time.Time {
	now := time.Now().In(s.opts.Location)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.opts.Location)
}
