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
	"time"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
	"github.com/ithaca-lab-org/idp-portal/api/internal/resources"
)

// fakeResourceCluster stands in for the BucketClaim and ResourceGrant resources; set status the
// way the broker would by writing to buckets and grants. wls holds the WorkloadIdentities.
type fakeResourceCluster struct {
	mu      sync.Mutex
	buckets map[string]*resources.Bucket
	grants  map[string]*resources.Grant
	wls     map[string]bool
	calls   []string
	failErr error
	// delay widens the window between the count and the create, to show the service's lock
	delay time.Duration
}

func (c *fakeResourceCluster) CreateBucket(_ context.Context, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "create bucket "+ns+"/"+name)
	if c.failErr != nil {
		return c.failErr
	}
	if c.buckets[ns+"/"+name] != nil {
		return resources.ErrBucketExists
	}
	c.buckets[ns+"/"+name] = &resources.Bucket{Namespace: ns, Name: name, Status: resources.StatusPending}
	return nil
}

func (c *fakeResourceCluster) GetBucket(_ context.Context, ns, name string) (*resources.Bucket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "get bucket "+ns+"/"+name)
	if b := c.buckets[ns+"/"+name]; b != nil {
		cp := *b
		return &cp, nil
	}
	return nil, resources.ErrNotFound
}

func (c *fakeResourceCluster) ListBuckets(_ context.Context, ns string) ([]resources.Bucket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "list buckets "+ns)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	out := []resources.Bucket{}
	for _, b := range c.buckets {
		if b.Namespace == ns {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *fakeResourceCluster) CreateGrant(_ context.Context, ns, name, workload, bucket, access string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "create grant "+ns+"/"+name)
	if c.failErr != nil {
		return c.failErr
	}
	if old := c.grants[ns+"/"+name]; old != nil {
		if old.Workload != workload || old.Bucket != bucket {
			return resources.ErrGrantCollision
		}
		return resources.ErrGrantExists
	}
	c.grants[ns+"/"+name] = &resources.Grant{Namespace: ns, Name: name, Workload: workload, Bucket: bucket, Access: access, Status: resources.StatusPending}
	return nil
}

func (c *fakeResourceCluster) ListGrants(_ context.Context, ns string) ([]resources.Grant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "list grants "+ns)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	out := []resources.Grant{}
	for _, g := range c.grants {
		if g.Namespace == ns {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *fakeResourceCluster) DeleteGrant(_ context.Context, ns, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "delete grant "+ns+"/"+name)
	if c.grants[ns+"/"+name] == nil {
		return resources.ErrNotFound
	}
	delete(c.grants, ns+"/"+name)
	return nil
}

func (c *fakeResourceCluster) WorkloadExists(_ context.Context, ns, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "get workload "+ns+"/"+name)
	return c.wls[ns+"/"+name], nil
}

func (c *fakeResourceCluster) callsString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.calls, ",")
}

type resourceEnv struct {
	*secretEnv
	rc *fakeResourceCluster
}

// setupResources builds the API with the Resources page on; the namespaces are those of
// setupSecrets: team-a (payment-bu), team-b (other-bu) and team-p (payment-bu, PENDING). The
// workload "orders" exists in team-a and team-b.
func setupResources(t *testing.T) *resourceEnv {
	t.Helper()
	s := setupSecrets(t)
	fc := &fakeResourceCluster{buckets: map[string]*resources.Bucket{}, grants: map[string]*resources.Grant{},
		wls: map[string]bool{"team-a/orders": true, "team-a/worker": true, "team-b/orders": true}}
	s.api.Resources = resources.NewService(fc, s.orders)
	return &resourceEnv{secretEnv: s, rc: fc}
}

// completeNamespace adds a COMPLETE namespace order for a business unit.
func completeNamespace(t *testing.T, e *secretEnv, name, bu string) {
	t.Helper()
	ctx := context.Background()
	o := &orders.Order{Kind: orders.KindNamespace, Name: name, BusinessUnit: bu, OwnerSub: "x", Status: orders.StatusPending}
	if err := e.orders.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := e.orders.UpdateStatus(ctx, o.ID, orders.StatusPending, orders.StatusComplete, ""); err != nil {
		t.Fatal(err)
	}
}

