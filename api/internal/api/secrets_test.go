package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
)

const (
	secretValue = "s3cr3t-VALUE-do-not-leak"
	saJWT       = "eyJhbGciOiJSUzI1NiJ9.fake-sa-jwt.sig"
	vaultTok    = "hvs.fake-vault-token"
)

type vaultReq struct {
	Method, Path, Token string
	Body                map[string]any
}

// fakeVault is an httptest Vault: Kubernetes login plus KV v2 data writes and metadata deletes.
type fakeVault struct {
	mu        sync.Mutex
	reqs      []vaultReq
	versions  map[string]int
	writeCode int // non-zero: answer KV writes with this status
	logins    int
	srv       *httptest.Server
}

func newFakeVault(t *testing.T) *fakeVault {
	f := &fakeVault{versions: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.reqs = append(f.reqs, vaultReq{r.Method, r.URL.EscapedPath(), r.Header.Get("X-Vault-Token"), body})
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/auth/kubernetes/login":
			f.logins++
			_, _ = io.WriteString(w, `{"auth":{"client_token":"`+vaultTok+`","lease_duration":3600}}`)
		case r.Header.Get("X-Vault-Token") != vaultTok:
			w.WriteHeader(http.StatusForbidden)
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
			if f.writeCode != 0 {
				w.WriteHeader(f.writeCode)
				return
			}
			p := strings.TrimPrefix(r.URL.Path, "/v1/kv/data/")
			f.versions[p]++
			_, _ = fmt.Fprintf(w, `{"data":{"version":%d}}`, f.versions[p])
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/kv/metadata/"):
			delete(f.versions, strings.TrimPrefix(r.URL.Path, "/v1/kv/metadata/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) requests() []vaultReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vaultReq(nil), f.reqs...)
}

type fakeSecretCluster struct {
	mu      sync.Mutex
	items   map[string]bool
	calls   []string
	failErr error
}

func (c *fakeSecretCluster) EnsureManagedSecret(_ context.Context, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "create "+ns+"/"+name)
	if c.failErr != nil {
		return c.failErr
	}
	c.items[ns+"/"+name] = true
	return nil
}

func (c *fakeSecretCluster) DeleteManagedSecret(_ context.Context, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "delete "+ns+"/"+name)
	delete(c.items, ns+"/"+name)
	return nil
}

type fakePub struct {
	mu     sync.Mutex
	events []secrets.Event
}

func (p *fakePub) PublishSecretEvent(_ context.Context, e secrets.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type secretEnv struct {
	*env
	vault   *fakeVault
	cluster *fakeSecretCluster
	store   *secrets.MemStore
	pub     *fakePub
	logs    *syncBuf
}

// setupSecrets builds the API with the Secrets page on: namespaces team-a (payment-bu,
// COMPLETE), team-b (other-bu, COMPLETE) and team-p (payment-bu, PENDING).
func setupSecrets(t *testing.T) *secretEnv {
	t.Helper()
	logs := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := setup(t)
	ostore := orders.NewMemStore()
	ctx := context.Background()
	for _, n := range []struct {
		name, bu string
		st       orders.Status
	}{{"team-a", "payment-bu", orders.StatusComplete}, {"team-b", "other-bu", orders.StatusComplete}, {"team-p", "payment-bu", orders.StatusPending}} {
		o := &orders.Order{Kind: orders.KindNamespace, Name: n.name, BusinessUnit: n.bu, OwnerSub: "x", Status: orders.StatusPending}
		if err := ostore.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		if n.st != orders.StatusPending {
			if err := ostore.UpdateStatus(ctx, o.ID, orders.StatusPending, n.st, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	fv := newFakeVault(t)
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte(saJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := secrets.DefaultVaultConfig()
	cfg.Addr, cfg.TokenPath = fv.srv.URL, tokFile
	vc, err := secrets.NewVaultClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cl := &fakeSecretCluster{items: map[string]bool{}}
	st := secrets.NewMemStore()
	pub := &fakePub{}
	svc := secrets.NewService(vc, cl, st, ostore)
	svc.Events = pub
	e.api.Secrets = svc
	return &secretEnv{env: e, vault: fv, cluster: cl, store: st, pub: pub, logs: logs}
}

func secretReq(method, path, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

const goodSecret = `{"data":{"DB_PASSWORD":"` + secretValue + `","DB_USER":"app"}}`

func (s *secretEnv) assertNoLeak(t *testing.T, extra ...string) {
	t.Helper()
	logs := s.logs.String()
	for _, needle := range []string{secretValue, saJWT, vaultTok} {
		if strings.Contains(logs, needle) {
			t.Errorf("logs contain %q:\n%s", needle, logs)
		}
	}
	ev, _ := json.Marshal(s.pub.events)
	list, _ := s.store.List(context.Background(), "team-a", 0)
	mdl, _ := json.Marshal(list)
	for _, hay := range append([]string{string(ev), string(mdl)}, extra...) {
		if strings.Contains(hay, secretValue) {
			t.Errorf("value leaked into %s", hay)
		}
	}
}

func TestSecretsDisabledAnswer404(t *testing.T) {
	e := setup(t) // Secrets nil: SECRETS_ENABLED is not true
	for _, r := range []*http.Request{
		secretReq("GET", "/api/namespaces", ""),
		secretReq("GET", "/api/namespaces/team-a/secrets", ""),
		secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret),
		secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""),
	} {
		if w := e.do(r, &alice, t); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s %s: %d %s", r.Method, r.URL.Path, w.Code, w.Body.String())
		}
	}
}

func TestSecretRoutesNeedASession(t *testing.T) {
	s := setupSecrets(t)
	for _, r := range []*http.Request{
		secretReq("GET", "/api/namespaces", ""),
		secretReq("GET", "/api/namespaces/team-a/secrets", ""),
		secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret),
		secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""),
	} {
		if w := s.do(r, nil, t); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", r.Method, r.URL.Path, w.Code)
		}
	}
	if len(s.vault.requests()) != 0 {
		t.Fatal("an unauthenticated request reached Vault")
	}
}

func TestNamespacesListsCompleteOrdersOfMyGroups(t *testing.T) {
	s := setupSecrets(t)
	w := s.do(secretReq("GET", "/api/namespaces", ""), &alice, t)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"namespaces":[{"name":"team-a","businessUnit":"payment-bu"}]}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	none := auth.User{Sub: "u9", Email: "n@example.com"}
	if w := s.do(secretReq("GET", "/api/namespaces", ""), &none, t); !strings.Contains(w.Body.String(), `"namespaces":[]`) {
		t.Fatalf("no groups must list nothing: %s", w.Body.String())
	}
}

