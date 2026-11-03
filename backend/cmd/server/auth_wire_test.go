package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"regressionmap/internal/apikeys"
)

type wired struct {
	h      http.Handler
	logs   *bytes.Buffer
	admin  string
	ana    string
	webDir string
}

func newWired(t *testing.T, allowed []string, limit int) *wired {
	t.Helper()
	data := t.TempDir()
	s := apikeys.NewStore(data)
	_, admin, err := s.Create("admin", apikeys.ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, ana, _ := s.Create("ci", apikeys.ScopeAnalyze)
	web := t.TempDir()
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>spa</title>"), 0o600)
	var logs bytes.Buffer
	cfg := config{Root: t.TempDir(), DataDir: data, WebDir: web, ModelID: "muse-spark-1.3", AllowedHosts: allowed, RateLimitPerHour: limit}
	h, _, err := buildHandler(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &wired{h: h, logs: &logs, admin: admin, ana: ana, webDir: web}
}

func (w *wired) req(method, path, remote, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr, r.Host = remote, host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

const (
	pubHost = "regressionmap.glenox.net"
	pubPeer = "203.0.113.7:40000"
	locPeer = "127.0.0.1:40000"
)

func TestWiredRemoteAPINeedsKeyButUIDoesNot(t *testing.T) {
	w := newWired(t, []string{pubHost}, 0)

	// Public host is accepted by LocalOnly, then auth requires a key.
	rec := w.req("GET", "/api/repos", pubPeer, pubHost, nil, "")
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("remote without key: %d %v", rec.Code, rec.Header())
	}
	// Security headers wrap the 401 (SecurityHeaders is outside auth).
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("401 lacks security headers: %v", rec.Header())
	}
	rec = w.req("GET", "/api/repos", pubPeer, pubHost, map[string]string{"Authorization": "Bearer " + w.ana}, "")
	if rec.Code != 200 {
		t.Fatalf("remote with key: %d %q", rec.Code, rec.Body.String())
	}

	// The SPA is served without a key.
	for _, p := range []string{"/", "/analyses/a1"} {
		rec = w.req("GET", p, pubPeer, pubHost, nil, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>spa</title>") {
			t.Fatalf("UI %s: %d %q", p, rec.Code, rec.Body.String())
		}
	}

	// Health is public and says whether auth is required.
	var health map[string]any
	rec = w.req("GET", "/api/health", pubPeer, pubHost, nil, "")
	json.Unmarshal(rec.Body.Bytes(), &health)
	if rec.Code != 200 || health["auth_required"] != true || health["ok"] != true || health["model_id"] != "muse-spark-1.3" {
		t.Fatalf("remote health: %d %v", rec.Code, health)
	}
	rec = w.req("GET", "/api/health", locPeer, "127.0.0.1:8787", nil, "")
	json.Unmarshal(rec.Body.Bytes(), &health)
	if health["auth_required"] != false {
		t.Fatalf("local health: %v", health)
	}

	// Local callers need no key, including for key management.
	rec = w.req("GET", "/api/keys", locPeer, "localhost:8787", nil, "")
	if rec.Code != 200 {
		t.Fatalf("local keys: %d %q", rec.Code, rec.Body.String())
	}
	// Remote keys management: analyze is forbidden, admin is fine.
	if rec = w.req("GET", "/api/keys", pubPeer, pubHost, map[string]string{"Authorization": "Bearer " + w.ana}, ""); rec.Code != 403 {
		t.Fatalf("analyze on keys: %d", rec.Code)
	}
	if rec = w.req("GET", "/api/keys", pubPeer, pubHost, map[string]string{"Authorization": "Bearer " + w.admin}, ""); rec.Code != 200 {
		t.Fatalf("admin on keys: %d", rec.Code)
	}

	// Host checks still run first: an unknown host is 403 even with a key.
	rec = w.req("GET", "/api/repos", pubPeer, "evil.example", map[string]string{"Authorization": "Bearer " + w.admin}, "")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "forbidden_host") {
		t.Fatalf("evil host: %d %q", rec.Code, rec.Body.String())
	}
	// A browser POST from the public origin passes Origin checks.
	rec = w.req("POST", "/api/keys", pubPeer, pubHost, map[string]string{
		"Authorization": "Bearer " + w.admin, "Content-Type": "application/json",
		"Origin": "https://" + pubHost, "Sec-Fetch-Site": "same-origin",
	}, `{"name":"browser","scope":"analyze"}`)
	if rec.Code != 201 {
		t.Fatalf("public-origin POST: %d %q", rec.Code, rec.Body.String())
	}
	rec = w.req("POST", "/api/keys", pubPeer, pubHost, map[string]string{
		"Authorization": "Bearer " + w.admin, "Content-Type": "application/json", "Origin": "https://evil.example",
	}, `{"name":"x","scope":"analyze"}`)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "forbidden_origin") {
		t.Fatalf("evil-origin POST: %d %q", rec.Code, rec.Body.String())
	}
}

func TestWiredRateLimit(t *testing.T) {
	w := newWired(t, []string{pubHost}, 1)
	hdr := map[string]string{"Authorization": "Bearer " + w.ana, "Content-Type": "application/json"}
	// The first request reaches the API (and fails validation there).
	if rec := w.req("POST", "/api/analyses", pubPeer, pubHost, hdr, `{"repo":"nope"}`); rec.Code == 429 {
		t.Fatalf("first request limited: %q", rec.Body.String())
	}
	rec := w.req("POST", "/api/analyses", pubPeer, pubHost, hdr, `{"repo":"nope"}`)
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "rate_limited") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestAccessLogNeverContainsAuthorizationOrSecret(t *testing.T) {
	w := newWired(t, []string{pubHost}, 0)
	for _, secret := range []string{w.admin, w.ana, "rm_live_" + strings.Repeat("Q", 32)} {
		for _, p := range []string{"/api/repos", "/api/me", "/api/keys", "/api/health", "/"} {
			w.req("GET", p, pubPeer, pubHost, map[string]string{"Authorization": "Bearer " + secret}, "")
		}
	}
	w.req("POST", "/api/keys", locPeer, "127.0.0.1:8787", map[string]string{"Content-Type": "application/json"}, `{"name":"n","scope":"admin"}`)
	logs := w.logs.String()
	if strings.Count(logs, "\n") < 16 {
		t.Fatalf("expected one access log line per request, got: %s", logs)
	}
	for _, bad := range []string{w.admin, w.ana, w.admin[12:], "rm_live_", "Bearer", "Authorization", "authorization"} {
		if strings.Contains(logs, bad) {
			t.Fatalf("access log contains %q:\n%s", bad, logs)
		}
	}
}