func TestResourcesDisabledAnswer404(t *testing.T) {
	e := setup(t) // Resources nil: RESOURCES_ENABLED is not true
	for _, r := range []*http.Request{
		secretReq("GET", "/api/resources/namespaces", ""),
		secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`),
		secretReq("GET", "/api/buckets?namespace=team-a", ""),
		secretReq("GET", "/api/buckets/team-a/data", ""),
		secretReq("POST", "/api/orders/grants", `{"namespace":"team-a","workload":"orders","bucket":"data","access":"reader"}`),
		secretReq("GET", "/api/grants?namespace=team-a", ""),
		secretReq("DELETE", "/api/grants/team-a/orders-data", ""),
	} {
		if w := e.do(r, &alice, t); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s %s: %d %s", r.Method, r.URL.Path, w.Code, w.Body.String())
		}
	}
}

func TestResourceRoutesNeedASession(t *testing.T) {
	e := setupResources(t)
	for _, r := range []*http.Request{
		secretReq("GET", "/api/resources/namespaces", ""),
		secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`),
		secretReq("GET", "/api/buckets?namespace=team-a", ""),
		secretReq("GET", "/api/buckets/team-a/data", ""),
		secretReq("POST", "/api/orders/grants", `{"namespace":"team-a","workload":"orders","bucket":"data","access":"reader"}`),
		secretReq("GET", "/api/grants?namespace=team-a", ""),
		secretReq("DELETE", "/api/grants/team-a/orders-data", ""),
	} {
		if w := e.do(r, nil, t); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", r.Method, r.URL.Path, w.Code)
		}
	}
	if len(e.rc.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the cluster: %v", e.rc.calls)
	}
}

func TestResourceNamespacesAndRules(t *testing.T) {
	e := setupResources(t)
	w := e.do(secretReq("GET", "/api/resources/namespaces", ""), &alice, t)
	want := `{"access":[{"value":"reader","label":"can read objects"},{"value":"writer","label":"can read and write objects"}],` +
		`"maxBuckets":3,"maxGrants":10,"maxNameLength":18,"maxNamespaceLength":31,"namespaces":[{"name":"team-a","businessUnit":"payment-bu"}]}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPlaceBucket(t *testing.T) {
	e := setupResources(t)
	w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`), &alice, t)
	var got resources.Bucket
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusAccepted || got.Namespace != "team-a" || got.Name != "data" || got.Status != "PENDING" || got.BucketName != "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if e.rc.callsString() != "list buckets team-a,create bucket team-a/data" {
		t.Fatalf("calls: %v", e.rc.calls)
	}
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`), &alice, t); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	// cluster failure: no detail in the answer
	e.rc.failErr = http.ErrAbortHandler
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"other"}`), &alice, t); w.Code != http.StatusBadGateway ||
		strings.Contains(w.Body.String(), "Abort") {
		t.Fatalf("upstream: %d %s", w.Code, w.Body.String())
	}
}

func TestPlaceBucketAuthorization(t *testing.T) {
	e := setupResources(t)
	var bodies []string
	for _, ns := range []string{"team-b", "team-p", "no-such-ns", "Bad..NS", "kube-system", ""} {
		w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"`+ns+`","name":"data"}`), &alice, t)
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
	if len(e.rc.calls) != 0 {
		t.Fatalf("unauthorized requests reached the cluster: %v", e.rc.calls)
	}
	none := auth.User{Sub: "u9", Email: "n@example.com"}
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`), &none, t); w.Code != http.StatusNotFound {
		t.Fatalf("no groups: %d", w.Code)
	}
	bob := auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-b","name":"data"}`), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// TestPlaceBucketRules is the API half of the name rules; the drift test in package resources
