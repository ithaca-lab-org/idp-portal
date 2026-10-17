// Package auth implements OIDC login (Authorization Code + PKCE) against Keycloak and a
// signed-cookie session. Only identity claims are kept in the cookie, never tokens.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ithaca-lab-org/idp-portal/api/internal/session"
)

const (
	loginCookie   = "idp_login"
	sessionCookie = "idp_session"
	loginTTL      = 10 * time.Minute
)

type Config struct {
	Issuer      string
	ClientID    string
	RedirectURL string
	SessionTTL  time.Duration
}

type User struct {
	Sub      string   `json:"sub"`
	Username string   `json:"username"`
	Email    string   `json:"email,omitempty"`
	Groups   []string `json:"groups"`
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

type Handler struct {
	cfg   Config
	codec *session.Codec

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func New(cfg Config, codec *session.Codec) *Handler {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = time.Hour
	}
	return &Handler{cfg: cfg, codec: codec}
}

// Routes registers /auth/login, /auth/callback, /auth/logout and /auth/me.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", h.login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.HandleFunc("GET /auth/me", h.me)
}

// discover resolves the provider lazily so the API can start before Keycloak is ready.
func (h *Handler) discover(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.oauth != nil {
		return h.oauth, h.verifier, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, h.cfg.Issuer)
	if err != nil {
		return nil, nil, err
	}
	h.oauth = &oauth2.Config{
		ClientID:    h.cfg.ClientID,
		Endpoint:    p.Endpoint(),
		RedirectURL: h.cfg.RedirectURL,
		Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
	}
	h.verifier = p.Verifier(&oidc.Config{ClientID: h.cfg.ClientID})
	return h.oauth, h.verifier, nil
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	oc, _, err := h.discover(r.Context())
	if err != nil {
		slog.Error("oidc discovery failed", "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	state, err1 := randomString()
	nonce, err2 := randomString()
	if err := errors.Join(err1, err2); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	if err := h.codec.Set(w, loginCookie, loginState{State: state, Nonce: nonce, Verifier: verifier}, loginTTL); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	url := oc.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	var ls loginState
	if err := h.codec.Get(r, loginCookie, &ls); err != nil {
		http.Error(w, "login session expired, start again", http.StatusBadRequest)
		return
	}
	h.codec.Clear(w, loginCookie) // single use
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ls.State)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		slog.Warn("oidc error from provider", "error", e)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	oc, verifier, err := h.discover(r.Context())
	if err != nil {
		slog.Error("oidc discovery failed", "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	tok, err := oc.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		slog.Warn("code exchange failed", "err", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ls.Nonce)) != 1 {
		slog.Warn("id token rejected", "err", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Username string   `json:"preferred_username"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
	}
	if err := idt.Claims(&claims); err != nil {
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	u := User{Sub: idt.Subject, Username: claims.Username, Email: claims.Email, Groups: claims.Groups}
	if err := h.codec.Set(w, sessionCookie, u, h.cfg.SessionTTL); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (h *Handler) logout(w http.ResponseWriter, _ *http.Request) {
	h.codec.Clear(w, sessionCookie)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, ok := h.UserFrom(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(u)
}

// UserFrom returns the logged-in user from the session cookie.
func (h *Handler) UserFrom(r *http.Request) (User, bool) {
	var u User
	if err := h.codec.Get(r, sessionCookie, &u); err != nil || u.Sub == "" {
		return User{}, false
	}
	return u, true
}
