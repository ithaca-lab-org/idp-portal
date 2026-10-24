package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/session"
)

type fakeCluster struct {
	mu      sync.Mutex
	items   map[string]*orders.TeamNamespaceInfo
	created []string
}

func (f *fakeCluster) CreateTeamNamespace(_ context.Context, name, owner, bu, size, orderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[name]; ok {
		return orders.ErrNameTaken
	}
	f.items[name] = &orders.TeamNamespaceInfo{Phase: "Pending", OrderID: orderID}
	f.created = append(f.created, owner+"|"+bu+"|"+size)
	return nil
}

func (f *fakeCluster) TeamNamespace(_ context.Context, name string) (*orders.TeamNamespaceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.items[name]; ok {
		c := *i
		return &c, nil
	}
	return nil, orders.ErrNotFound
}

type env struct {
	mux     *http.ServeMux
	codec   *session.Codec
	cluster *fakeCluster
	svc     *orders.Service
}

func setup(t *testing.T) *env {
	t.Helper()
	codec, err := session.NewCodec([]byte(strings.Repeat("k", 32)), true)
	if err != nil {
		t.Fatal(err)
	}
	h, err := auth.New(auth.Config{Issuer: "http://127.0.0.1:1", ClientID: "idp-portal", RedirectURL: "https://portal.test/auth/callback"}, codec)
	if err != nil {
		t.Fatal(err)
	}
	cl := &fakeCluster{items: map[string]*orders.TeamNamespaceInfo{}}
	svc := orders.NewService(orders.NewMemStore(), cl, 10, 5)
	mux := http.NewServeMux()
	(&API{Auth: h, Svc: svc}).Routes(mux)
	return &env{mux: mux, codec: codec, cluster: cl, svc: svc}
}

func (e *env) cookie(t *testing.T, u auth.User) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := e.codec.Set(rec, e.codec.Name("idp_session"), u, time.Minute); err != nil {
		t.Fatal(err)
	}
	return rec.Result().Cookies()[0]
}

func (e *env) do(r *http.Request, u *auth.User, t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	if u != nil {
		r.AddCookie(e.cookie(t, *u))
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

var alice = auth.User{Sub: "u1", Username: "alice", Email: "alice@example.com", Groups: []string{"payment-bu"}}

func post(body string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/api/orders/namespaces", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

const goodBody = `{"name":"payments-team","businessUnit":"payment-bu","size":"small"}`

func TestEveryRouteNeedsASession(t *testing.T) {
	e := setup(t)
	for _, rq := range []*http.Request{
		httptest.NewRequest("GET", "/api/business-units", nil),
		post(goodBody, nil),
		httptest.NewRequest("GET", "/api/orders", nil),
		httptest.NewRequest("GET", "/api/orders/ORD-2026-000001", nil),
	} {
		w := e.do(rq, nil, t)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status %d", rq.Method, rq.URL.Path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"error"`) || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: want a JSON error, got %q", rq.URL.Path, w.Body.String())
		}
	}
	if len(e.cluster.created) != 0 {
		t.Fatal("an unauthenticated request reached the cluster")
	}
}

func TestBusinessUnits(t *testing.T) {
	e := setup(t)
	w := e.do(httptest.NewRequest("GET", "/api/business-units", nil), &alice, t)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"payment-bu"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	none := auth.User{Sub: "u2", Email: "n@example.com"}
	w = e.do(httptest.NewRequest("GET", "/api/business-units", nil), &none, t)
	if !strings.Contains(w.Body.String(), `"businessUnits":[]`) {
		t.Fatalf("no groups must give an empty list, got %s", w.Body.String())
	}
}

func TestPlaceOrderUsesSessionIdentity(t *testing.T) {
	e := setup(t)
	w := e.do(post(goodBody, nil), &alice, t)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var o orders.Order
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil || o.Owner != "alice@example.com" || o.Status != orders.StatusPending {
		t.Fatalf("order %+v err %v", o, err)
	}
	if len(e.cluster.created) != 1 || e.cluster.created[0] != "alice@example.com|payment-bu|small" {
		t.Fatalf("created %v", e.cluster.created)
	}
}

func TestForgedOwnerIsRejected(t *testing.T) {
	e := setup(t)
	for _, body := range []string{
		`{"name":"x-team","businessUnit":"payment-bu","size":"small","owner":"victim@example.com"}`,
		`{"name":"x-team","businessUnit":"payment-bu","size":"small","Owner":"victim@example.com"}`,
		`{"name":"x-team","businessUnit":"payment-bu","size":"small","orderId":"ORD-1999-000001"}`,
	} {
		if w := e.do(post(body, nil), &alice, t); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, w.Code)
		}
	}
	if len(e.cluster.created) != 0 {
		t.Fatalf("a forged field reached the cluster: %v", e.cluster.created)
	}
}