// checks them against the CRD and the broker.
func TestPlaceBucketRules(t *testing.T) {
	cases := map[string]struct {
		body, ctype string
		want        int
	}{
		"upper-case":       {`{"namespace":"team-a","name":"Data"}`, "", 400},
		"empty name":       {`{"namespace":"team-a","name":""}`, "", 400},
		"19 chars":         {`{"namespace":"team-a","name":"` + strings.Repeat("a", 19) + `"}`, "", 400},
		"18 chars ok":      {`{"namespace":"team-a","name":"` + strings.Repeat("a", 18) + `"}`, "", 202},
		"dot":              {`{"namespace":"team-a","name":"a.b"}`, "", 400},
		"leading dash":     {`{"namespace":"team-a","name":"-a"}`, "", 400},
		"contains google":  {`{"namespace":"team-a","name":"my-google"}`, "", 400},
		"starts with goog": {`{"namespace":"team-a","name":"goog-data"}`, "", 400},
		"unknown field":    {`{"namespace":"team-a","name":"x","spec":{"a":1}}`, "", 400},
		"trailing data":    {`{"namespace":"team-a","name":"x"}{}`, "", 400},
		"not json":         {`nope`, "", 400},
		"wrong type":       {`{"namespace":"team-a","name":"x"}`, "text/plain", 415},
		"body too big":     {`{"namespace":"team-a","name":"` + strings.Repeat("x", 9<<10) + `"}`, "", 413},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			e := setupResources(t)
			r := secretReq("POST", "/api/orders/buckets", c.body)
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			w := e.do(r, &alice, t)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if c.want != 202 && strings.Contains(e.rc.callsString(), "create") {
				t.Fatalf("invalid request reached the cluster: %v", e.rc.calls)
			}
		})
	}
}

// TestPlaceBucketLongOrGoogleNamespace: the broker cannot name a bucket for a namespace longer
// than 31 characters or one Cloud Storage refuses; the answer is a 422 that says only that.
func TestPlaceBucketLongOrGoogleNamespace(t *testing.T) {
	e := setupResources(t)
	for _, ns := range []string{strings.Repeat("n", 32), strings.Repeat("n", 40), "google-team", "goog-team"} {
		completeNamespace(t, e.secretEnv, ns, "payment-bu")
		w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"`+ns+`","name":"data"}`), &alice, t)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "too long") || strings.Contains(w.Body.String(), ns) {
			t.Errorf("%s: %d %s", ns, w.Code, w.Body.String())
		}
	}
	// 31 characters is fine
	ns31 := strings.Repeat("n", 31)
	completeNamespace(t, e.secretEnv, ns31, "payment-bu")
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"`+ns31+`","name":"data"}`), &alice, t); w.Code != http.StatusAccepted {
		t.Fatalf("31: %d %s", w.Code, w.Body.String())
	}
	// a namespace the caller does not own stays a plain 404, however long
	long := strings.Repeat("z", 35)
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"`+long+`","name":"data"}`), &alice, t); w.Code != http.StatusNotFound {
		t.Fatalf("foreign long: %d", w.Code)
	}
	if strings.Contains(e.rc.callsString(), strings.Repeat("n", 32)) {
		t.Fatalf("a refused namespace reached the cluster: %v", e.rc.calls)
	}
}

func TestPlaceBucketNeedsSameOrigin(t *testing.T) {
	e := setupResources(t)
	for _, hdr := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "https://evil.example"}} {
		for _, r := range []*http.Request{
			secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`),
			secretReq("POST", "/api/orders/grants", `{"namespace":"team-a","workload":"orders","bucket":"data","access":"reader"}`),
			secretReq("DELETE", "/api/grants/team-a/orders-data", ""),
		} {
			for k, v := range hdr {
				r.Header.Set(k, v)
			}
			if w := e.do(r, &alice, t); w.Code != http.StatusForbidden {
				t.Errorf("%s %v: %d", r.Method, hdr, w.Code)
			}
		}
	}
	if len(e.rc.calls) != 0 {
		t.Fatalf("cross-site request reached the cluster: %v", e.rc.calls)
	}
}

