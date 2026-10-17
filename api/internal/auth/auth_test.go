package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ithaca-lab-org/idp-portal/api/internal/session"
)

func fakeIssuer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/auth",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHandler(t *testing.T, issuer string) (*Handler, *session.Codec) {
	t.Helper()
	codec, err := session.NewCodec([]byte(strings.Repeat("k", 32)), true)
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Issuer: issuer, ClientID: "idp-portal", RedirectURL: "https://portal.test/auth/callback"}, codec), codec
}

func serve(h *Handler, r *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.Routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestLoginRedirectsWithPKCEStateNonce(t *testing.T) {
	iss := fakeIssuer(t)
	h, _ := newHandler(t, iss.URL)
	w := serve(h, httptest.NewRequest("GET", "/auth/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("status %d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("client_id") != "idp-portal" {
		t.Fatalf("missing params: %v", q)
	}
	ck := w.Result().Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly || !ck[0].Secure || ck[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie flags: %+v", ck)
	}
}

func TestLoginProviderDown(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1")
	if w := serve(h, httptest.NewRequest("GET", "/auth/login", nil)); w.Code != http.StatusBadGateway {
		t.Fatalf("status %d", w.Code)
	}
}

func TestCallbackWithoutLoginCookie(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1")
	if w := serve(h, httptest.NewRequest("GET", "/auth/callback?code=x&state=y", nil)); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}

func TestCallbackStateMismatch(t *testing.T) {
	h, codec := newHandler(t, "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	if err := codec.Set(rec, loginCookie, loginState{State: "good", Nonce: "n", Verifier: "v"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/auth/callback?code=x&state=evil", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if w := serve(h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}

func TestMeUnauthorizedWithoutSession(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1")
	if w := serve(h, httptest.NewRequest("GET", "/auth/me", nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestMeWithSession(t *testing.T) {
	h, codec := newHandler(t, "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	if err := codec.Set(rec, sessionCookie, User{Sub: "u1", Username: "alice", Groups: []string{"payments-bu"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	w := serve(h, r)
	var u User
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &u) != nil || u.Username != "alice" || u.Groups[0] != "payments-bu" {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestForgedSessionRejected(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1")
	other, _ := session.NewCodec([]byte(strings.Repeat("x", 32)), true)
	rec := httptest.NewRecorder()
	_ = other.Set(rec, sessionCookie, User{Sub: "evil", Username: "admin"}, time.Minute)
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(rec.Result().Cookies()[0])
	if w := serve(h, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}
