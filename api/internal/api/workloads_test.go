package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/workloads"
)

// fakeWorkloadCluster stands in for the WorkloadIdentity resources; set status the way the
// operator would by writing to items.
type fakeWorkloadCluster struct {
	mu      sync.Mutex
	items   map[string]*workloads.Workload
	calls   []string
	failErr error
}

func (c *fakeWorkloadCluster) Create(_ context.Context, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "create "+ns+"/"+name)
	if c.failErr != nil {
		return c.failErr
	}
	if c.items[ns+"/"+name] != nil {
		return workloads.ErrExists
	}
	c.items[ns+"/"+name] = &workloads.Workload{Namespace: ns, Name: name, ServiceAccount: name,
		Status: workloads.StatusPending, Conditions: []workloads.Condition{}}
	return nil
}

func (c *fakeWorkloadCluster) Get(_ context.Context, ns, name string) (*workloads.Workload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "get "+ns+"/"+name)
	if w := c.items[ns+"/"+name]; w != nil {
		cp := *w
		return &cp, nil
	}
	return nil, workloads.ErrNotFound
}

func (c *fakeWorkloadCluster) List(_ context.Context, ns string) ([]workloads.Workload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "list "+ns)
	out := []workloads.Workload{}
	for _, w := range c.items {
		if w.Namespace == ns {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type workloadEnv struct {
	*secretEnv
	wl *fakeWorkloadCluster
}

// setupWorkloads builds the API with the Workloads page on; the namespaces are those of
// setupSecrets: team-a (payment-bu), team-b (other-bu) and team-p (payment-bu, PENDING).
func setupWorkloads(t *testing.T) *workloadEnv {
	t.Helper()
	s := setupSecrets(t)
	fc := &fakeWorkloadCluster{items: map[string]*workloads.Workload{}}
	s.api.Workloads = workloads.NewService(fc, s.orders)
	return &workloadEnv{secretEnv: s, wl: fc}
}

func TestWorkloadsDisabledAnswer404(t *testing.T) {
	e := setup(t) // Workloads nil: WORKLOADS_ENABLED is not true
	for _, r := range []*http.Request{
		secretReq("GET", "/api/workloads/namespaces", ""),
		secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders"}`),
		secretReq("GET", "/api/workloads?namespace=team-a", ""),
		secretReq("GET", "/api/workloads/team-a/orders", ""),
	} {
		if w := e.do(r, &alice, t); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s %s: %d %s", r.Method, r.URL.Path, w.Code, w.Body.String())
		}
	}
}

func TestWorkloadRoutesNeedASession(t *testing.T) {
	e := setupWorkloads(t)
	for _, r := range []*http.Request{
		secretReq("GET", "/api/workloads/namespaces", ""),
		secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders"}`),
		secretReq("GET", "/api/workloads?namespace=team-a", ""),
		secretReq("GET", "/api/workloads/team-a/orders", ""),
	} {
		if w := e.do(r, nil, t); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", r.Method, r.URL.Path, w.Code)
		}
	}
	if len(e.wl.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the cluster: %v", e.wl.calls)
	}
}