func TestResourceWritesAreRateLimited(t *testing.T) {
	e := setupResources(t)
	var last int
	for i := 0; i < resourceWritesPerMinute+1; i++ {
		last = e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"b`+strings.Repeat("a", i)+`"}`), &alice, t).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last: %d", last)
	}
	// the limit is shared by grants and revokes: one budget of writes
	if w := e.do(secretReq("DELETE", "/api/grants/team-a/x", ""), &alice, t); w.Code != http.StatusTooManyRequests {
		t.Fatalf("delete: %d", w.Code)
	}
}

func TestResourceReadsAreRateLimited(t *testing.T) {
	e := setupResources(t)
	var last int
	for i := 0; i < resourceReadsPerMinute+1; i++ {
		last = e.do(secretReq("GET", "/api/buckets?namespace=team-a", ""), &alice, t).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last: %d", last)
	}
}

func TestListAndGetBucket(t *testing.T) {
	e := setupResources(t)
	e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"data"}`), &alice, t)
	// the broker reports its status
	b := e.rc.buckets["team-a/data"]
	b.BucketName, b.Status = "idp-team-a-data-0123abcd", resources.StatusComplete
	e.rc.grants["team-a/orders-data"] = &resources.Grant{Namespace: "team-a", Name: "orders-data", Workload: "orders", Bucket: "data", Access: "reader", Status: resources.StatusComplete}
	e.rc.grants["team-a/orders-other"] = &resources.Grant{Namespace: "team-a", Name: "orders-other", Workload: "orders", Bucket: "other", Access: "writer"}
	e.rc.buckets["team-b/hidden"] = &resources.Bucket{Namespace: "team-b", Name: "hidden"}

	w := e.do(secretReq("GET", "/api/buckets?namespace=team-a", ""), &alice, t)
	var list struct{ Buckets []resources.Bucket }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Buckets) != 1 || list.Buckets[0].Status != "COMPLETE" || list.Buckets[0].BucketName != "idp-team-a-data-0123abcd" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// the namespace is required: no fan-out over every namespace
	for _, p := range []string{"/api/buckets", "/api/buckets?namespace=", "/api/buckets?namespace=team-a&namespace=team-b", "/api/buckets?x=1",
		"/api/grants", "/api/grants?namespace=", "/api/grants?namespace=team-a&x=1"} {
		if w := e.do(secretReq("GET", p, ""), &alice, t); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/api/buckets?namespace=team-b", "/api/grants?namespace=team-b", "/api/grants?namespace=nope"} {
		if w := e.do(secretReq("GET", p, ""), &alice, t); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}

	w = e.do(secretReq("GET", "/api/buckets/team-a/data", ""), &alice, t)
	var d resources.BucketDetail
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if w.Code != 200 || d.BucketName != "idp-team-a-data-0123abcd" || d.Namespace != "team-a" || len(d.Grants) != 1 || d.Grants[0].Name != "orders-data" ||
		d.Grants[0].Access != "reader" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// a bucket with no grants lists an empty array, not null
	e.rc.buckets["team-a/empty"] = &resources.Bucket{Namespace: "team-a", Name: "empty"}
	if w := e.do(secretReq("GET", "/api/buckets/team-a/empty", ""), &alice, t); !strings.Contains(w.Body.String(), `"grants":[]`) {
		t.Fatalf("%s", w.Body.String())
	}
	// foreign, missing and malformed names (and namespaces) are indistinguishable
	var bodies []string
	for _, p := range []string{"/api/buckets/team-b/hidden", "/api/buckets/team-a/missing", "/api/buckets/team-a/Bad..",
		"/api/buckets/team-a/goog-x", "/api/buckets/nope/data", "/api/buckets/Bad..NS/data"} {
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

func TestBucketCap(t *testing.T) {
	e := setupResources(t)
	for i := 0; i < resources.MaxBuckets; i++ {
		e.rc.buckets[fmt.Sprintf("team-a/b%d", i)] = &resources.Bucket{Namespace: "team-a", Name: fmt.Sprintf("b%d", i)}
	}
	w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-a","name":"one-more"}`), &alice, t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "limit") || e.rc.buckets["team-a/one-more"] != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// another namespace is unaffected
	bob := auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}
	if w := e.do(secretReq("POST", "/api/orders/buckets", `{"namespace":"team-b","name":"x"}`), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d", w.Code)
	}
}

func grantReq(ns, workload, bucket, access string) *http.Request {
	return secretReq("POST", "/api/orders/grants",
		fmt.Sprintf(`{"namespace":%q,"workload":%q,"bucket":%q,"access":%q}`, ns, workload, bucket, access))
}