func TestRequestHygiene(t *testing.T) {
	e := setup(t)
	cases := map[string]struct {
		req  *http.Request
		code int
	}{
		"no content type":      {func() *http.Request { r := post(goodBody, nil); r.Header.Del("Content-Type"); return r }(), 415},
		"form content type":    {post(goodBody, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}), 415},
		"text plain":           {post(goodBody, map[string]string{"Content-Type": "text/plain"}), 415},
		"cross-site fetch":     {post(goodBody, map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		"same-site sibling":    {post(goodBody, map[string]string{"Sec-Fetch-Site": "same-site"}), 403},
		"foreign origin":       {post(goodBody, map[string]string{"Origin": "https://evil.example"}), 403},
		"trailing json":        {post(goodBody+goodBody, nil), 400},
		"not json":             {post(`name=x`, nil), 400},
		"empty body":           {post(``, nil), 400},
		"oversized body":       {post(`{"name":"`+strings.Repeat("a", 9000)+`"}`, nil), 413},
		"json array":           {post(`[]`, nil), 400},
		"wrong method on path": {httptest.NewRequest("PUT", "/api/orders/namespaces", strings.NewReader(goodBody)), 405},
	}
	for name, tc := range cases {
		if w := e.do(tc.req, &alice, t); w.Code != tc.code {
			t.Errorf("%s: want %d, got %d (%s)", name, tc.code, w.Code, w.Body.String())
		}
	}
	if len(e.cluster.created) != 0 {
		t.Fatalf("a rejected request reached the cluster: %v", e.cluster.created)
	}
	if w := e.do(post(goodBody, map[string]string{"Origin": "https://portal.test", "Content-Type": "application/json; charset=utf-8"}), &alice, t); w.Code != http.StatusAccepted {
		t.Fatalf("same-origin request with a charset parameter must work: %d %s", w.Code, w.Body.String())
	}
}

func TestOrderErrorsMapToStatusCodes(t *testing.T) {
	e := setup(t)
	if w := e.do(post(`{"name":"x-team","businessUnit":"other-bu","size":"small"}`, nil), &alice, t); w.Code != 403 {
		t.Errorf("business unit not in groups: %d", w.Code)
	}
	if w := e.do(post(`{"name":"kube-system","businessUnit":"payment-bu","size":"small"}`, nil), &alice, t); w.Code != 400 {
		t.Errorf("reserved name: %d", w.Code)
	}
	if w := e.do(post(`{"name":"dup","businessUnit":"payment-bu","size":"small"}`, nil), &alice, t); w.Code != 202 {
		t.Fatalf("first: %d", w.Code)
	}
	if w := e.do(post(`{"name":"dup","businessUnit":"payment-bu","size":"small"}`, nil), &alice, t); w.Code != 409 {
		t.Errorf("duplicate: %d", w.Code)
	}
	e.svc.CapPerUser = 1
	if w := e.do(post(`{"name":"another","businessUnit":"payment-bu","size":"small"}`, nil), &alice, t); w.Code != 429 {
		t.Errorf("cap: %d", w.Code)
	}
}

func TestOrdersAreOwnerScoped(t *testing.T) {
	e := setup(t)
	w := e.do(post(goodBody, nil), &alice, t)
	var o orders.Order
	_ = json.Unmarshal(w.Body.Bytes(), &o)

	mallory := auth.User{Sub: "u9", Email: "mallory@example.com", Groups: []string{"payment-bu"}}
	if w := e.do(httptest.NewRequest("GET", "/api/orders/"+o.ID, nil), &mallory, t); w.Code != 404 {
		t.Errorf("another user's order: %d", w.Code)
	}
	if w := e.do(httptest.NewRequest("GET", "/api/orders/"+o.ID, nil), &alice, t); w.Code != 200 {
		t.Errorf("own order: %d", w.Code)
	}
	for _, id := range []string{"abc", "ORD-1-1", "ORD-2026-00000x"} {
		if w := e.do(httptest.NewRequest("GET", "/api/orders/"+id, nil), &alice, t); w.Code != 404 {
			t.Errorf("malformed id %q: %d", id, w.Code)
		}
	}
	// the router cleans traversal paths and redirects before any handler runs
	if w := e.do(httptest.NewRequest("GET", "/api/orders/../etc/passwd", nil), &alice, t); w.Code != http.StatusTemporaryRedirect || w.Body.Len() > 200 {
		t.Errorf("traversal path: %d", w.Code)
	}
	w = e.do(httptest.NewRequest("GET", "/api/orders", nil), &mallory, t)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"orders":[]`) {
		t.Errorf("mallory's list must be empty: %d %s", w.Code, w.Body.String())
	}
	w = e.do(httptest.NewRequest("GET", "/api/orders", nil), &alice, t)
	if !strings.Contains(w.Body.String(), o.ID) {
		t.Errorf("own list: %s", w.Body.String())
	}
	for _, q := range []string{"limit=0", "limit=-1", "limit=abc"} {
		if w := e.do(httptest.NewRequest("GET", "/api/orders?"+q, nil), &alice, t); w.Code != 400 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	if w := e.do(httptest.NewRequest("GET", "/api/orders?limit=100000", nil), &alice, t); w.Code != 200 {
		t.Errorf("a huge limit is clamped, not rejected: %d", w.Code)
	}
}