func TestWorkloadNamespacesAndRules(t *testing.T) {
	e := setupWorkloads(t)
	w := e.do(secretReq("GET", "/api/workloads/namespaces", ""), &alice, t)
	want := `{"namespaces":[{"name":"team-a","businessUnit":"payment-bu"}],"reservedNames":["vault-reader","default"],` +
		`"reservedPrefixes":["default-token","vault-reader-token","sh-helm","kube-","istio-","idp-"]}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPlaceWorkload(t *testing.T) {
	e := setupWorkloads(t)
	w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders-api"}`), &alice, t)
	var got workloads.Workload
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusAccepted || got.Namespace != "team-a" || got.Name != "orders-api" || got.ServiceAccount != "orders-api" ||
		got.Status != "PENDING" || got.SpiffeID != "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if e.wl.callsString() != "list team-a,create team-a/orders-api" {
		t.Fatalf("calls: %v", e.wl.calls)
	}
	// duplicate
	if w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders-api"}`), &alice, t); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	// cluster failure: no detail in the answer
	e.wl.failErr = http.ErrAbortHandler
	if w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"other"}`), &alice, t); w.Code != http.StatusBadGateway ||
		strings.Contains(w.Body.String(), "Abort") {
		t.Fatalf("upstream: %d %s", w.Code, w.Body.String())
	}
}

func (c *fakeWorkloadCluster) callsString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.calls, ",")
}

func TestPlaceWorkloadAuthorization(t *testing.T) {
	e := setupWorkloads(t)
	var bodies []string
	for _, ns := range []string{"team-b", "team-p", "no-such-ns", "Bad..NS", "kube-system", ""} {
		w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"`+ns+`","name":"orders"}`), &alice, t)
		if w.Code != http.StatusNotFound {
			t.Errorf("%q: %d", ns, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("responses differ: %q vs %q", b, bodies[0])
		}
	}
	if len(e.wl.calls) != 0 {
		t.Fatalf("unauthorized requests reached the cluster: %v", e.wl.calls)
	}
	none := auth.User{Sub: "u9", Email: "n@example.com"}
	if w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders"}`), &none, t); w.Code != http.StatusNotFound {
		t.Fatalf("no groups: %d", w.Code)
	}
	// a member of the owning group may order in team-b
	bob := auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}
	if w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-b","name":"orders"}`), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPlaceWorkloadValidation(t *testing.T) {
	cases := map[string]struct {
		body, ctype string
		want        int
	}{
		"upper-case":       {`{"namespace":"team-a","name":"Orders"}`, "", 400},
		"empty name":       {`{"namespace":"team-a","name":""}`, "", 400},
		"name too long":    {`{"namespace":"team-a","name":"` + strings.Repeat("a", 41) + `"}`, "", 400},
		"40 chars ok":      {`{"namespace":"team-a","name":"` + strings.Repeat("a", 40) + `"}`, "", 202},
		"dot":              {`{"namespace":"team-a","name":"a.b"}`, "", 400},
		"leading dash":     {`{"namespace":"team-a","name":"-a"}`, "", 400},
		"vault-reader":     {`{"namespace":"team-a","name":"vault-reader"}`, "", 400},
		"default":          {`{"namespace":"team-a","name":"default"}`, "", 400},
		"default-token-x":  {`{"namespace":"team-a","name":"default-token-abc"}`, "", 400},
		"vault-reader-tok": {`{"namespace":"team-a","name":"vault-reader-token-x"}`, "", 400},
		"sh-helm":          {`{"namespace":"team-a","name":"sh-helm-x"}`, "", 400},
		"kube-":            {`{"namespace":"team-a","name":"kube-x"}`, "", 400},
		"istio-":           {`{"namespace":"team-a","name":"istio-x"}`, "", 400},
		"idp-":             {`{"namespace":"team-a","name":"idp-x"}`, "", 400},
		"unknown field":    {`{"namespace":"team-a","name":"x","spec":{"a":1}}`, "", 400},
		"trailing data":    {`{"namespace":"team-a","name":"x"}{}`, "", 400},
		"not json":         {`nope`, "", 400},
		"wrong type":       {`{"namespace":"team-a","name":"x"}`, "text/plain", 415},
		"body too big":     {`{"namespace":"team-a","name":"` + strings.Repeat("x", 9<<10) + `"}`, "", 413},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			e := setupWorkloads(t)
			r := secretReq("POST", "/api/orders/workloads", c.body)
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			w := e.do(r, &alice, t)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if c.want != 202 && e.wl.callsString() != "" {
				t.Fatalf("invalid request reached the cluster: %v", e.wl.calls)
			}
		})
	}
}

