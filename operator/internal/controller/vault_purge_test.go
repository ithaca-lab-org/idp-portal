package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeJWT        = "eyJhbGciOiJSUzI1NiJ9.fake-sa-jwt-DO-NOT-LOG.sig"
	fakeVaultToken = "hvs.fake-vault-token-DO-NOT-LOG"
)

// fakeVault is a minimal KV v2 + Kubernetes-auth Vault. It records every call
// and fails the test on anything outside login, list/delete under
// kv/metadata/ and revoke-self.
type fakeVault struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	data    map[string]bool // "<ns>/<path...>" -> exists
	calls   []string
	deleted []string
	// failStatus, when set, answers every request with that status and a body
	// that echoes the token, to prove bodies never reach errors or logs.
	failStatus int
	// listOverride returns these keys for every list (hostile-key tests).
	listOverride []string
}

func newFakeVault(t *testing.T, keys ...string) *fakeVault {
	fv := &fakeVault{t: t, data: map[string]bool{}}
	for _, k := range keys {
		fv.data[k] = true
	}
	fv.srv = httptest.NewServer(http.HandlerFunc(fv.serve))
	t.Cleanup(fv.srv.Close)
	return fv
}

func (fv *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	call := r.Method + " " + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	fv.calls = append(fv.calls, call)

	if fv.failStatus != 0 {
		w.WriteHeader(fv.failStatus)
		_, _ = w.Write([]byte(`{"errors":["upstream broke; token ` + fakeVaultToken + ` jwt ` + fakeJWT + `"]}`))
		return
	}
	if r.Header.Get("X-Vault-Request") != "true" {
		fv.t.Errorf("missing X-Vault-Request on %s", call)
	}

	const meta = "/v1/kv/metadata/"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/kubernetes/login":
		var in struct{ Role, JWT string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Role != "idp-operator-purge" || in.JWT != fakeJWT {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": fakeVaultToken}})
		return
	case r.Header.Get("X-Vault-Token") != fakeVaultToken:
		w.WriteHeader(http.StatusForbidden)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/token/revoke-self":
		w.WriteHeader(http.StatusNoContent)
		return
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, meta) && r.URL.Query().Get("list") == "true":
		keys := fv.listOverride
		if keys == nil {
			keys = fv.children(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, meta), "/"))
		}
		if len(keys) == 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
		return
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, meta):
		p := strings.TrimPrefix(r.URL.Path, meta)
		fv.deleted = append(fv.deleted, p)
		delete(fv.data, p)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fv.t.Errorf("unexpected Vault call %s", call)
	w.WriteHeader(http.StatusBadRequest)
}