func TestSecretAuthorizationDoesNotLeakExistence(t *testing.T) {
	s := setupSecrets(t)
	var bodies []string
	for _, ns := range []string{"team-b", "team-p", "no-such-ns", "Bad..NS", "kube-system"} {
		for _, r := range []*http.Request{
			secretReq("GET", "/api/namespaces/"+ns+"/secrets", ""),
			secretReq("PUT", "/api/namespaces/"+ns+"/secrets/db", goodSecret),
			secretReq("DELETE", "/api/namespaces/"+ns+"/secrets/db", ""),
		} {
			w := s.do(r, &alice, t)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", r.Method, r.URL.Path, w.Code)
			}
			bodies = append(bodies, w.Body.String())
		}
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("responses differ: %q vs %q", b, bodies[0])
		}
	}
	if n := len(s.vault.requests()); n != 0 {
		t.Fatalf("unauthorized requests reached Vault %d times", n)
	}
	if len(s.cluster.calls) != 0 {
		t.Fatalf("unauthorized requests reached the cluster: %v", s.cluster.calls)
	}
	// a member of the owning group may manage team-b
	bob := auth.User{Sub: "u2", Email: "Bob@Example.com", Groups: []string{"other-bu"}}
	if w := s.do(secretReq("PUT", "/api/namespaces/team-b/secrets/db", goodSecret), &bob, t); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPutSecretExactVaultRequestsAndMetadata(t *testing.T) {
	s := setupSecrets(t)
	w := s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db-creds", goodSecret), &alice, t)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"name":"db-creds","keys":["DB_PASSWORD","DB_USER"],"version":1}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	reqs := s.vault.requests()
	if len(reqs) != 2 {
		t.Fatalf("want login + write, got %+v", reqs)
	}
	if l := reqs[0]; l.Method != "POST" || l.Path != "/v1/auth/kubernetes/login" || l.Token != "" ||
		l.Body["role"] != "portal-secrets" || l.Body["jwt"] != saJWT || len(l.Body) != 2 {
		t.Fatalf("login: %+v", l)
	}
	wr := reqs[1]
	data, _ := wr.Body["data"].(map[string]any)
	if wr.Method != "POST" || wr.Path != "/v1/kv/data/team-a/db-creds" || wr.Token != vaultTok || len(wr.Body) != 1 ||
		data["DB_PASSWORD"] != secretValue || data["DB_USER"] != "app" || len(data) != 2 {
		t.Fatalf("write: %+v", wr)
	}
	if !s.cluster.items["team-a/db-creds"] {
		t.Fatal("ManagedSecret not created")
	}
	// a second write is a new version and reuses the cached token
	s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db-creds", `{"data":{"ONLY":"v2"}}`), &alice, t)
	if s.vault.logins != 1 {
		t.Fatalf("token not cached: %d logins", s.vault.logins)
	}
	w = s.do(secretReq("GET", "/api/namespaces/team-a/secrets", ""), &alice, t)
	var got struct{ Secrets []map[string]any }
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Secrets) != 1 || got.Secrets[0]["version"] != float64(2) || got.Secrets[0]["updatedBy"] != "alice@example.com" ||
		fmt.Sprint(got.Secrets[0]["keys"]) != "[ONLY]" || got.Secrets[0]["updatedAt"] == nil {
		t.Fatalf("%s", w.Body.String())
	}
	if len(s.pub.events) != 2 || s.pub.events[0].Type != secrets.EventChanged || s.pub.events[1].SecretVersion != 2 ||
		s.pub.events[0].Actor != "alice@example.com" || s.pub.events[0].Namespace != "team-a" {
		t.Fatalf("events: %+v", s.pub.events)
	}
	s.assertNoLeak(t, w.Body.String())
}