func TestPlaceWorkloadNeedsSameOrigin(t *testing.T) {
	e := setupWorkloads(t)
	for _, hdr := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "https://evil.example"}} {
		r := secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders"}`)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		if w := e.do(r, &alice, t); w.Code != http.StatusForbidden {
			t.Errorf("%v: %d", hdr, w.Code)
		}
	}
	if len(e.wl.calls) != 0 {
		t.Fatalf("cross-site request reached the cluster: %v", e.wl.calls)
	}
}

func TestPlaceWorkloadIsRateLimited(t *testing.T) {
	e := setupWorkloads(t)
	var last int
	for i := 0; i < workloadWritesPerMinute+1; i++ {
		last = e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"w`+strings.Repeat("a", i)+`"}`), &alice, t).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last: %d", last)
	}
}

func TestListAndGetWorkload(t *testing.T) {
	e := setupWorkloads(t)
	e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"orders"}`), &alice, t)
	// the operator reports its status
	e.wl.items["team-a/orders"].SpiffeID = "spiffe://idp.example/ns/team-a/sa/orders"
	e.wl.items["team-a/orders"].Hostname = "orders.team-a.apps.example"
	e.wl.items["team-a/orders"].Status = workloads.StatusComplete
	e.wl.items["team-a/orders"].Conditions = []workloads.Condition{{Type: "Synced", Status: "True", Reason: "Synced"}}
	e.wl.items["team-b/hidden"] = &workloads.Workload{Namespace: "team-b", Name: "hidden"}

	w := e.do(secretReq("GET", "/api/workloads?namespace=team-a", ""), &alice, t)
	var list struct{ Workloads []workloads.Workload }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Workloads) != 1 || list.Workloads[0].Status != "COMPLETE" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// the namespace is required: no fan-out over every namespace
	for _, p := range []string{"/api/workloads", "/api/workloads?namespace=", "/api/workloads?namespace=team-a&namespace=team-b"} {
		if w := e.do(secretReq("GET", p, ""), &alice, t); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := e.do(secretReq("GET", "/api/workloads?namespace=team-b", ""), &alice, t); w.Code != http.StatusNotFound {
		t.Fatalf("other bu: %d", w.Code)
	}
	if w := e.do(secretReq("GET", "/api/workloads?x=1", ""), &alice, t); w.Code != http.StatusBadRequest {
		t.Fatalf("bad query: %d", w.Code)
	}

	w = e.do(secretReq("GET", "/api/workloads/team-a/orders", ""), &alice, t)
	var d workloads.Workload
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d.SpiffeID != "spiffe://idp.example/ns/team-a/sa/orders" || d.Hostname != "orders.team-a.apps.example" ||
		d.ServiceAccount != "orders" || d.Namespace != "team-a" || len(d.Conditions) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// foreign, missing and malformed names (and namespaces) are indistinguishable
	var bodies []string
	for _, p := range []string{"/api/workloads/team-b/hidden", "/api/workloads/team-a/missing", "/api/workloads/team-a/Bad..",
		"/api/workloads/team-a/kube-x", "/api/workloads/nope/orders", "/api/workloads/Bad..NS/orders"} {
		w := e.do(secretReq("GET", p, ""), &alice, t)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("404 bodies differ: %q vs %q", b, bodies[0])
		}
	}
}

func TestPlaceWorkloadCap(t *testing.T) {
	e := setupWorkloads(t)
	for i := 0; i < workloads.MaxPerNamespace; i++ {
		e.wl.items[fmt.Sprintf("team-a/w%d", i)] = &workloads.Workload{Namespace: "team-a", Name: fmt.Sprintf("w%d", i)}
	}
	w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-a","name":"one-more"}`), &alice, t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "limit") || e.wl.items["team-a/one-more"] != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// another namespace is unaffected
	bob := auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}
	if w := e.do(secretReq("POST", "/api/orders/workloads", `{"namespace":"team-b","name":"x"}`), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d", w.Code)
	}
}
