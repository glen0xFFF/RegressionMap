package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const indexHTML = "<!doctype html><title>spa-index</title>"

// webFixture lays out:
//
//	<tmp>/secret.txt          (outside WEB_DIR; must never be served)
//	<tmp>/dist/index.html
//	<tmp>/dist/assets/app.js
func webFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	dist := filepath.Join(base, "dist")
	os.MkdirAll(filepath.Join(dist, "assets"), 0o755)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("TOP-SECRET"), 0o600)
	os.WriteFile(filepath.Join(dist, "index.html"), []byte(indexHTML), 0o600)
	os.WriteFile(filepath.Join(dist, "assets", "app.js"), []byte("console.log('app')"), 0o600)
	return dist
}

func apiStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-From", "api")
		io.WriteString(w, "api:"+r.URL.Path)
	})
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", nil)
	// Set the raw path directly so httptest does not normalise traversal attempts away.
	req.URL.Path = target
	req.RequestURI = target
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewHandlerWithoutWebDirIsAPIOnly(t *testing.T) {
	h, err := newHandler(apiStub(), "")
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "GET", "/anything")
	if rec.Header().Get("X-From") != "api" {
		t.Fatalf("without WEB_DIR every request goes to the API handler, got %q", rec.Body.String())
	}
}

func TestNewHandlerRoutesAPI(t *testing.T) {
	h, err := newHandler(apiStub(), webFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/health", "/api/repos/x/commits", "/api"} {
		rec := do(t, h, "GET", p)
		if rec.Header().Get("X-From") != "api" || rec.Body.String() != "api:"+p {
			t.Errorf("%s: want API handler, got %d %q", p, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, h, "POST", "/api/analyses"); rec.Header().Get("X-From") != "api" {
		t.Errorf("POST /api must reach API, got %q", rec.Body.String())
	}
	// "/apix" is not the API namespace.
	if rec := do(t, h, "GET", "/apix"); rec.Header().Get("X-From") == "api" {
		t.Errorf("/apix must not reach the API")
	}
}

func TestNewHandlerServesFilesAndSPAFallback(t *testing.T) {
	h, err := newHandler(apiStub(), webFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, "GET", "/assets/app.js")
	if rec.Code != 200 || rec.Body.String() != "console.log('app')" {
		t.Fatalf("asset: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset content type %q", ct)
	}
	for _, p := range []string{"/", "/index.html", "/analyses/a1", "/deep/link/x", "/assets", "/assets/missing.js"} {
		rec := do(t, h, "GET", p)
		if rec.Code != 200 || rec.Body.String() != indexHTML {
			t.Errorf("%s: want SPA index, got %d %q", p, rec.Code, rec.Body.String())
		}
	}
	if rec := do(t, h, "HEAD", "/somewhere"); rec.Code != 200 {
		t.Errorf("HEAD fallback: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/somewhere"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST outside /api must be 405, got %d", rec.Code)
	}
}

func TestNewHandlerNeverServesOutsideWebDir(t *testing.T) {
	dist := webFixture(t)
	h, err := newHandler(apiStub(), dist)
	if err != nil {
		t.Fatal(err)
	}
	attempts := []string{
		"/../secret.txt",
		"/assets/../../secret.txt",
		"/..%2fsecret.txt",
		`/..\secret.txt`,
		`/assets\..\..\secret.txt`,
		"/" + filepath.ToSlash(filepath.Join(filepath.Dir(dist), "secret.txt")),
		"//../secret.txt",
	}
	for _, p := range attempts {
		rec := do(t, h, "GET", p)
		if strings.Contains(rec.Body.String(), "TOP-SECRET") {
			t.Fatalf("%s: served a file outside WEB_DIR", p)
		}
	}

	// A symlink inside WEB_DIR that points outside must not be followed.
	link := filepath.Join(dist, "escape.txt")
	if err := os.Symlink(filepath.Join(filepath.Dir(dist), "secret.txt"), link); err != nil {
		t.Logf("symlink not supported here (%v); skipping symlink escape check", err)
		return
	}
	if rec := do(t, h, "GET", "/escape.txt"); strings.Contains(rec.Body.String(), "TOP-SECRET") {
		t.Fatal("followed a symlink out of WEB_DIR")
	}
}

func TestNewHandlerRejectsBadWebDir(t *testing.T) {
	if _, err := newHandler(apiStub(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing WEB_DIR must error")
	}
}