func TestPlaceGrant(t *testing.T) {
	e := setupResources(t)
	e.rc.buckets["team-a/data"] = &resources.Bucket{Namespace: "team-a", Name: "data"}
	w := e.do(grantReq("team-a", "orders", "data", "writer"), &alice, t)
	var got resources.Grant
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusAccepted || got.Name != "orders-data" || got.Workload != "orders" || got.Bucket != "data" || got.Access != "writer" || got.Status != "PENDING" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if e.rc.callsString() != "get workload team-a/orders,get bucket team-a/data,list grants team-a,create grant team-a/orders-data" {
		t.Fatalf("calls: %v", e.rc.calls)
	}
	g := e.rc.grants["team-a/orders-data"]
	if g == nil || g.Workload != "orders" || g.Bucket != "data" || g.Access != "writer" {
		t.Fatalf("stored: %+v", g)
	}
	// the deterministic name makes a repeat a duplicate, whatever the access
	for _, access := range []string{"writer", "reader"} {
		if w := e.do(grantReq("team-a", "orders", "data", access), &alice, t); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already") {
			t.Fatalf("duplicate %s: %d %s", access, w.Code, w.Body.String())
		}
	}
	if e.rc.grants["team-a/orders-data"].Access != "writer" {
		t.Fatal("a duplicate changed the grant")
	}
	// cluster failure: no detail in the answer
	e.rc.failErr = http.ErrAbortHandler
	if w := e.do(grantReq("team-a", "worker", "data", "reader"), &alice, t); w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "Abort") {
		t.Fatalf("upstream: %d %s", w.Code, w.Body.String())
	}
}

func TestPlaceGrantAuthorizationAndExistence(t *testing.T) {
	e := setupResources(t)
	e.rc.buckets["team-a/data"] = &resources.Bucket{Namespace: "team-a", Name: "data"}
	e.rc.buckets["team-b/theirs"] = &resources.Bucket{Namespace: "team-b", Name: "theirs"}
	// a foreign or unknown namespace, a workload or bucket that is missing (or lives in another
	// namespace), all answer the same 404 and nothing is written
	var bodies []string
	for _, r := range []*http.Request{
		grantReq("team-b", "orders", "theirs", "reader"),
		grantReq("team-p", "orders", "data", "reader"),
		grantReq("no-such-ns", "orders", "data", "reader"),
		grantReq("Bad..NS", "orders", "data", "reader"),
		grantReq("team-a", "ghost", "data", "reader"),
		grantReq("team-a", "orders", "ghost", "reader"),
		grantReq("team-a", "orders", "theirs", "reader"),
	} {
		w := e.do(r, &alice, t)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", r.URL, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("responses differ: %q vs %q", b, bodies[0])
		}
	}
	if strings.Contains(e.rc.callsString(), "create") || len(e.rc.grants) != 0 {
		t.Fatalf("a refused request wrote: %v", e.rc.calls)
	}
	// the first four never reached the cluster at all
	if strings.HasPrefix(e.rc.callsString(), "get") && strings.Count(e.rc.callsString(), "team-b") > 0 {
		t.Fatalf("a foreign namespace reached the cluster: %v", e.rc.calls)
	}
}

