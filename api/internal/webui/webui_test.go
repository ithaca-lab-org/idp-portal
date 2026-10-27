package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testHandler() http.Handler {
	return handler(fstest.MapFS{
		"index.html":    {Data: []byte("<html>app</html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
		"favicon.svg":   {Data: []byte("<svg/>")},
	})
}

func get(path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	testHandler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestServesIndexWithSecurityHeaders(t *testing.T) {
	for _, p := range []string{"/", "/orders", "/some/deep/link"} {
		w := get(p)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "app") {
			t.Fatalf("%s: status %d body %q", p, w.Code, w.Body.String())
		}
		h := w.Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") ||
			strings.Contains(h.Get("Content-Security-Policy"), "unsafe-inline") ||
			h.Get("Cache-Control") != "no-cache" || h.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: headers %v", p, h)
		}
	}
}

func TestAssetsAreImmutableAndMissingAssetsAre404(t *testing.T) {
	w := get("/assets/app.js")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	if w := get("/assets/missing.js"); w.Code != http.StatusNotFound {
		t.Fatalf("missing asset: status %d", w.Code)
	}
}

func TestStaticFileAtRoot(t *testing.T) {
	if w := get("/favicon.svg"); w.Code != http.StatusOK || w.Body.String() != "<svg/>" {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
}

func TestAPIAndAuthPathsNeverReturnTheApp(t *testing.T) {
	for _, p := range []string{"/api/nope", "/auth/nope"} {
		if w := get(p); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "app") {
			t.Fatalf("%s: status %d", p, w.Code)
		}
	}
}

func TestEmbeddedDistHasIndex(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

func TestAssetDirectoriesAreNotListed(t *testing.T) {
	h := handler(fstest.MapFS{"index.html": {Data: []byte("app")}, "assets/sub/x.js": {Data: []byte("x")}})
	for _, p := range []string{"/assets/sub", "/assets/sub/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", p, w.Code)
		}
	}
}
