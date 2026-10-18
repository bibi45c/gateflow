// Package session keeps a console sign-in in a signed cookie, so the console stores
// no state. The cookie holds the user id, name, role, tenant and expiry, signed with
// HMAC-SHA256 under CONSOLE_SESSION_SECRET; it never holds a password or key.
package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// CookieName is the name of the session cookie.
const CookieName = "flowgate_console"

// Lifetime is how long a sign-in lasts.
const Lifetime = 12 * time.Hour

// Roles a user may have.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User is the signed-in user a cookie names.
type User struct {
	ID       string `json:"id"`
	Username string `json:"name"`
	Role     string `json:"role"`
	TenantID string `json:"tenant,omitempty"`
	Nonce    string `json:"nonce"`
	Expires  int64  `json:"exp"`
}

// Admin reports whether the user is an admin.
func (u User) Admin() bool { return u.Role == RoleAdmin }

// Manager signs and checks session cookies.
type Manager struct {
	secret []byte
	secure bool
	now    func() time.Time
}

// New returns a manager signing with secret. secure marks cookies Secure, for a
// console served over https.
func New(secret []byte, secure bool) *Manager {
	return &Manager{secret: secret, secure: secure, now: time.Now}
}

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Start sets the cookie of a new sign-in of u.
func (m *Manager) Start(w http.ResponseWriter, u User) {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	u.Nonce = base64.RawURLEncoding.EncodeToString(nonce[:])
	u.Expires = m.now().Add(Lifetime).Unix()
	raw, _ := json.Marshal(u)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: payload + "." + m.sign(payload), Path: "/",
		MaxAge: int(Lifetime.Seconds()), HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode,
	})
}

// End clears the cookie.
func (m *Manager) End(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: m.secure, SameSite: http.SameSiteLaxMode})
}

// ErrNoSession means the request carries no valid, unexpired session.
var ErrNoSession = errors.New("session: none")

// Read returns the user of r's cookie, or ErrNoSession.
func (m *Manager) Read(r *http.Request) (User, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return User{}, ErrNoSession
	}
	payload, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(m.sign(payload))) {
		return User{}, ErrNoSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return User{}, ErrNoSession
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil || u.ID == "" || m.now().Unix() >= u.Expires {
		return User{}, ErrNoSession
	}
	if u.Role != RoleAdmin && (u.Role != RoleUser || u.TenantID == "") {
		return User{}, ErrNoSession
	}
	return u, nil
}

// CSRFToken returns the token that forms and fetches of u's session must send. It
// is bound to the session nonce, so it changes with each sign-in.
func (m *Manager) CSRFToken(u User) string {
	return m.sign("csrf:" + u.Nonce)
}

// CheckCSRF reports whether token is u's CSRF token.
func (m *Manager) CheckCSRF(u User, token string) bool {
	return token != "" && hmac.Equal([]byte(token), []byte(m.CSRFToken(u)))
}