func TestPutSecretValidation(t *testing.T) {
	many := map[string]string{}
	for i := 0; i < 21; i++ {
		many[fmt.Sprintf("K%d", i)] = "v"
	}
	manyJSON, _ := json.Marshal(map[string]any{"data": many})
	big := map[string]string{}
	for i := 0; i < 8; i++ {
		big[fmt.Sprintf("K%d", i)] = strings.Repeat("x", 8192)
	}
	bigJSON, _ := json.Marshal(map[string]any{"data": big})
	cases := map[string]struct {
		name, body, ctype string
		want              int
	}{
		"upper-case name":   {"DB", goodSecret, "", 400},
		"name too long":     {strings.Repeat("a", 41), goodSecret, "", 400},
		"reserved name":     {"vault-reader", goodSecret, "", 400},
		"no keys":           {"db", `{"data":{}}`, "", 400},
		"no data":           {"db", `{}`, "", 400},
		"21 keys":           {"db", string(manyJSON), "", 400},
		"bad key":           {"db", `{"data":{"1BAD":"v"}}`, "", 400},
		"key with dash":     {"db", `{"data":{"A-B":"v"}}`, "", 400},
		"key too long":      {"db", `{"data":{"` + strings.Repeat("A", 65) + `":"v"}}`, "", 400},
		"empty value":       {"db", `{"data":{"A":""}}`, "", 400},
		"value too big":     {"db", `{"data":{"A":"` + strings.Repeat("x", 8193) + `"}}`, "", 400},
		"total too big":     {"db", string(bigJSON), "", 400},
		"non-string value":  {"db", `{"data":{"A":1}}`, "", 400},
		"unknown field":     {"db", `{"data":{"A":"v"},"extra":1}`, "", 400},
		"trailing data":     {"db", `{"data":{"A":"v"}}{}`, "", 400},
		"wrong type":        {"db", goodSecret, "text/plain", 415},
		"body over 128 KiB": {"db", `{"data":{"A":"` + strings.Repeat("x", 130<<10) + `"}}`, "", 413},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			s := setupSecrets(t)
			r := secretReq("PUT", "/api/namespaces/team-a/secrets/"+c.name, c.body)
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			w := s.do(r, &alice, t)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "xxxx") || strings.Contains(w.Body.String(), secretValue) {
				t.Fatalf("error echoes a value: %s", w.Body.String())
			}
			if n := len(s.vault.requests()); n != 0 {
				t.Fatalf("invalid request reached Vault")
			}
		})
	}
	s := setupSecrets(t) // the boundaries are accepted
	ok := map[string]string{strings.Repeat("A", 64): strings.Repeat("y", 8192)}
	for i := 0; i < 19; i++ {
		ok[fmt.Sprintf("_K%d", i)] = "v"
	}
	okJSON, _ := json.Marshal(map[string]any{"data": ok})
	if w := s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/"+strings.Repeat("a", 40), string(okJSON)), &alice, t); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestSecretWritesNeedSameOrigin(t *testing.T) {
	s := setupSecrets(t)
	for _, hdr := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Sec-Fetch-Site": "same-site"}, {"Origin": "https://evil.example"}} {
		for _, r := range []*http.Request{
			secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret),
			secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""),
		} {
			for k, v := range hdr {
				r.Header.Set(k, v)
			}
			if w := s.do(r, &alice, t); w.Code != http.StatusForbidden {
				t.Errorf("%s %v: %d", r.Method, hdr, w.Code)
			}
		}
	}
	if len(s.vault.requests()) != 0 {
		t.Fatal("a cross-site request reached Vault")
	}
	r := secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if w := s.do(r, &alice, t); w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
}

