package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/pipelines"
)

const (
	goodRepo = "https://github.com/ithaca-lab-org/orders"
	goodSHA  = "0123456789abcdef0123456789abcdef01234567"
)

// fakePipelineCluster stands in for the DeliveryPipeline, DeliveryRun and WorkloadIdentity resources.
type fakePipelineCluster struct {
	mu        sync.Mutex
	workloads map[string]bool
	pipes     map[string]*pipelines.Pipeline
	runs      []*pipelines.Run
	calls     []string
	failErr   error
}

func (c *fakePipelineCluster) rec(s string) { c.calls = append(c.calls, s) }

func (c *fakePipelineCluster) CreatePipeline(_ context.Context, p *pipelines.Pipeline) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec("create pipeline " + p.Namespace + "/" + p.Name)
	if c.failErr != nil {
		return c.failErr
	}
	if c.pipes[p.Namespace+"/"+p.Name] != nil {
		return pipelines.ErrExists
	}
	cp := *p
	c.pipes[p.Namespace+"/"+p.Name] = &cp
	return nil
}

func (c *fakePipelineCluster) GetPipeline(_ context.Context, ns, name string) (*pipelines.Pipeline, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec("get pipeline " + ns + "/" + name)
	if p := c.pipes[ns+"/"+name]; p != nil {
		cp := *p
		return &cp, nil
	}
	return nil, pipelines.ErrNotFound
}

func (c *fakePipelineCluster) ListPipelines(_ context.Context, ns string) ([]pipelines.Pipeline, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec("list pipelines " + ns)
	out := []pipelines.Pipeline{}
	for _, p := range c.pipes {
		if p.Namespace == ns {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *fakePipelineCluster) WorkloadExists(_ context.Context, ns, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec("get workload " + ns + "/" + name)
	return c.workloads[ns+"/"+name], nil
}

func (c *fakePipelineCluster) CreateRun(_ context.Context, r *pipelines.Run) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec("create run " + r.Namespace + "/" + r.Name)
	if c.failErr != nil {
		return c.failErr
	}
	cp := *r
	c.runs = append(c.runs, &cp)
	return nil
}

// ListRuns answers newest first: the fake keeps creation order, so it reverses it.
func (c *fakePipelineCluster) ListRuns(_ context.Context, ns string) ([]pipelines.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec("list runs " + ns)
	out := []pipelines.Run{}
	for i := len(c.runs) - 1; i >= 0; i-- {
		if c.runs[i].Namespace == ns {
			out = append(out, *c.runs[i])
		}
	}
	return out, nil
}

func (c *fakePipelineCluster) callsString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.calls, ",")
}

type pipelineEnv struct {
	*secretEnv
	pc *fakePipelineCluster
}

// setupPipelines builds the API with the Pipelines page on; namespaces as in setupSecrets:
// team-a (payment-bu), team-b (other-bu) and team-p (payment-bu, PENDING). Workload orders exists
// in team-a and team-b.
func setupPipelines(t *testing.T) *pipelineEnv {
	t.Helper()
	s := setupSecrets(t)
	fc := &fakePipelineCluster{workloads: map[string]bool{"team-a/orders": true, "team-b/orders": true},
		pipes: map[string]*pipelines.Pipeline{}}
	s.api.Pipelines = pipelines.NewService(fc, s.orders, []string{"ithaca-lab-org"})
	return &pipelineEnv{secretEnv: s, pc: fc}
}

var bob = auth.User{Sub: "u2", Email: "bob@example.com", Groups: []string{"other-bu"}}

const pipeBody = `{"workload":"orders","repo":"` + goodRepo + `"}`

func runBodyFor(ref string) string { return `{"ref":"` + ref + `"}` }

func TestPipelinesDisabledAnswer404(t *testing.T) {
	e := setup(t) // Pipelines nil: PIPELINES_ENABLED is not true
	for _, r := range pipelineRequests() {
		if w := e.do(r, &alice, t); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("%s %s: %d %s", r.Method, r.URL.Path, w.Code, w.Body.String())
		}
	}
}

