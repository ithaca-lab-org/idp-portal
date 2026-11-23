package delivery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secretBody = "SECRET-INTERNALS-java.lang.NullPointerException at com.netflix"

func TestHTTPGate(t *testing.T) {
	var gotPath, gotBody, gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody, gotAuth = r.Method+" "+r.URL.Path, string(b), r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/api/v1/applications/demo-orders":
			io.WriteString(w, `{"attributes":{"description":"idp-managed namespace=demo workload=orders"}}`)
		case r.URL.Path == "/api/v1/applications/missing":
			http.Error(w, secretBody, http.StatusNotFound)
		case r.URL.Path == "/api/v1/applications/demo-orders/pipelineConfigs":
			io.WriteString(w, `[{"id":"p1","name":"other"},{"id":"p2","name":"deploy"}]`)
		case r.URL.Path == "/api/v1/pipelines/demo-orders/deploy":
			io.WriteString(w, `{"ref":"/pipelines/01EXEC"}`)
		case r.URL.Path == "/api/v1/pipelines/01EXEC":
			io.WriteString(w, `{"status":"SUCCEEDED","application":"demo-orders","stages":[]}`)
		case r.URL.Path == "/api/v1/tasks" || r.URL.Path == "/api/v1/pipelines" || r.URL.Path == "/api/v1/pipelines/p2":
			w.WriteHeader(202)
		case r.URL.Path == "/api/v1/applications/redir":
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
		default:
			http.Error(w, secretBody, 400)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g, err := NewHTTPGate(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	ok, desc, err := g.Application(ctx, "demo-orders")
	if err != nil || !ok || desc != AppDescription("demo", "orders") {
		t.Fatalf("Application = %v %q %v", ok, desc, err)
	}
	if gotAuth != "" {
		t.Error("credentials sent")
	}
	if ok, _, err := g.Application(ctx, "missing"); err != nil || ok {
		t.Errorf("missing = %v, %v", ok, err)
	}
	if id, err := g.PipelineID(ctx, "demo-orders", "deploy"); err != nil || id != "p2" {
		t.Errorf("PipelineID = %q, %v", id, err)
	}
	if err := g.CreateApplication(ctx, "demo-orders", "d"); err != nil || !strings.Contains(gotBody, `"createApplication"`) {
		t.Errorf("create: %v %s", err, gotBody)
	}
	if err := g.SavePipeline(ctx, "", []byte(`{"application":"a"}`)); err != nil || gotPath != "POST /api/v1/pipelines" {
		t.Errorf("create pipeline: %v %s", err, gotPath)
	}
	if err := g.SavePipeline(ctx, "p2", []byte(`{"application":"a"}`)); err != nil || gotPath != "PUT /api/v1/pipelines/p2" || !strings.Contains(gotBody, `"id":"p2"`) {
		t.Errorf("update pipeline: %v %s %s", err, gotPath, gotBody)
	}
	if id, err := g.Trigger(ctx, "demo-orders", "deploy", map[string]string{"ref": "abc"}); err != nil || id != "01EXEC" || !strings.Contains(gotBody, `"ref":"abc"`) {
		t.Errorf("Trigger = %q, %v, %s", id, err, gotBody)
	}
	if st, app, err := g.ExecutionStatus(ctx, "01EXEC"); err != nil || st != "SUCCEEDED" || app != "demo-orders" {
		t.Errorf("ExecutionStatus = %q %q, %v", st, app, err)
	}

	// Failures: status code and a fixed reason, never the body.
	_, err = g.Trigger(ctx, "nope", "deploy", nil)
	var ge *GateError
	if !errors.As(err, &ge) || ge.Status != 400 || !ge.Permanent() {
		t.Fatalf("err = %v", err)
	}
	for _, e := range []error{err, func() error { _, e := g.PipelineID(ctx, "boom", "deploy"); return e }()} {
		if e == nil || strings.Contains(e.Error(), "SECRET") || strings.Contains(Safe(e), "SECRET") {
			t.Errorf("error leaks the body or is nil: %v", e)
		}
	}
	if _, _, err := g.Application(ctx, "redir"); err == nil || !strings.Contains(err.Error(), "302") {
		t.Errorf("redirect followed or hidden: %v", err)
	}
}

func TestHTTPGateUnreachableAndConfig(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	g, err := NewHTTPGate(url)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = g.Application(context.Background(), "x")
	var ge *GateError
	if !errors.As(err, &ge) || ge.Status != 0 || ge.Permanent() || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("err = %v", err)
	}
	if got := Safe(errors.New("raw secret")); strings.Contains(got, "secret") {
		t.Errorf("Safe leaked: %s", got)
	}
	for _, bad := range []string{"", "ftp://x", "http://u:p@gate:8084", "http://gate:8084?x=1", "gate:8084", "http://"} {
		if _, err := NewHTTPGate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	g, _ = NewHTTPGate("http://gate.spinnaker.svc:8084/")
	if g.base != "http://gate.spinnaker.svc:8084/api/v1" {
		t.Errorf("base = %s", g.base)
	}
}