func TestSecretWritesAreRateLimited(t *testing.T) {
	s := setupSecrets(t)
	for i := 0; i < secretWritesPerMinute; i++ {
		if w := s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := s.do(secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""), &alice, t)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d", w.Code)
	}
	bob := auth.User{Sub: "u2", Email: "b@example.com", Groups: []string{"payment-bu"}}
	if w := s.do(secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""), &bob, t); w.Code != 204 {
		t.Fatalf("the limit is per user: %d", w.Code)
	}
}

func TestVaultFailureGives502AndNoMetadata(t *testing.T) {
	for _, code := range []int{500, 403, 307} {
		s := setupSecrets(t)
		s.vault.writeCode = code
		w := s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t)
		if w.Code != http.StatusBadGateway || strings.TrimSpace(w.Body.String()) != `{"error":"the secret backend is unavailable, try again"}` {
			t.Fatalf("%d: %d %s", code, w.Code, w.Body.String())
		}
		if l, _ := s.store.List(context.Background(), "team-a", 0); len(l) != 0 {
			t.Fatalf("metadata written after a failed Vault write: %+v", l)
		}
		if len(s.cluster.calls) != 0 || len(s.pub.events) != 0 {
			t.Fatal("cluster or event touched after a failed Vault write")
		}
		s.assertNoLeak(t, w.Body.String())
	}
	s := setupSecrets(t) // Vault unreachable
	s.vault.srv.Close()
	if w := s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t); w.Code != http.StatusBadGateway {
		t.Fatalf("%d", w.Code)
	}
	if l, _ := s.store.List(context.Background(), "team-a", 0); len(l) != 0 {
		t.Fatal("metadata written while Vault was down")
	}
	s.assertNoLeak(t)
}

func TestManagedSecretFailureKeepsVaultVersionAndRetryHeals(t *testing.T) {
	s := setupSecrets(t)
	s.cluster.failErr = errors.New("apiserver says no")
	w := s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "apiserver") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if s.vault.versions["team-a/db"] != 1 {
		t.Fatal("the Vault write should stand")
	}
	if l, _ := s.store.List(context.Background(), "team-a", 0); len(l) != 0 {
		t.Fatal("metadata must not be written when the resource could not be created")
	}
	s.cluster.failErr = nil
	w = s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"version":2`) || !s.cluster.items["team-a/db"] {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	s.assertNoLeak(t)
}

func TestDeleteSecretFlow(t *testing.T) {
	s := setupSecrets(t)
	s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t)
	before := len(s.vault.requests())
	w := s.do(secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""), &alice, t)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	reqs := s.vault.requests()[before:]
	if len(reqs) != 1 || reqs[0].Method != "DELETE" || reqs[0].Path != "/v1/kv/metadata/team-a/db" || reqs[0].Token != vaultTok {
		t.Fatalf("%+v", reqs)
	}
	if got := strings.Join(s.cluster.calls, ","); got != "create team-a/db,delete team-a/db" {
		t.Fatalf("cluster calls: %s", got)
	}
	if l, _ := s.store.List(context.Background(), "team-a", 0); len(l) != 0 {
		t.Fatal("metadata not deleted")
	}
	last := s.pub.events[len(s.pub.events)-1]
	if last.Type != secrets.EventDeleted || last.Name != "db" || last.SecretVersion != 1 || len(last.Keys) != 2 {
		t.Fatalf("%+v", last)
	}
	// idempotent
	if w := s.do(secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""), &alice, t); w.Code != http.StatusNoContent {
		t.Fatalf("second delete: %d", w.Code)
	}
	s.assertNoLeak(t)
}

func TestSecretEventsCarryNoValues(t *testing.T) {
	s := setupSecrets(t)
	s.do(secretReq("PUT", "/api/namespaces/team-a/secrets/db", goodSecret), &alice, t)
	s.do(secretReq("DELETE", "/api/namespaces/team-a/secrets/db", ""), &alice, t)
	for _, e := range s.pub.events {
		b, _ := json.Marshal(e)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for k := range m {
			switch k {
			case "version", "type", "namespace", "name", "actor", "keys", "secretVersion", "at":
			default:
				t.Errorf("unexpected event field %q", k)
			}
		}
		if strings.Contains(string(b), secretValue) || strings.Contains(string(b), `"app"`) {
			t.Fatalf("event carries a value: %s", b)
		}
	}
	if len(s.pub.events) != 2 {
		t.Fatalf("%d events", len(s.pub.events))
	}
}
