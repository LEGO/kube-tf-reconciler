package ui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookieName = "krec_ui_session"
	stateCookieName   = "krec_ui_oauth_state"
	sessionTTL        = 12 * time.Hour
	stateTTL          = 10 * time.Minute
)

// AuthConfig holds the OAuth2/OIDC settings for the dashboard. When all of
// Issuer, ClientID and ClientSecret are set, authentication is enabled.
type AuthConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	Scopes        []string
	SessionSecret []byte
}

type userClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
}

type sessionPayload struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Expires int64  `json:"exp"`
}

type statePayload struct {
	State string `json:"state"`
	Nonce string `json:"nonce"`
	Next  string `json:"next,omitempty"`
	Exp   int64  `json:"exp"`
}

type authContextKey struct{}

// userFromContext returns the authenticated user claims, if any.
func userFromContext(ctx context.Context) (*userClaims, bool) {
	claims, ok := ctx.Value(authContextKey{}).(*userClaims)
	return claims, ok
}

type authenticator struct {
	cfg      AuthConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	key      []byte
}

// newAuthenticator performs OIDC discovery and prepares the verifier. It
// returns an error if the issuer is unreachable or misconfigured.
func newAuthenticator(ctx context.Context, cfg AuthConfig) (*authenticator, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("oauth requires issuer, client-id and client-secret")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", cfg.Issuer, err)
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if len(cfg.SessionSecret) == 0 {
		cfg.SessionSecret = randomTokenBytes()
		slog.Warn("no session secret configured, generated ephemeral one (sessions reset on restart)")
	}
	return &authenticator{
		cfg:      cfg,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		key:      cfg.SessionSecret,
	}, nil
}

// middleware protects everything except /oauth2/* endpoints. Browsers are
// redirected to the login endpoint; API clients receive 401.
func (a *authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/oauth2/") {
			next.ServeHTTP(w, r)
			return
		}
		claims, ok := a.sessionFromCookie(r)
		if ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, claims)))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, a.loginURL(r), http.StatusFound)
	})
}

func (a *authenticator) loginURL(r *http.Request) string {
	login := "/oauth2/login"
	if r.URL.Path != "/" {
		return login + "?next=" + r.URL.Path
	}
	return login
}

// handleLogin redirects the user to the OAuth2 provider authorization endpoint.
func (a *authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := statePayload{
		State: randomToken(),
		Nonce: randomToken(),
		Next:  sanitizeNext(r.FormValue("next")),
		Exp:   time.Now().Add(stateTTL).Unix(),
	}
	http.SetCookie(w, a.signedCookie(stateCookieName, state, stateTTL))
	http.Redirect(w, r, a.oauth2Config(r).AuthCodeURL(state.State, oidc.Nonce(state.Nonce)), http.StatusFound)
}

// handleCallback exchanges the authorization code for tokens, verifies the
// ID token and establishes the session cookie.
func (a *authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	next := "/"
	state, err := a.stateFromCookie(r)
	if err != nil {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	if r.FormValue("state") != state.State {
		http.Error(w, "oauth state mismatch", http.StatusBadRequest)
		return
	}
	if state.Next != "" {
		next = state.Next
	}

	oauth2Cfg := a.oauth2Config(r)
	token, err := oauth2Cfg.Exchange(r.Context(), r.FormValue("code"))
	if err != nil {
		http.Error(w, fmt.Sprintf("oauth token exchange failed: %s", err), http.StatusUnauthorized)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Error(w, "no id_token in token response", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Error(w, fmt.Sprintf("id_token verification failed: %s", err), http.StatusUnauthorized)
		return
	}
	if idToken.Nonce != state.Nonce {
		http.Error(w, "id_token nonce mismatch", http.StatusBadRequest)
		return
	}

	var claims userClaims
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, fmt.Sprintf("parsing id_token claims failed: %s", err), http.StatusUnauthorized)
		return
	}
	if claims.Subject == "" {
		claims.Subject = idToken.Subject
	}

	// Clear the one-time state cookie and establish the session.
	http.SetCookie(w, a.expireCookie(stateCookieName))
	http.SetCookie(w, a.signedCookie(sessionCookieName, sessionPayload{
		Subject: claims.Subject,
		Email:   claims.Email,
		Expires: time.Now().Add(sessionTTL).Unix(),
	}, sessionTTL))
	http.Redirect(w, r, next, http.StatusFound)
}

// handleLogout clears the session cookie and redirects home.
func (a *authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, a.expireCookie(sessionCookieName))
	http.Redirect(w, r, "/", http.StatusFound)
}

// oauth2Config returns the oauth2 config, deriving the callback URL from the
// request when no explicit redirect URL is configured.
func (a *authenticator) oauth2Config(r *http.Request) *oauth2.Config {
	redirect := a.cfg.RedirectURL
	if redirect == "" {
		redirect = requestScheme(r) + "://" + r.Host + "/oauth2/callback"
	}
	return &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  redirect,
		Scopes:       a.cfg.Scopes,
	}
}

func sanitizeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return ""
	}
	return next
}

func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// --- signed cookie helpers ---

func (a *authenticator) signedCookie(name string, payload any, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    a.sign(payload),
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (a *authenticator) expireCookie(name string) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

func (a *authenticator) sign(payload any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Error("marshalling cookie payload failed", "error", err)
		return ""
	}
	body := base64.RawURLEncoding.EncodeToString(data)
	return body + "." + a.mac(body)
}

func (a *authenticator) mac(body string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *authenticator) verify(signed string, payload any) bool {
	body, sig, found := strings.Cut(signed, ".")
	if !found {
		return false
	}
	if !hmac.Equal([]byte(sig), []byte(a.mac(body))) {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	if err := json.Unmarshal(data, payload); err != nil {
		return false
	}
	return true
}

func (a *authenticator) sessionFromCookie(r *http.Request) (*userClaims, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	var session sessionPayload
	if !a.verify(c.Value, &session) {
		return nil, false
	}
	if time.Now().Unix() > session.Expires {
		return nil, false
	}
	return &userClaims{Subject: session.Subject, Email: session.Email}, true
}

func (a *authenticator) stateFromCookie(r *http.Request) (*statePayload, error) {
	c, err := r.Cookie(stateCookieName)
	if err != nil || c.Value == "" {
		return nil, errors.New("missing oauth state cookie")
	}
	var state statePayload
	if !a.verify(c.Value, &state) {
		return nil, errors.New("invalid oauth state cookie")
	}
	if time.Now().Unix() > state.Exp {
		return nil, errors.New("expired oauth state")
	}
	return &state, nil
}

func randomToken() string {
	return base64.RawURLEncoding.EncodeToString(randomTokenBytes())
}

func randomTokenBytes() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("generating random token: %s", err))
	}
	return b
}
