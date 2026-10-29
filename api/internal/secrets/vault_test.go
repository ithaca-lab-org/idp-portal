package secrets

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct{ method, path, token, body string }

func vaultServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body string)) (*httptest.Server, *[]recorded, *sync.Mutex) {
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recorded{r.Method, r.URL.EscapedPath(), r.Header.Get("X-Vault-Token"), string(b)})
		mu.Unlock()
		h(w, r, string(b))
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &mu
}

func client(t *testing.T, addr string) (*VaultClient, string) {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("jwt-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultVaultConfig()
	cfg.Addr, cfg.TokenPath = addr, tok
	c, err := NewVaultClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, tok
}

func TestVaultConfigValidation(t *testing.T) {
	for _, addr := range []string{"", "ftp://v:8200", "http://v:8200/x", "http://u:p@v:8200", "vault:8200", "http://v:8200?a=b"} {
		cfg := DefaultVaultConfig()
		cfg.Addr = addr
		if _, err := NewVaultClient(cfg); err == nil {
			t.Errorf("accepted %q", addr)
		}
	}
	cfg := DefaultVaultConfig()
	cfg.KVMount = "../sys"
	if _, err := NewVaultClient(cfg); err == nil {
		t.Error("accepted a KV mount with ..")
	}
	if _, err := NewVaultClient(DefaultVaultConfig()); err != nil {
		t.Fatal(err)
	}
}

func TestVaultTokenReloginOnExpiryAnd403(t *testing.T) {
	var mu sync.Mutex
	logins, deny := 0, false
	srv, reqs, rmu := vaultServer(t, func(w http.ResponseWriter, r *http.Request, body string) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/auth/kubernetes/login" {
			logins++
			_, _ = io.WriteString(w, `{"auth":{"client_token":"tok-`+string(rune('0'+logins))+`","lease_duration":600}}`)
			return
		}
		if deny {
			deny = false
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"version":3}}`)
	})
	c, tokFile := client(t, srv.URL)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	if v, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); err != nil || v != 3 {
		t.Fatal(v, err)
	}
	if _, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); err != nil || logins != 1 {
		t.Fatalf("token not cached: %d logins, %v", logins, err)
	}
	// the projected token is re-read at every login
	if err := os.WriteFile(tokFile, []byte("jwt-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(600*time.Second - 29*time.Second) // inside the 30 s safety margin
	if _, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); err != nil || logins != 2 {
		t.Fatalf("no re-login before expiry: %d, %v", logins, err)
	}
	mu.Lock()
	deny = true
	mu.Unlock()
	if _, err := c.Write(ctx, "team-a", "db", map[string]string{"A": "b"}); err != nil || logins != 3 {
		t.Fatalf("no re-login after 403: %d, %v", logins, err)
	}
	rmu.Lock()
	defer rmu.Unlock()
	var jwts []string
	for _, r := range *reqs {
		if strings.HasSuffix(r.path, "/login") {
			jwts = append(jwts, r.body)
		}
	}
	if !strings.Contains(jwts[0], `"jwt":"jwt-1"`) || !strings.Contains(jwts[1], `"jwt":"jwt-2"`) {
		t.Fatalf("%v", jwts)
	}
}

func TestVaultNeverFollowsRedirects(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer other.Close()
	srv, _, _ := vaultServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	c, _ := client(t, srv.URL)
	if _, err := c.Write(context.Background(), "team-a", "db", map[string]string{"A": "b"}); !errors.Is(err, ErrVault) {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatal("followed a redirect")
	}
}

func TestVaultRejectsBadPathsWithoutARequest(t *testing.T) {
	srv, reqs, _ := vaultServer(t, func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(500) })
	c, _ := client(t, srv.URL)
	for _, p := range [][2]string{{"../sys", "x"}, {"team-a", "a/b"}, {"team-a", "%2e%2e"}, {"", "x"}, {"team-a", ".."}, {"Team", "x"}} {
		if _, err := c.Write(context.Background(), p[0], p[1], map[string]string{"A": "b"}); !errors.Is(err, ErrVault) {
			t.Errorf("%v accepted", p)
		}
		if err := c.DeleteAll(context.Background(), p[0], p[1]); !errors.Is(err, ErrVault) {
			t.Errorf("%v accepted", p)
		}
	}
	if len(*reqs) != 0 {
		t.Fatalf("requests sent: %v", *reqs)
	}
}

func TestVaultErrorsCarryNoDetail(t *testing.T) {
	srv, _, _ := vaultServer(t, func(w http.ResponseWriter, r *http.Request, body string) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"auth":{"client_token":"tok-secret","lease_duration":60}}`)
			return
		}
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"errors":["echo: `+body+`"]}`)
	})
	c, _ := client(t, srv.URL)
	_, err := c.Write(context.Background(), "team-a", "db", map[string]string{"A": "the-value"})
	if !errors.Is(err, ErrVault) || strings.Contains(err.Error(), "the-value") || strings.Contains(err.Error(), "tok-secret") || strings.Contains(err.Error(), "jwt") {
		t.Fatalf("%v", err)
	}
	// delete: 404 means already gone
	srv2, _, _ := vaultServer(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = io.WriteString(w, `{"auth":{"client_token":"t","lease_duration":60}}`)
			return
		}
		w.WriteHeader(404)
	})
	c2, _ := client(t, srv2.URL)
	if err := c2.DeleteAll(context.Background(), "team-a", "db"); err != nil {
		t.Fatal(err)
	}
}

func TestMemStoreKeepsCreatedAt(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	t1, t2 := time.Unix(1, 0), time.Unix(2, 0)
	_ = s.Upsert(ctx, Metadata{Namespace: "a", Name: "x", Version: 1, UpdatedAt: t1})
	_ = s.Upsert(ctx, Metadata{Namespace: "a", Name: "x", Version: 2, UpdatedAt: t2})
	l, _ := s.List(ctx, "a", "", 0)
	if len(l) != 1 || l[0].Version != 2 || !l[0].CreatedAt.Equal(t1) || !l[0].UpdatedAt.Equal(t2) {
		t.Fatalf("%+v", l)
	}
	if old, _ := s.Delete(ctx, "a", "x"); old == nil || old.Version != 2 {
		t.Fatal(old)
	}
	if old, _ := s.Delete(ctx, "a", "x"); old != nil {
		t.Fatal("second delete found something")
	}
}
