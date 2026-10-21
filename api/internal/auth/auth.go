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
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/ithaca-lab-org/idp-portal/api/internal/session"
)

const (
	loginCookieBase   = "idp_login"
	sessionCookieBase = "idp_session"
	loginTTL          = 10 * time.Minute
	defaultSessionTTL = 15 * time.Minute // groups are only re-read at login, so keep this short
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
	cfg         Config
	codec       *session.Codec
	loginName   string
	sessionName string
	origin      string // scheme://host of RedirectURL, for the same-origin check

	sf         singleflight.Group
	mu         sync.RWMutex
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	endSession string
}

func New(cfg Config, codec *session.Codec) *Handler {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	h := &Handler{
		cfg: cfg, codec: codec,
		loginName: codec.Name(loginCookieBase), sessionName: codec.Name(sessionCookieBase),
	}
	if u, err := url.Parse(cfg.RedirectURL); err == nil {
		h.origin = u.Scheme + "://" + u.Host
	}
	return h
}

// Routes registers /auth/login, /auth/callback, /auth/logout and /auth/me.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", h.login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.HandleFunc("GET /auth/me", h.me)
}

// discover resolves the provider lazily so the API can start before Keycloak is ready. Concurrent
// callers share one lookup (singleflight) and no lock is held during the network call.
func (h *Handler) discover(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	h.mu.RLock()
	oc, v := h.oauth, h.verifier
	h.mu.RUnlock()
	if oc != nil {
		return oc, v, nil
	}
	ch := h.sf.DoChan("discover", func() (any, error) {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := oidc.NewProvider(dctx, h.cfg.Issuer)
		if err != nil {
			return nil, err
		}
		var meta struct {
			EndSession string `json:"end_session_endpoint"`
		}
		_ = p.Claims(&meta)
		h.mu.Lock()
		h.oauth = &oauth2.Config{
			ClientID:    h.cfg.ClientID,
			Endpoint:    p.Endpoint(),
			RedirectURL: h.cfg.RedirectURL,
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
		}
		h.verifier = p.Verifier(&oidc.Config{ClientID: h.cfg.ClientID})
		h.endSession = meta.EndSession
		h.mu.Unlock()
		return nil, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, nil, res.Err
		}
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
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
	if err := h.codec.Set(w, h.loginName, loginState{State: state, Nonce: nonce, Verifier: verifier}, loginTTL); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	url := oc.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	var ls loginState
	if err := h.codec.Get(r, h.loginName, &ls); err != nil {
		http.Error(w, "login session expired, start again", http.StatusBadRequest)
		return
	}
	h.codec.Clear(w, h.loginName) // single use
	if ls.State == "" || ls.Nonce == "" || ls.Verifier == "" {
		http.Error(w, "login session invalid, start again", http.StatusBadRequest)
		return
	}
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
	if err := h.codec.Set(w, h.sessionName, u, h.cfg.SessionTTL); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// sameOrigin rejects cross-site browser requests to state-changing endpoints. Requests without
// Sec-Fetch-Site and Origin come from non-browser clients, which cannot be CSRF'd.
func (h *Handler) sameOrigin(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		return o == h.origin
	}
	return true
}

// logout clears the local session and returns the Keycloak end-session URL for the browser to
// visit, so the SSO session ends too and the next login asks for credentials.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	h.codec.Clear(w, h.sessionName)
	h.mu.RLock()
	end := h.endSession
	h.mu.RUnlock()
	resp := map[string]string{}
	if end != "" {
		q := url.Values{"client_id": {h.cfg.ClientID}, "post_logout_redirect_uri": {h.origin + "/"}}
		resp["end_session_url"] = end + "?" + q.Encode()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
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
	if err := h.codec.Get(r, h.sessionName, &u); err != nil || u.Sub == "" {
		return User{}, false
	}
	return u, true
}
