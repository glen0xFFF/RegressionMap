package main

import (
	"bytes"
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

func localPOST(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// WEBHOOK_SECRET reaches api.Config: with it notify_url is validated, without
// it any notify_url is refused. The access log never holds the URL.
func TestBuildHandlerWiresWebhookSecret(t *testing.T) {
	const hook = "https://hooks.example.com/services/T0/B0/PATHSECRET?token=QUERYSECRET"
	body := func(u string) string {
		return `{"repo":"nope","baseline":"HEAD","command_id":"node-test","notify_url":"` + u + `"}`
	}
	var logs bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logs, nil))

	on, _, err := buildHandler(config{Root: t.TempDir(), ModelID: "m", WebhookSecret: strings.Repeat("s", 32)}, lg)
	if err != nil {
		t.Fatal(err)
	}
	if rec := localPOST(on, "/api/analyses", body("http://hooks.example.com/x")); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_notify_url") {
		t.Fatalf("with secret, http URL: %d %s", rec.Code, rec.Body)
	}
	// A valid URL passes the notify checks and fails later, on the repo.
	if rec := localPOST(on, "/api/analyses", body(hook)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_repo") {
		t.Fatalf("with secret, valid URL: %d %s", rec.Code, rec.Body)
	}

	off, _, err := buildHandler(config{Root: t.TempDir(), ModelID: "m"}, lg)
	if err != nil {
		t.Fatal(err)
	}
	if rec := localPOST(off, "/api/analyses", body(hook)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "webhooks_disabled") {
		t.Fatalf("without secret: %d %s", rec.Code, rec.Body)
	}
	for _, leak := range []string{"PATHSECRET", "QUERYSECRET", "/services/"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("access log leaks %q: %s", leak, logs.String())
		}
	}
}