func (fv *fakeVault) children(dir string) []string {
	seen := map[string]bool{}
	for k := range fv.data {
		rest, ok := strings.CutPrefix(k, dir+"/")
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			seen[rest[:i+1]] = true
		} else {
			seen[rest] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (fv *fakeVault) snapshot() (calls []string, remaining []string) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	for k := range fv.data {
		remaining = append(remaining, k)
	}
	sort.Strings(remaining)
	return slices.Clone(fv.calls), remaining
}

func (fv *fakeVault) options(t *testing.T) *VaultOptions {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(fakeJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(fv.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &VaultOptions{
		Addr: u, PurgeRole: defaultVaultPurgeRole, AuthMount: defaultVaultAuthMount,
		KVMount: defaultVaultKVMount, TokenPath: tokenPath, GiveUpAfter: 30 * time.Minute,
	}
}

func noSecrets(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range []string{fakeJWT, fakeVaultToken, "DO-NOT-LOG"} {
		if strings.Contains(s, secret) {
			t.Errorf("%s leaks a credential: %q", what, s)
		}
	}
}

// wantOnlyNamespaceCalls checks every call is login, revoke-self, or a
// list/delete under kv/metadata/<ns>/.
func wantOnlyNamespaceCalls(t *testing.T, ns string, calls []string) {
	t.Helper()
	for _, c := range calls {
		switch {
		case c == "POST /v1/auth/kubernetes/login", c == "POST /v1/auth/token/revoke-self":
		case strings.HasPrefix(c, "GET /v1/kv/metadata/"+ns) && strings.HasSuffix(c, "?list=true") &&
			(strings.HasPrefix(c, "GET /v1/kv/metadata/"+ns+"?") || strings.HasPrefix(c, "GET /v1/kv/metadata/"+ns+"/")):
		case strings.HasPrefix(c, "DELETE /v1/kv/metadata/"+ns+"/") && !strings.Contains(c, "?"):
		default:
			t.Errorf("call outside kv/metadata/%s/: %s", ns, c)
		}
	}
}

func TestPurgeDeletesNestedKeysOfOneNamespaceOnly(t *testing.T) {
	fv := newFakeVault(t,
		"team-a/top", "team-a/app/db", "team-a/app/api", "team-a/x/y/z", "team-a/sp ace/k",
		"team-b/app/db", "team-ab/keep")
	if err := newHTTPPurger(fv.options(t)).Purge(context.Background(), "team-a"); err != nil {
		t.Fatal(err)
	}
	calls, remaining := fv.snapshot()
	if want := []string{"team-ab/keep", "team-b/app/db"}; !slices.Equal(remaining, want) {
		t.Errorf("remaining = %v, want %v", remaining, want)
	}
	wantOnlyNamespaceCalls(t, "team-a", calls)
	if calls[0] != "POST /v1/auth/kubernetes/login" || calls[len(calls)-1] != "POST /v1/auth/token/revoke-self" {
		t.Errorf("calls should start with login and end with revoke-self: %v", calls)
	}
	if !slices.Contains(calls, "DELETE /v1/kv/metadata/team-a/sp%20ace/k") {
		t.Errorf("segments are not escaped: %v", calls)
	}
}

func TestPurgeTreatsMissingNamespaceAsSuccess(t *testing.T) {
	fv := newFakeVault(t, "other/app/db")
	if err := newHTTPPurger(fv.options(t)).Purge(context.Background(), "team-empty"); err != nil {
		t.Fatalf("404 must be success: %v", err)
	}
	calls, _ := fv.snapshot()
	want := []string{"POST /v1/auth/kubernetes/login", "GET /v1/kv/metadata/team-empty?list=true", "POST /v1/auth/token/revoke-self"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestPurgeRejectsHostileNamespaceNames(t *testing.T) {
	fv := newFakeVault(t, "sys/x")
	p := newHTTPPurger(fv.options(t))
	for _, ns := range []string{"", "..", "../sys", "a/b", "team-a/../../sys", "%2e%2e", "Team", "a.b",
		"team?list=true", "-lead", strings.Repeat("a", 64), "a\x00b"} {
		if err := p.Purge(context.Background(), ns); !errors.Is(err, ErrInvalidNamespace) {
			t.Errorf("Purge(%q) = %v, want ErrInvalidNamespace", ns, err)
		}
	}
	if calls, _ := fv.snapshot(); len(calls) != 0 {
		t.Errorf("hostile names reached Vault: %v", calls)
	}
}

func TestPurgeRefusesHostileKeysFromVault(t *testing.T) {
	for _, key := range []string{"../", "..", "a/b", "./", "x%2f..", "q?x", ""} {
		fv := newFakeVault(t)
		fv.listOverride = []string{key}
		err := newHTTPPurger(fv.options(t)).Purge(context.Background(), "team-a")
		if err == nil || !strings.Contains(err.Error(), "refusing unexpected key") {
			t.Errorf("key %q: err = %v", key, err)
		}
		calls, _ := fv.snapshot()
		for _, c := range calls {
			if strings.HasPrefix(c, "DELETE") {
				t.Errorf("key %q: issued %s", key, c)
			}
		}
	}
}

func TestPurgeBoundsDepth(t *testing.T) {
	fv := newFakeVault(t, "team-a/"+strings.Repeat("d/", vaultMaxDepth+2)+"leaf")
	err := newHTTPPurger(fv.options(t)).Purge(context.Background(), "team-a")
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("err = %v", err)
	}
}

func TestPurgeErrorsNeverCarryCredentials(t *testing.T) {
	t.Run("vault returns 503", func(t *testing.T) {
		fv := newFakeVault(t, "team-a/k")
		fv.failStatus = http.StatusServiceUnavailable
		err := newHTTPPurger(fv.options(t)).Purge(context.Background(), "team-a")
		if err == nil {
			t.Fatal("want error")
		}
		noSecrets(t, "error", err.Error())
	})
	t.Run("vault unreachable", func(t *testing.T) {
		fv := newFakeVault(t)
		o := fv.options(t)
		fv.srv.Close()
		err := newHTTPPurger(o).Purge(context.Background(), "team-a")
		if err == nil || !strings.Contains(err.Error(), "vault unreachable") {
			t.Fatalf("err = %v", err)
		}
		noSecrets(t, "error", err.Error())
	})
	t.Run("login denied", func(t *testing.T) {
		fv := newFakeVault(t, "team-a/k")
		o := fv.options(t)
		o.PurgeRole = "wrong-role"
		err := newHTTPPurger(o).Purge(context.Background(), "team-a")
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("err = %v", err)
		}
		noSecrets(t, "error", err.Error())
		if _, remaining := fv.snapshot(); len(remaining) != 1 {
			t.Error("data deleted without login")
		}
	})
	t.Run("redirect not followed", func(t *testing.T) {
		hit := false
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
		defer other.Close()
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		defer redir.Close()
		fv := newFakeVault(t)
		o := fv.options(t)
		o.Addr, _ = url.Parse(redir.URL)
		if err := newHTTPPurger(o).Purge(context.Background(), "team-a"); err == nil {
			t.Fatal("want error")
		}
		if hit {
			t.Error("redirect was followed")
		}
	})
}

func TestVaultOptionsFromEnv(t *testing.T) {
	envOf := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if o, err := VaultOptionsFromEnv(envOf(nil)); o != nil || err != nil {
		t.Errorf("unset: %v %v", o, err)
	}
	if o, err := VaultOptionsFromEnv(envOf(map[string]string{"VAULT_ENABLED": "false"})); o != nil || err != nil {
		t.Errorf("false: %v %v", o, err)
	}
	o, err := VaultOptionsFromEnv(envOf(map[string]string{"VAULT_ENABLED": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr.String() != "http://vault.vault.svc.cluster.local:8200" || o.PurgeRole != "idp-operator-purge" || o.AuthMount != "kubernetes" ||
		o.KVMount != "kv" || o.TokenPath != "/var/run/secrets/vault/token" || o.GiveUpAfter != 30*time.Minute {
		t.Errorf("defaults = %+v", o)
	}
	for k, v := range map[string]string{
		"VAULT_ENABLED":             "maybe",
		"VAULT_ADDR":                "ftp://vault",
		"VAULT_KV_MOUNT":            "kv/../sys",
		"VAULT_AUTH_MOUNT":          "a b",
		"VAULT_PURGE_ROLE":          "role/../x",
		"VAULT_PURGE_GIVE_UP_AFTER": "-1m",
	} {
		env := map[string]string{"VAULT_ENABLED": "true", k: v}
		if _, err := VaultOptionsFromEnv(envOf(env)); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
	for _, addr := range []string{"http://user:pw@vault:8200", "http://vault:8200?x=1", "vault:8200"} {
		if _, err := VaultOptionsFromEnv(envOf(map[string]string{"VAULT_ENABLED": "1", "VAULT_ADDR": addr})); err == nil {
			t.Errorf("VAULT_ADDR=%q accepted", addr)
		}
	}
}

func TestPurgeBackoff(t *testing.T) {
	giveUp := 30 * time.Minute
	for elapsed, want := range map[time.Duration]time.Duration{
		0:                10 * time.Second,
		2 * time.Minute:  30 * time.Second,
		25 * time.Minute: 5 * time.Minute,
		29 * time.Minute: time.Minute + time.Second,
	} {
		if got := purgeBackoff(elapsed, giveUp); got != want {
			t.Errorf("purgeBackoff(%s) = %s, want %s", elapsed, got, want)
		}
	}
}
