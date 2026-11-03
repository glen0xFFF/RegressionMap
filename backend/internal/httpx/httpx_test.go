package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	})
}

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var b errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("error body is not JSON: %v; body=%q", err, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error Content-Type = %q, want application/json", ct)
	}
	if b.Error.Message == "" {
		t.Fatalf("error message empty; body=%q", rec.Body.String())
	}
	return b
}

func TestLocalOnly(t *testing.T) {
	const jsonCT = "application/json"
	tests := []struct {
		name     string
		method   string
		host     string
		headers  map[string]string
		allowed  []string
		wantCode int
		wantErr  string // "" means passes through to next
	}{
		{name: "GET 127.0.0.1 with port", method: "GET", host: "127.0.0.1:8787", wantCode: 200},
		{name: "GET 127.0.0.1 no port", method: "GET", host: "127.0.0.1", wantCode: 200},
		{name: "GET localhost vite port", method: "GET", host: "localhost:5173", wantCode: 200},
		{name: "GET localhost uppercase", method: "GET", host: "LOCALHOST:5173", wantCode: 200},
		{name: "GET ipv6 loopback with port", method: "GET", host: "[::1]:8787", wantCode: 200},
		{name: "GET ipv6 loopback no port", method: "GET", host: "[::1]", wantCode: 200},
		{name: "DNS rebinding evil host", method: "GET", host: "evil.example:8787", wantCode: 403, wantErr: "forbidden_host"},
		{name: "evil host with localhost prefix", method: "GET", host: "localhost.evil.example", wantCode: 403, wantErr: "forbidden_host"},
		{name: "evil host with 127 prefix", method: "GET", host: "127.0.0.1.nip.io:8787", wantCode: 403, wantErr: "forbidden_host"},
		{name: "empty host", method: "GET", host: "", wantCode: 403, wantErr: "forbidden_host"},
		{name: "LAN address host", method: "GET", host: "192.168.1.10:8787", wantCode: 403, wantErr: "forbidden_host"},
		{name: "evil host POST rejected as host first", method: "POST", host: "evil.example",
			headers: map[string]string{"Content-Type": jsonCT}, wantCode: 403, wantErr: "forbidden_host"},
		{name: "GET from evil origin allowed when host ok", method: "GET", host: "127.0.0.1:8787",
			headers: map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}, wantCode: 200},
		{name: "HEAD from evil origin allowed when host ok", method: "HEAD", host: "127.0.0.1:8787",
			headers: map[string]string{"Origin": "http://evil.example"}, wantCode: 200},
		{name: "OPTIONS from evil origin allowed when host ok", method: "OPTIONS", host: "localhost:8787",
			headers: map[string]string{"Origin": "http://evil.example"}, wantCode: 200},
		{name: "GET from evil origin with evil host rejected", method: "GET", host: "evil.example",
			headers: map[string]string{"Origin": "http://evil.example"}, wantCode: 403, wantErr: "forbidden_host"},
		{name: "POST same-origin json", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "http://127.0.0.1:8787", "Sec-Fetch-Site": "same-origin"}, wantCode: 200},
		{name: "POST via vite proxy origin", method: "POST", host: "localhost:5173",
			headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Origin": "http://localhost:5173", "Sec-Fetch-Site": "same-origin"}, wantCode: 200},
		{name: "POST https localhost origin", method: "POST", host: "localhost:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "https://localhost"}, wantCode: 200},
		{name: "POST ipv6 origin", method: "POST", host: "[::1]:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "http://[::1]:8787"}, wantCode: 200},
		{name: "POST no origin no fetch-site (curl)", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT}, wantCode: 200},
		{name: "POST sec-fetch-site none", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Sec-Fetch-Site": "none"}, wantCode: 200},
		{name: "cross-site origin POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "http://evil.example"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "origin with localhost suffix trick", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "http://localhost.evil.example"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "null origin POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "null"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "non-http origin scheme", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "file://localhost"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "cross-site origin DELETE", method: "DELETE", host: "127.0.0.1:8787",
			headers: map[string]string{"Origin": "https://evil.example"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "sec-fetch-site cross-site POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Sec-Fetch-Site": "cross-site"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "sec-fetch-site same-site POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": jsonCT, "Origin": "http://127.0.0.1:8787", "Sec-Fetch-Site": "same-site"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "sec-fetch-site cross-site PUT", method: "PUT", host: "127.0.0.1:8787",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, wantCode: 403, wantErr: "forbidden_origin"},
		{name: "form-encoded POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, wantCode: 415, wantErr: "unsupported_media_type"},
		{name: "multipart POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, wantCode: 415, wantErr: "unsupported_media_type"},
		{name: "text/plain POST", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": "text/plain"}, wantCode: 415, wantErr: "unsupported_media_type"},
		{name: "POST without content-type", method: "POST", host: "127.0.0.1:8787", wantCode: 415, wantErr: "unsupported_media_type"},
		{name: "POST json lookalike", method: "POST", host: "127.0.0.1:8787",
			headers: map[string]string{"Content-Type": "application/jsonp"}, wantCode: 415, wantErr: "unsupported_media_type"},
		{name: "custom allow list accepts its host", method: "GET", host: "regressionmap.test:8787",
			allowed: []string{"regressionmap.test"}, wantCode: 200},
		{name: "custom allow list replaces defaults", method: "GET", host: "localhost:8787",
			allowed: []string{"regressionmap.test"}, wantCode: 403, wantErr: "forbidden_host"},
		{name: "custom allow list bare ipv6", method: "GET", host: "[::1]:8787",
			allowed: []string{"::1"}, wantCode: 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			h := LocalOnly(next, tc.allowed)
			req := httptest.NewRequest(tc.method, "/api/analyses", strings.NewReader(`{}`))
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantErr == "" {
				if !called {
					t.Fatal("next handler not called")
				}
				return
			}
			if called {
				t.Fatal("next handler called for a rejected request")
			}
			if got := decodeErr(t, rec).Error.Code; got != tc.wantErr {
				t.Fatalf("error code = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func TestLocalOnlyForMiddleware(t *testing.T) {
	h := Chain(okHandler(), LocalOnlyFor(nil))
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	tests := []struct {
		name        string
		path        string
		wantNoStore bool
	}{
		{name: "api path", path: "/api/health", wantNoStore: true},
		{name: "api nested path", path: "/api/analyses/a1", wantNoStore: true},
		{name: "spa root", path: "/", wantNoStore: false},
		{name: "spa asset", path: "/assets/index-abc.js", wantNoStore: false},
		{name: "api lookalike", path: "/apiary", wantNoStore: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			SecurityHeaders(okHandler()).ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
			want := map[string]string{
				"X-Content-Type-Options":  "nosniff",
				"X-Frame-Options":         "DENY",
				"Referrer-Policy":         "no-referrer",
				"Content-Security-Policy": wantCSP,
			}
			for k, v := range want {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			cc := rec.Header().Get("Cache-Control")
			if tc.wantNoStore && cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if !tc.wantNoStore && cc == "no-store" {
				t.Errorf("Cache-Control = no-store on non-API path %s", tc.path)
			}
		})
	}
}

func TestSecurityHeadersOnRejectedRequest(t *testing.T) {
	// SecurityHeaders outside LocalOnly: a 403 still carries the headers.
	h := Chain(okHandler(), SecurityHeaders, LocalOnlyFor(nil))
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing security headers on 403: %v", rec.Header())
	}
}

func newBufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestRecover(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "panic string", handler: func(w http.ResponseWriter, r *http.Request) { panic("boom") }},
		{name: "panic error", handler: func(w http.ResponseWriter, r *http.Request) { panic(io.ErrUnexpectedEOF) }},
		{name: "panic after setting headers", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Length", "999")
			panic("boom")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newBufLogger()
			h := Recover(logger)(tc.handler)
			req := httptest.NewRequest("POST", "/api/analyses?token=querysecret", strings.NewReader(`{"secret":"bodysecret"}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			b := decodeErr(t, rec)
			if b.Error.Code != "internal" || b.Error.Message != "internal error" {
				t.Fatalf("error = %+v, want internal/internal error", b.Error)
			}
			if rec.Header().Get("Content-Length") == "999" {
				t.Fatal("stale Content-Length kept on 500")
			}
			logs := buf.String()
			if !strings.Contains(logs, "panic") {
				t.Fatalf("panic not logged: %q", logs)
			}
			if strings.Contains(logs, "bodysecret") || strings.Contains(logs, "querysecret") {
				t.Fatalf("log contains request body or query: %q", logs)
			}
		})
	}
}

func TestRecoverAfterWriteDoesNotRewrite(t *testing.T) {
	logger, buf := newBufLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "partial")
		panic("late boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Fatalf("got %d %q, want untouched 202 partial", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), "late boom") {
		t.Fatalf("late panic not logged: %q", buf.String())
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	logger, _ := newBufLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler re-panicked", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestRecoverNoPanicPassThrough(t *testing.T) {
	logger, buf := newBufLogger()
	rec := httptest.NewRecorder()
	Recover(logger)(okHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Code != 200 || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected log: %q", buf.String())
	}
}

func TestAccessLog(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus float64
		wantBytes  float64
	}{
		{name: "explicit status", handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, "hello")
		}, wantStatus: 202, wantBytes: 5},
		{name: "implicit 200", handler: func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "abc")
			io.WriteString(w, "de")
		}, wantStatus: 200, wantBytes: 5},
		{name: "no write at all", handler: func(w http.ResponseWriter, r *http.Request) {}, wantStatus: 200, wantBytes: 0},
		{name: "error status", handler: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		}, wantStatus: 404, wantBytes: 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newBufLogger()
			h := AccessLog(logger)(tc.handler)
			req := httptest.NewRequest("POST", "/api/analyses?api_key=querysecret", strings.NewReader(`{"k":"bodysecret"}`))
			req.Header.Set("Authorization", "Bearer sk-authsecret-123")
			req.Header.Set("Cookie", "session=cookiesecret")
			req.Header.Set("X-Api-Key", "headersecret")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			logs := buf.String()
			for _, secret := range []string{"sk-authsecret-123", "Bearer", "cookiesecret", "headersecret", "querysecret", "bodysecret", "api_key"} {
				if strings.Contains(logs, secret) {
					t.Fatalf("access log leaks %q: %s", secret, logs)
				}
			}
			lines := strings.Split(strings.TrimSpace(logs), "\n")
			if len(lines) != 1 {
				t.Fatalf("want exactly one log line, got %d: %q", len(lines), logs)
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
				t.Fatalf("bad log JSON: %v", err)
			}
			if entry["method"] != "POST" || entry["path"] != "/api/analyses" {
				t.Fatalf("method/path = %v %v", entry["method"], entry["path"])
			}
			if entry["status"] != tc.wantStatus {
				t.Fatalf("status = %v, want %v", entry["status"], tc.wantStatus)
			}
			if entry["bytes"] != tc.wantBytes {
				t.Fatalf("bytes = %v, want %v", entry["bytes"], tc.wantBytes)
			}
			if d, ok := entry["duration_ms"].(float64); !ok || d < 0 {
				t.Fatalf("duration_ms = %v", entry["duration_ms"])
			}
		})
	}
}

func TestAccessLogKeepsFlusher(t *testing.T) {
	logger, _ := newBufLogger()
	var flushed bool
	h := AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err == nil {
			flushed = true
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if !flushed || !rec.Flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { order = append(order, "h") }), mw("a"), mw("b"), mw("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got := strings.Join(order, ","); got != "a,b,c,h" {
		t.Fatalf("order = %s, want a,b,c,h", got)
	}
	if Chain(okHandler()) == nil {
		t.Fatal("Chain with no middleware returned nil")
	}
}

func TestFullChainPanicLoggedAs500(t *testing.T) {
	logger, buf := newBufLogger()
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := Chain(panicky, AccessLog(logger), Recover(logger), SecurityHeaders, LocalOnlyFor(nil))
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Authorization", "Bearer sk-authsecret-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing on recovered 500")
	}
	logs := buf.String()
	if !strings.Contains(logs, `"status":500`) {
		t.Fatalf("access log did not record 500: %s", logs)
	}
	if strings.Contains(logs, "sk-authsecret-123") {
		t.Fatalf("log leaks Authorization: %s", logs)
	}
}

// A public deployment passes defaults + ALLOWED_HOSTS; the browser then
// sends Host and Origin with the public name over https.
func TestLocalOnlyPublicHost(t *testing.T) {
	allowed := WithDefaultHosts([]string{"regressionmap.glenox.net"})
	tests := []struct {
		name     string
		method   string
		host     string
		origin   string
		site     string
		wantCode int
		wantErr  string
	}{
		{name: "public same-origin POST", method: "POST", host: "regressionmap.glenox.net",
			origin: "https://regressionmap.glenox.net", site: "same-origin", wantCode: 200},
		{name: "public host with port", method: "POST", host: "regressionmap.glenox.net:443",
			origin: "https://regressionmap.glenox.net:443", site: "same-origin", wantCode: 200},
		{name: "public host upper case", method: "GET", host: "RegressionMap.Glenox.NET", wantCode: 200},
		{name: "defaults still accepted", method: "POST", host: "127.0.0.1:8787",
			origin: "http://127.0.0.1:8787", site: "same-origin", wantCode: 200},
		{name: "evil host", method: "POST", host: "evil.example",
			origin: "https://evil.example", site: "same-origin", wantCode: 403, wantErr: "forbidden_host"},
		{name: "evil origin to public host", method: "POST", host: "regressionmap.glenox.net",
			origin: "https://evil.example", site: "cross-site", wantCode: 403, wantErr: "forbidden_origin"},
		{name: "evil origin without fetch-site", method: "POST", host: "regressionmap.glenox.net",
			origin: "https://evil.example", wantCode: 403, wantErr: "forbidden_origin"},
		{name: "suffix trick origin", method: "POST", host: "regressionmap.glenox.net",
			origin: "https://regressionmap.glenox.net.evil.example", site: "same-origin", wantCode: 403, wantErr: "forbidden_origin"},
		{name: "subdomain host not allowed", method: "GET", host: "x.regressionmap.glenox.net", wantCode: 403, wantErr: "forbidden_host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := LocalOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), allowed)
			req := httptest.NewRequest(tc.method, "/api/analyses", strings.NewReader(`{}`))
			req.Host = tc.host
			req.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantErr == "" {
				if !called {
					t.Fatal("next not called")
				}
				return
			}
			if called {
				t.Fatal("next called for rejected request")
			}
			if got := decodeErr(t, rec).Error.Code; got != tc.wantErr {
				t.Fatalf("code = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

func TestWithDefaultHosts(t *testing.T) {
	got := WithDefaultHosts([]string{"a.example", "B.Example"})
	want := []string{"127.0.0.1", "localhost", "[::1]", "a.example", "B.Example"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
	if strings.Join(WithDefaultHosts(nil), ",") != strings.Join(DefaultAllowedHosts, ",") {
		t.Fatal("nil extras must give the defaults")
	}
	// The result must not alias DefaultAllowedHosts.
	got[0] = "changed"
	if DefaultAllowedHosts[0] != "127.0.0.1" {
		t.Fatal("WithDefaultHosts mutated DefaultAllowedHosts")
	}
}

func TestHostName(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8787": "127.0.0.1", "LOCALHOST": "localhost", "[::1]:80": "::1", "[::1]": "::1",
		"regressionmap.glenox.net": "regressionmap.glenox.net",
	}
	for in, want := range cases {
		if got, ok := HostName(in); !ok || got != want {
			t.Errorf("HostName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "::1", "[::1", "a:b:c"} {
		if _, ok := HostName(bad); ok {
			t.Errorf("HostName(%q) must fail", bad)
		}
	}
}
