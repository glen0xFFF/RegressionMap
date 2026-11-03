package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildHandlerAppliesSecurityChain(t *testing.T) {
	cfg := config{Root: t.TempDir(), DataDir: t.TempDir(), ModelID: "muse-spark-1.3"}
	h, _, err := buildHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health: %d %v", rec.Code, rec.Header())
	}

	req = httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "evil.example:8787"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden_host") {
		t.Fatalf("rebinding host: %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/analyses", strings.NewReader("repo=x"))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form csrf: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/analyses", nil)
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:50000" // v2: only a loopback peer is local (httptest defaults to 192.0.2.1)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list endpoint: %d %s", rec.Code, rec.Body)
	}
}
