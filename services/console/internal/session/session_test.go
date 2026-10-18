package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func roundTrip(m *Manager, u User) (*http.Request, *http.Cookie) {
	rec := httptest.NewRecorder()
	m.Start(rec, u)
	cookie := rec.Result().Cookies()[0]
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	return r, cookie
}

func TestSessionRoundTripAndCSRF(t *testing.T) {
	m := New([]byte("secret"), false)
	r, cookie := roundTrip(m, User{ID: "user-demo", Username: "demo", Role: RoleUser, TenantID: "tenant-demo"})
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: %+v", cookie)
	}
	u, err := m.Read(r)
	if err != nil || u.TenantID != "tenant-demo" || u.Admin() {
		t.Fatalf("Read = %+v, %v", u, err)
	}
	token := m.CSRFToken(u)
	if !m.CheckCSRF(u, token) || m.CheckCSRF(u, "") || m.CheckCSRF(u, token+"x") {
		t.Error("CSRF check")
	}
	// Another sign-in has another token.
	r2, _ := roundTrip(m, User{ID: "user-demo", Username: "demo", Role: RoleUser, TenantID: "tenant-demo"})
	u2, _ := m.Read(r2)
	if m.CheckCSRF(u2, token) {
		t.Error("a CSRF token works across sign-ins")
	}
}

func TestSessionRefusesForgedExpiredAndForeign(t *testing.T) {
	m := New([]byte("secret"), false)
	_, cookie := roundTrip(m, User{ID: "u", Username: "demo", Role: RoleUser, TenantID: "t"})

	payload, sig, _ := strings.Cut(cookie.Value, ".")
	for name, value := range map[string]string{
		"no signature":  payload,
		"bad signature": payload + "." + sig[:len(sig)-2] + "AA",
		"empty":         "",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: CookieName, Value: value})
		if _, err := m.Read(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other := New([]byte("other"), false)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	if _, err := other.Read(r); err == nil {
		t.Error("cookie of another secret accepted")
	}
	m.now = func() time.Time { return time.Now().Add(Lifetime + time.Minute) }
	if _, err := m.Read(r); err == nil {
		t.Error("expired cookie accepted")
	}
	if _, err := m.Read(httptest.NewRequest("GET", "/", nil)); err != ErrNoSession {
		t.Error("no cookie accepted")
	}
	m.now = time.Now
	if r, _ := roundTrip(m, User{ID: "u", Username: "x", Role: RoleUser}); true {
		if _, err := m.Read(r); err == nil {
			t.Error("a user without a tenant was accepted")
		}
	}
}