func TestPlaceGrantValidation(t *testing.T) {
	cases := map[string]struct {
		body string
		want int
	}{
		"bad access":      {`{"namespace":"team-a","workload":"orders","bucket":"data","access":"admin"}`, 400},
		"empty access":    {`{"namespace":"team-a","workload":"orders","bucket":"data","access":""}`, 400},
		"upper access":    {`{"namespace":"team-a","workload":"orders","bucket":"data","access":"Reader"}`, 400},
		"bad workload":    {`{"namespace":"team-a","workload":"Orders","bucket":"data","access":"reader"}`, 400},
		"reserved wl":     {`{"namespace":"team-a","workload":"kube-x","bucket":"data","access":"reader"}`, 400},
		"empty workload":  {`{"namespace":"team-a","workload":"","bucket":"data","access":"reader"}`, 400},
		"bad bucket":      {`{"namespace":"team-a","workload":"orders","bucket":"Data","access":"reader"}`, 400},
		"long bucket":     {`{"namespace":"team-a","workload":"orders","bucket":"` + strings.Repeat("a", 19) + `","access":"reader"}`, 400},
		"unknown field":   {`{"namespace":"team-a","workload":"orders","bucket":"data","access":"reader","name":"x"}`, 400},
		"member smuggled": {`{"namespace":"team-a","workload":"orders","bucket":"data","access":"reader","member":"allUsers"}`, 400},
		"missing field":   {`{"namespace":"team-a","workload":"orders","bucket":"data"}`, 400},
		"ok":              {`{"namespace":"team-a","workload":"orders","bucket":"data","access":"reader"}`, 202},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			e := setupResources(t)
			e.rc.buckets["team-a/data"] = &resources.Bucket{Namespace: "team-a", Name: "data"}
			w := e.do(secretReq("POST", "/api/orders/grants", c.body), &alice, t)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if c.want != 202 && strings.Contains(e.rc.callsString(), "create") {
				t.Fatalf("invalid request reached the cluster: %v", e.rc.calls)
			}
		})
	}
	// the longest names give a 59-character grant name, which the CRD accepts
	e := setupResources(t)
	wl, bk := strings.Repeat("w", 40), strings.Repeat("b", 18)
	e.rc.wls["team-a/"+wl] = true
	e.rc.buckets["team-a/"+bk] = &resources.Bucket{Namespace: "team-a", Name: bk}
	if w := e.do(grantReq("team-a", wl, bk, "reader"), &alice, t); w.Code != http.StatusAccepted || e.rc.grants["team-a/"+wl+"-"+bk] == nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestGrantCap(t *testing.T) {
	e := setupResources(t)
	e.rc.buckets["team-a/data"] = &resources.Bucket{Namespace: "team-a", Name: "data"}
	for i := 0; i < resources.MaxGrants; i++ {
		e.rc.grants[fmt.Sprintf("team-a/g%d", i)] = &resources.Grant{Namespace: "team-a", Name: fmt.Sprintf("g%d", i), Workload: "x", Bucket: "y"}
	}
	w := e.do(grantReq("team-a", "orders", "data", "reader"), &alice, t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "limit") || e.rc.grants["team-a/orders-data"] != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// another namespace is unaffected
	bob := auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}
	e.rc.buckets["team-b/data"] = &resources.Bucket{Namespace: "team-b", Name: "data"}
	if w := e.do(grantReq("team-b", "orders", "data", "reader"), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestListGrants(t *testing.T) {
	e := setupResources(t)
	e.rc.grants["team-a/orders-data"] = &resources.Grant{Namespace: "team-a", Name: "orders-data", Workload: "orders", Bucket: "data", Access: "reader", Status: "COMPLETE"}
	e.rc.grants["team-b/hidden-x"] = &resources.Grant{Namespace: "team-b", Name: "hidden-x"}
	w := e.do(secretReq("GET", "/api/grants?namespace=team-a", ""), &alice, t)
	var list struct{ Grants []resources.Grant }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Grants) != 1 || list.Grants[0].Name != "orders-data" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestRevokeGrant(t *testing.T) {
	e := setupResources(t)
	e.rc.grants["team-a/orders-data"] = &resources.Grant{Namespace: "team-a", Name: "orders-data"}
	e.rc.grants["team-b/orders-data"] = &resources.Grant{Namespace: "team-b", Name: "orders-data"}
	// foreign namespaces, missing grants and malformed names are indistinguishable
	var bodies []string
	for _, p := range []string{"/api/grants/team-b/orders-data", "/api/grants/team-p/orders-data", "/api/grants/nope/orders-data", "/api/grants/Bad..NS/orders-data",
		"/api/grants/team-a/missing", "/api/grants/team-a/Bad..", "/api/grants/team-a/" + strings.Repeat("a", 60)} {
		w := e.do(secretReq("DELETE", p, ""), &alice, t)
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
	if e.rc.grants["team-b/orders-data"] == nil || e.rc.grants["team-a/orders-data"] == nil {
		t.Fatal("a refused revoke deleted something")
	}
	for _, c := range strings.Split(e.rc.callsString(), ",") {
		if strings.Contains(c, "team-b") || strings.Contains(c, "team-p") || strings.Contains(c, "nope") {
			t.Fatalf("a foreign namespace reached the cluster: %s", c)
		}
	}
	w := e.do(secretReq("DELETE", "/api/grants/team-a/orders-data", ""), &alice, t)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 || e.rc.grants["team-a/orders-data"] != nil || e.rc.grants["team-b/orders-data"] == nil {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	// there is no route to delete a bucket
	e.rc.buckets["team-a/data"] = &resources.Bucket{Namespace: "team-a", Name: "data"}
	for _, p := range []string{"/api/buckets/team-a/data", "/api/buckets?namespace=team-a", "/api/orders/buckets"} {
		if w := e.do(secretReq("DELETE", p, ""), &alice, t); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("DELETE %s: %d", p, w.Code)
		}
	}
	if e.rc.buckets["team-a/data"] == nil {
		t.Fatal("a bucket was deleted")
	}
}

// TestGrantNameCollision: workload a-b on bucket c and workload a on bucket b-c share the name
// a-b-c. The second is refused with its own message, and nothing of the first is shown beyond
// the fact that the name is taken.
func TestGrantNameCollision(t *testing.T) {
	e := setupResources(t)
	e.rc.wls["team-a/a-b"], e.rc.wls["team-a/a"] = true, true
	e.rc.buckets["team-a/c"] = &resources.Bucket{Namespace: "team-a", Name: "c"}
	e.rc.buckets["team-a/b-c"] = &resources.Bucket{Namespace: "team-a", Name: "b-c"}
	if w := e.do(grantReq("team-a", "a-b", "c", "reader"), &alice, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	w := e.do(grantReq("team-a", "a", "b-c", "writer"), &alice, t)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "collides") || strings.Contains(w.Body.String(), "a-b") {
		t.Fatalf("collision: %d %s", w.Code, w.Body.String())
	}
	if g := e.rc.grants["team-a/a-b-c"]; g.Workload != "a-b" || g.Access != "reader" {
		t.Fatalf("the first grant changed: %+v", g)
	}
	// the same pair again is the idempotent "already has access"
	if w := e.do(grantReq("team-a", "a-b", "c", "reader"), &alice, t); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already has access") {
		t.Fatalf("repeat: %d %s", w.Code, w.Body.String())
	}
}

// TestGrantInLongNamespaceIsRefusedUpFront: no bucket can exist there, so no grant is attempted.
func TestGrantInLongNamespaceIsRefusedUpFront(t *testing.T) {
	e := setupResources(t)
	ns := strings.Repeat("n", 32)
	completeNamespace(t, e.secretEnv, ns, "payment-bu")
	if w := e.do(grantReq(ns, "orders", "data", "reader"), &alice, t); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if e.rc.callsString() != "" {
		t.Fatalf("reached the cluster: %v", e.rc.calls)
	}
}

// parallelOrders sends ten requests at once as who and counts the 202s and 409s.
func parallelOrders(t *testing.T, e *resourceEnv, who *auth.User, mk func(i int) *http.Request) (created, refused int) {
	t.Helper()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := e.do(mk(i), who, t)
			mu.Lock()
			defer mu.Unlock()
			switch w.Code {
			case http.StatusAccepted:
				created++
			case http.StatusConflict:
				refused++
			default:
				t.Errorf("%d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	return
}

// TestCapsHoldUnderParallelOrders: ten orders at once, with a slow count, still create at most
// the cap (the service serializes count-then-create per namespace).
func TestCapsHoldUnderParallelOrders(t *testing.T) {
	e := setupResources(t)
	e.rc.delay = 5 * time.Millisecond
	c, r := parallelOrders(t, e, &alice, func(i int) *http.Request {
		return secretReq("POST", "/api/orders/buckets", fmt.Sprintf(`{"namespace":"team-a","name":"p%d"}`, i))
	})
	if c != resources.MaxBuckets || r != 10-c || len(e.rc.buckets) != resources.MaxBuckets {
		t.Fatalf("buckets: created %d refused %d total %d", c, r, len(e.rc.buckets))
	}

	bob := auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}
	e.rc.buckets["team-b/data"] = &resources.Bucket{Namespace: "team-b", Name: "data"}
	for i := 0; i < 10; i++ {
		e.rc.wls[fmt.Sprintf("team-b/w%d", i)] = true
	}
	for i := 0; i < resources.MaxGrants-3; i++ {
		e.rc.grants[fmt.Sprintf("team-b/old%d", i)] = &resources.Grant{Namespace: "team-b", Name: fmt.Sprintf("old%d", i)}
	}
	c, r = parallelOrders(t, e, &bob, func(i int) *http.Request { return grantReq("team-b", fmt.Sprintf("w%d", i), "data", "reader") })
	if c != 3 || r != 7 || len(e.rc.grants) != resources.MaxGrants {
		t.Fatalf("grants: created %d refused %d total %d", c, r, len(e.rc.grants))
	}
}
