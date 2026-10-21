package auth

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
			"end_session_endpoint":   srv.URL + "/logout",
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
	h, err := New(Config{Issuer: issuer, ClientID: "idp-portal", RedirectURL: "https://portal.test/auth/callback"}, codec)
	if err != nil {
		t.Fatal(err)
	}
	return h, codec
}

func serve(h *Handler, r *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.Routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func cookieFor(t *testing.T, codec *session.Codec, name string, v any) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := codec.Set(rec, name, v, time.Minute); err != nil {
		t.Fatal(err)
	}
	return rec.Result().Cookies()[0]
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
	if len(ck) != 1 || ck[0].Name != "__Host-idp_login" || !ck[0].HttpOnly || !ck[0].Secure || ck[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie: %+v", ck)
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
	r := httptest.NewRequest("GET", "/auth/callback?code=x&state=evil", nil)
	r.AddCookie(cookieFor(t, codec, h.loginName, loginState{State: "good", Nonce: "n", Verifier: "v"}))
	if w := serve(h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}

func TestCallbackRejectsEmptyLoginState(t *testing.T) {
	h, codec := newHandler(t, "http://127.0.0.1:1")
	r := httptest.NewRequest("GET", "/auth/callback?code=x&state=", nil)
	r.AddCookie(cookieFor(t, codec, h.loginName, loginState{}))
	if w := serve(h, r); w.Code != http.StatusBadRequest {
		t.Fatalf("empty state must be rejected, status %d", w.Code)
	}
}

func TestSessionCookieCannotBeReplayedAsLoginCookie(t *testing.T) {
	h, codec := newHandler(t, "http://127.0.0.1:1")
	sess := cookieFor(t, codec, h.sessionName, User{Sub: "u1", Username: "alice"})
	r := httptest.NewRequest("GET", "/auth/callback?code=x&state=", nil)
	r.AddCookie(&http.Cookie{Name: h.loginName, Value: sess.Value})
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
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(cookieFor(t, codec, h.sessionName, User{Sub: "u1", Username: "alice", Groups: []string{"payments-bu"}}))
	w := serve(h, r)
	var u User
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &u) != nil || u.Username != "alice" || u.Groups[0] != "payments-bu" {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestForgedSessionRejected(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1")
	other, _ := session.NewCodec([]byte(strings.Repeat("x", 32)), true)
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(cookieFor(t, other, h.sessionName, User{Sub: "evil", Username: "admin"}))
	if w := serve(h, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestLogoutRejectsCrossSite(t *testing.T) {
	h, _ := newHandler(t, "http://127.0.0.1:1")
	for name, hdr := range map[string]map[string]string{
		"sec-fetch-site cross-site": {"Sec-Fetch-Site": "cross-site"},
		"same-site (sibling)":       {"Sec-Fetch-Site": "same-site"},
		"foreign origin":            {"Origin": "https://evil.example"},
	} {
		r := httptest.NewRequest("POST", "/auth/logout", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if w := serve(h, r); w.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d", name, w.Code)
		}
	}
}

func TestLogoutSameOriginReturnsEndSessionURL(t *testing.T) {
	iss := fakeIssuer(t)
	h, _ := newHandler(t, iss.URL)
	if serve(h, httptest.NewRequest("GET", "/auth/login", nil)).Code != http.StatusFound { // primes discovery
		t.Fatal("discovery failed")
	}
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	r.Header.Set("Origin", "https://portal.test")
	w := serve(h, r)
	var resp map[string]string
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil ||
		!strings.HasPrefix(resp["end_session_url"], iss.URL+"/logout?") ||
		!strings.Contains(resp["end_session_url"], "client_id=idp-portal") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-idp_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("session cookie not cleared")
	}
}

func tlsIssuer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/auth",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks",
		})
	}))
	t.Cleanup(srv.Close)
	pemFile := t.TempDir() + "/ca.pem"
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(pemFile, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, pemFile
}

func TestUntrustedIssuerCertFailsWithoutExtraCA(t *testing.T) {
	srv, _ := tlsIssuer(t)
	h, _ := newHandler(t, srv.URL)
	if w := serve(h, httptest.NewRequest("GET", "/auth/login", nil)); w.Code != http.StatusBadGateway {
		t.Fatalf("an untrusted issuer certificate must fail, status %d", w.Code)
	}
}

func TestExtraCAFileMakesIssuerTrusted(t *testing.T) {
	srv, pemFile := tlsIssuer(t)
	codec, _ := session.NewCodec([]byte(strings.Repeat("k", 32)), true)
	h, err := New(Config{Issuer: srv.URL, ClientID: "idp-portal", RedirectURL: "https://portal.test/auth/callback", ExtraCAFile: pemFile}, codec)
	if err != nil {
		t.Fatal(err)
	}
	if w := serve(h, httptest.NewRequest("GET", "/auth/login", nil)); w.Code != http.StatusFound {
		t.Fatalf("status %d", w.Code)
	}
}

func TestExtraCAFileMustContainACertificate(t *testing.T) {
	f := t.TempDir() + "/bad.pem"
	_ = os.WriteFile(f, []byte("not a certificate"), 0o600)
	codec, _ := session.NewCodec([]byte(strings.Repeat("k", 32)), true)
	if _, err := New(Config{ExtraCAFile: f}, codec); err == nil {
		t.Fatal("expected an error for a bundle without certificates")
	}
	if _, err := New(Config{ExtraCAFile: "/nonexistent"}, codec); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
