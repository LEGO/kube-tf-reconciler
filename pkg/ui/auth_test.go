package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestAuthenticator(t *testing.T) *authenticator {
	t.Helper()
	return &authenticator{
		cfg: AuthConfig{
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			RedirectURL:  "http://localhost:7777/oauth2/callback",
			Scopes:       []string{"openid"},
		},
		key: []byte("test-session-key"),
	}
}

func TestSignedCookieRoundTrip(t *testing.T) {
	a := newTestAuthenticator(t)

	payload := sessionPayload{Subject: "user-1", Email: "user@example.com", Expires: time.Now().Add(time.Hour).Unix()}
	signed := a.sign(payload)

	var decoded sessionPayload
	if !a.verify(signed, &decoded) {
		t.Fatal("expected signed payload to verify")
	}
	if decoded.Subject != payload.Subject || decoded.Email != payload.Email {
		t.Fatalf("unexpected decoded payload: %+v", decoded)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	a := newTestAuthenticator(t)

	signed := a.sign(sessionPayload{Subject: "user-1", Expires: time.Now().Add(time.Hour).Unix()})
	tampered := "eyJzdWIiOiJhZG1pbiJ9" + signed[len("eyJzdWIiOiJhZG1pbiJ9"):] // different body, same sig
	var decoded sessionPayload
	if a.verify(tampered, &decoded) {
		t.Fatal("expected tampered payload to fail verification")
	}
	if a.verify(signed+".extra", &decoded) {
		t.Fatal("expected appended garbage to fail verification")
	}
}

func TestVerifyRejectsForeignKey(t *testing.T) {
	a := newTestAuthenticator(t)
	other := newTestAuthenticator(t)
	other.key = []byte("another-key")

	signed := a.sign(sessionPayload{Subject: "user-1", Expires: time.Now().Add(time.Hour).Unix()})
	var decoded sessionPayload
	if other.verify(signed, &decoded) {
		t.Fatal("expected payload signed with a different key to fail verification")
	}
}

func TestSessionFromCookieExpiry(t *testing.T) {
	a := newTestAuthenticator(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: a.sign(sessionPayload{Subject: "u", Expires: time.Now().Add(-time.Minute).Unix()})})
	if _, ok := a.sessionFromCookie(req); ok {
		t.Fatal("expected expired session to be rejected")
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: a.sign(sessionPayload{Subject: "u", Expires: time.Now().Add(time.Hour).Unix()})})
	claims, ok := a.sessionFromCookie(req)
	if !ok || claims.Subject != "u" {
		t.Fatalf("expected valid session, got ok=%v claims=%+v", ok, claims)
	}
}

func TestMiddlewareAPIReturns401AndBrowserRedirects(t *testing.T) {
	a := newTestAuthenticator(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := a.middleware(inner)

	// API request without session -> 401.
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for API request, got %d", rec.Code)
	}

	// Browser request without session -> redirect to login.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect for browser request, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/oauth2/login" {
		t.Fatalf("expected redirect to /oauth2/login, got %q", loc)
	}

	// /oauth2/login passes through without a session.
	req = httptest.NewRequest(http.MethodGet, "/oauth2/login", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /oauth2/login to pass through, got %d", rec.Code)
	}

	// API request with valid session -> passes through, claims in context.
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: a.sign(sessionPayload{Subject: "u", Expires: time.Now().Add(time.Hour).Unix()})})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for authenticated API request, got %d", rec.Code)
	}
}

func TestSanitizeNext(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"/workspaces":              "/workspaces",
		"https://evil.example.com": "",
		"//evil.example.com":       "",
		"/\\evil":                  "/\\evil",
	}
	for input, want := range cases {
		if got := sanitizeNext(input); got != want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", input, got, want)
		}
	}
}