func pipelineRequests() []*http.Request {
	return []*http.Request{
		secretReq("GET", "/api/pipelines/namespaces", ""),
		secretReq("GET", "/api/namespaces/team-a/pipelines", ""),
		secretReq("POST", "/api/namespaces/team-a/pipelines", pipeBody),
		secretReq("GET", "/api/namespaces/team-a/pipelines/orders/runs", ""),
		secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", runBodyFor(goodSHA)),
	}
}

func TestPipelineRoutesNeedASession(t *testing.T) {
	e := setupPipelines(t)
	for _, r := range pipelineRequests() {
		if w := e.do(r, nil, t); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", r.Method, r.URL.Path, w.Code)
		}
	}
	if len(e.pc.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the cluster: %v", e.pc.calls)
	}
}

func TestPipelineNamespacesAndRules(t *testing.T) {
	e := setupPipelines(t)
	w := e.do(secretReq("GET", "/api/pipelines/namespaces", ""), &alice, t)
	want := `{"allowedOrgs":["ithaca-lab-org"],"defaultPath":"deploy/base","maxRuns":100,"namespaces":[{"name":"team-a","businessUnit":"payment-bu"}]}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestPlaceAndListPipeline(t *testing.T) {
	e := setupPipelines(t)
	w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines", pipeBody), &alice, t)
	var got pipelines.Pipeline
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusAccepted || got.Name != "orders" || got.Workload != "orders" || got.Repo != goodRepo ||
		got.Path != "deploy/base" || got.Phase != "PENDING" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if p := e.pc.pipes["team-a/orders"]; p == nil || p.Path != "deploy/base" {
		t.Fatalf("stored: %+v", p)
	}
	// a custom path, in the other namespace, by a member of the other group
	w = e.do(secretReq("POST", "/api/namespaces/team-b/pipelines", `{"workload":"orders","repo":"`+goodRepo+`","path":"k8s/prod"}`), &bob, t)
	if w.Code != http.StatusAccepted || e.pc.pipes["team-b/orders"].Path != "k8s/prod" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// the controller reports; the list shows it, only for the caller's namespace
	e.pc.pipes["team-a/orders"].Phase = "READY"
	e.pc.pipes["team-a/orders"].ApplicationName = "team-a-orders"
	w = e.do(secretReq("GET", "/api/namespaces/team-a/pipelines", ""), &alice, t)
	var list struct{ Pipelines []pipelines.Pipeline }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Pipelines) != 1 || list.Pipelines[0].Phase != "READY" || list.Pipelines[0].ApplicationName != "team-a-orders" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// duplicate
	if w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines", pipeBody), &alice, t); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(e.logs.String(), "deliverypipeline created") || !strings.Contains(e.logs.String(), "sub=u1") {
		t.Fatalf("no audit line: %s", e.logs.String())
	}
}

func TestPlacePipelineValidation(t *testing.T) {
	long := strings.Repeat("a", 129)
	cases := map[string]struct {
		body, ctype string
		want        int
	}{
		"ok":                {pipeBody, "", 202},
		"ok path":           {`{"workload":"orders","repo":"` + goodRepo + `","path":"a/b.c-d_e"}`, "", 202},
		"ok mixed case org": {`{"workload":"orders","repo":"https://github.com/Ithaca-Lab-Org/x"}`, "", 202},
		"other org":         {`{"workload":"orders","repo":"https://github.com/evil/orders"}`, "", 400},
		"org prefix":        {`{"workload":"orders","repo":"https://github.com/ithaca-lab-org-evil/orders"}`, "", 400},
		"not github":        {`{"workload":"orders","repo":"https://gitlab.com/ithaca-lab-org/orders"}`, "", 400},
		"http":              {`{"workload":"orders","repo":"http://github.com/ithaca-lab-org/orders"}`, "", 400},
		"dot git":           {`{"workload":"orders","repo":"` + goodRepo + `.git"}`, "", 400},
		"trailing slash":    {`{"workload":"orders","repo":"` + goodRepo + `/"}`, "", 400},
		"ssh":               {`{"workload":"orders","repo":"git@github.com:ithaca-lab-org/orders.git"}`, "", 400},
		"empty repo":        {`{"workload":"orders","repo":""}`, "", 400},
		"dotdot":            {`{"workload":"orders","repo":"` + goodRepo + `","path":"a/../b"}`, "", 400},
		"absolute":          {`{"workload":"orders","repo":"` + goodRepo + `","path":"/etc"}`, "", 400},
		"double slash":      {`{"workload":"orders","repo":"` + goodRepo + `","path":"a//b"}`, "", 400},
		"trailing slash p":  {`{"workload":"orders","repo":"` + goodRepo + `","path":"a/"}`, "", 400},
		"space in path":     {`{"workload":"orders","repo":"` + goodRepo + `","path":"a b"}`, "", 400},
		"path too long":     {`{"workload":"orders","repo":"` + goodRepo + `","path":"` + long + `"}`, "", 400},
		"missing workload":  {`{"workload":"nope","repo":"` + goodRepo + `"}`, "", 400},
		"bad workload":      {`{"workload":"Bad..","repo":"` + goodRepo + `"}`, "", 400},
		"no workload":       {`{"repo":"` + goodRepo + `"}`, "", 400},
		"unknown field":     {`{"workload":"orders","repo":"` + goodRepo + `","labels":{"a":"b"}}`, "", 400},
		"trailing data":     {pipeBody + `{}`, "", 400},
		"not json":          {`nope`, "", 400},
		"wrong type":        {pipeBody, "text/plain", 415},
		"body too big":      {`{"workload":"orders","repo":"` + strings.Repeat("x", 9<<10) + `"}`, "", 413},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			e := setupPipelines(t)
			r := secretReq("POST", "/api/namespaces/team-a/pipelines", c.body)
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			w := e.do(r, &alice, t)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if c.want != 202 && strings.Contains(e.pc.callsString(), "create") {
				t.Fatalf("invalid request reached the cluster: %v", e.pc.calls)
			}
		})
	}
}

func TestPipelineAuthorization(t *testing.T) {
	e := setupPipelines(t)
	var bodies []string
	for _, ns := range []string{"team-b", "team-p", "no-such-ns", "Bad..NS", "kube-system"} {
		for _, r := range []*http.Request{
			secretReq("GET", "/api/namespaces/"+ns+"/pipelines", ""),
			secretReq("POST", "/api/namespaces/"+ns+"/pipelines", pipeBody),
			secretReq("GET", "/api/namespaces/"+ns+"/pipelines/orders/runs", ""),
			secretReq("POST", "/api/namespaces/"+ns+"/pipelines/orders/runs", runBodyFor(goodSHA)),
		} {
			w := e.do(r, &alice, t)
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
	if len(e.pc.calls) != 0 {
		t.Fatalf("unauthorized requests reached the cluster: %v", e.pc.calls)
	}
	none := auth.User{Sub: "u9", Email: "n@example.com"}
	if w := e.do(secretReq("GET", "/api/namespaces/team-a/pipelines", ""), &none, t); w.Code != http.StatusNotFound {
		t.Fatalf("no groups: %d", w.Code)
	}
	// every member of the owning group may do all of it
	if w := e.do(secretReq("POST", "/api/namespaces/team-b/pipelines", pipeBody), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// a pipeline of another namespace is not visible
	if w := e.do(secretReq("GET", "/api/namespaces/team-a/pipelines", ""), &alice, t); strings.Contains(w.Body.String(), "team-b") {
		t.Fatalf("leak: %s", w.Body.String())
	}
}

func TestPipelineWritesNeedSameOrigin(t *testing.T) {
	e := setupPipelines(t)
	e.pc.pipes["team-a/orders"] = &pipelines.Pipeline{Namespace: "team-a", Name: "orders"}
	for _, hdr := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "https://evil.example"}} {
		for _, r := range []*http.Request{
			secretReq("POST", "/api/namespaces/team-a/pipelines", pipeBody),
			secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", runBodyFor(goodSHA)),
		} {
			for k, v := range hdr {
				r.Header.Set(k, v)
			}
			if w := e.do(r, &alice, t); w.Code != http.StatusForbidden {
				t.Errorf("%v %s: %d", hdr, r.URL.Path, w.Code)
			}
		}
	}
	if len(e.pc.calls) != 0 {
		t.Fatalf("cross-site request reached the cluster: %v", e.pc.calls)
	}
}

func TestPipelineWritesAreRateLimited(t *testing.T) {
	e := setupPipelines(t)
	var last int
	for i := 0; i < pipelineWritesPerMinute+1; i++ {
		last = e.do(secretReq("POST", "/api/namespaces/team-a/pipelines", `{"workload":"nope","repo":"`+goodRepo+`"}`), &alice, t).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last: %d", last)
	}
}

func TestPlaceRun(t *testing.T) {
	e := setupPipelines(t)
	e.do(secretReq("POST", "/api/namespaces/team-a/pipelines", pipeBody), &alice, t)
	// the subject is the session's; a body that tries to set it is refused
	if w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", `{"ref":"`+goodSHA+`","requestedBy":"admin"}`), &alice, t); w.Code != http.StatusBadRequest {
		t.Fatalf("requestedBy in the body: %d %s", w.Code, w.Body.String())
	}
	if len(e.pc.runs) != 0 {
		t.Fatal("a run was created from a refused body")
	}
	w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", runBodyFor(goodSHA)), &alice, t)
	var got pipelines.Run
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusAccepted || !regexpRunName.MatchString(got.Name) || got.Pipeline != "orders" || got.Ref != goodSHA ||
		got.RequestedBy != "u1" || got.Phase != "PENDING" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(e.pc.runs) != 1 || e.pc.runs[0].RequestedBy != "u1" || e.pc.runs[0].Name != got.Name {
		t.Fatalf("stored: %+v", e.pc.runs)
	}
	// another user's run carries that user's subject
	e.pc.pipes["team-b/orders"] = &pipelines.Pipeline{Namespace: "team-b", Name: "orders"}
	if w := e.do(secretReq("POST", "/api/namespaces/team-b/pipelines/orders/runs", runBodyFor(goodSHA)), &bob, t); w.Code != http.StatusAccepted ||
		e.pc.runs[1].RequestedBy != "u2" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(e.logs.String(), "deliveryrun created") {
		t.Fatalf("no audit line: %s", e.logs.String())
	}
	// cluster failure: no detail in the answer
	e.pc.failErr = http.ErrAbortHandler
	if w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", runBodyFor(goodSHA)), &alice, t); w.Code != http.StatusBadGateway ||
		strings.Contains(w.Body.String(), "Abort") {
		t.Fatalf("upstream: %d %s", w.Code, w.Body.String())
	}
}

func TestPlaceRunValidation(t *testing.T) {
	cases := map[string]string{
		"tag":          "v1.2.3",
		"branch":       "main",
		"short sha":    "0123456",
		"39 hex":       goodSHA[:39],
		"41 hex":       goodSHA + "a",
		"upper case":   strings.ToUpper(goodSHA),
		"non hex":      strings.Repeat("g", 40),
		"empty":        "",
		"with newline": goodSHA + "\n",
		"sha256":       strings.Repeat("a", 64),
	}
	for label, ref := range cases {
		t.Run(label, func(t *testing.T) {
			e := setupPipelines(t)
			e.pc.pipes["team-a/orders"] = &pipelines.Pipeline{Namespace: "team-a", Name: "orders"}
			w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", `{"ref":`+mustJSON(ref)+`}`), &alice, t)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "deploy by commit") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if len(e.pc.runs) != 0 {
				t.Fatal("a run was created")
			}
		})
	}
	e := setupPipelines(t)
	// the pipeline must exist; a malformed name is the same 404
	for _, name := range []string{"missing", "Bad..", strings.Repeat("a", 41)} {
		if w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/"+name+"/runs", runBodyFor(goodSHA)), &alice, t); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := e.do(secretReq("GET", "/api/namespaces/team-a/pipelines/missing/runs", ""), &alice, t); w.Code != http.StatusNotFound {
		t.Errorf("list: %d", w.Code)
	}
	if len(e.pc.runs) != 0 {
		t.Fatal("a run was created")
	}
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRunCap(t *testing.T) {
	e := setupPipelines(t)
	e.pc.pipes["team-a/orders"] = &pipelines.Pipeline{Namespace: "team-a", Name: "orders"}
	e.pc.pipes["team-b/orders"] = &pipelines.Pipeline{Namespace: "team-b", Name: "orders"}
	for i := 0; i < pipelines.MaxRuns-1; i++ {
		e.pc.runs = append(e.pc.runs, &pipelines.Run{Namespace: "team-a", Name: fmt.Sprintf("r%d", i), Pipeline: "orders", Ref: goodSHA})
	}
	if w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", runBodyFor(goodSHA)), &alice, t); w.Code != http.StatusAccepted {
		t.Fatalf("99th: %d %s", w.Code, w.Body.String())
	}
	w := e.do(secretReq("POST", "/api/namespaces/team-a/pipelines/orders/runs", runBodyFor(goodSHA)), &alice, t)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "100") || !strings.Contains(w.Body.String(), "cannot delete") ||
		len(e.pc.runs) != pipelines.MaxRuns {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// another namespace is unaffected
	if w := e.do(secretReq("POST", "/api/namespaces/team-b/pipelines/orders/runs", runBodyFor(goodSHA)), &bob, t); w.Code != http.StatusAccepted {
		t.Fatalf("%d", w.Code)
	}
}

func TestListRuns(t *testing.T) {
	e := setupPipelines(t)
	e.pc.pipes["team-a/orders"] = &pipelines.Pipeline{Namespace: "team-a", Name: "orders"}
	e.pc.pipes["team-a/other"] = &pipelines.Pipeline{Namespace: "team-a", Name: "other"}
	for i := 0; i < 25; i++ {
		e.pc.runs = append(e.pc.runs, &pipelines.Run{Namespace: "team-a", Name: fmt.Sprintf("orders-%02d", i), Pipeline: "orders",
			Ref: goodSHA, RequestedBy: "u1", Phase: "SUCCEEDED", ExecutionID: "01HX", Message: "done"})
	}
	e.pc.runs = append(e.pc.runs, &pipelines.Run{Namespace: "team-a", Name: "other-1", Pipeline: "other", Ref: goodSHA},
		&pipelines.Run{Namespace: "team-b", Name: "orders-b", Pipeline: "orders", Ref: goodSHA})
	w := e.do(secretReq("GET", "/api/namespaces/team-a/pipelines/orders/runs", ""), &alice, t)
	var list struct{ Runs []pipelines.Run }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Runs) != 20 || list.Runs[0].Name != "orders-24" || list.Runs[19].Name != "orders-05" ||
		list.Runs[0].ExecutionID != "01HX" || list.Runs[0].RequestedBy != "u1" || list.Runs[0].Phase != "SUCCEEDED" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, r := range list.Runs {
		if r.Pipeline != "orders" || r.Namespace != "team-a" {
			t.Fatalf("foreign run: %+v", r)
		}
	}
	// an empty history is an empty array
	w = e.do(secretReq("GET", "/api/namespaces/team-a/pipelines/other/runs", ""), &alice, t)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"runs":[{`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	delete(e.pc.pipes, "team-a/other")
	e.pc.pipes["team-a/fresh"] = &pipelines.Pipeline{Namespace: "team-a", Name: "fresh"}
	w = e.do(secretReq("GET", "/api/namespaces/team-a/pipelines/fresh/runs", ""), &alice, t)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"runs":[]}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

var regexpRunName = regexp.MustCompile(`^orders-[0-9a-f]{8}$`)
