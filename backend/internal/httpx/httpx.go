// Package httpx holds the HTTP middleware wrapped around the RegressionMap
// server: localhost-only access (DNS rebinding and CSRF protection),
// security headers, panic recovery and access logging.
//
// Recommended order (outermost first):
//
//	httpx.Chain(mux,
//		httpx.AccessLog(logger),
//		httpx.Recover(logger),
//		httpx.SecurityHeaders,
//		httpx.LocalOnlyFor(nil),
//	)
//
// so that rejected and recovered responses are logged with their final
// status and still carry the security headers.
package httpx

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with mws; the first middleware is the outermost, so
// Chain(h, a, b) is a(b(h)).
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// DefaultAllowedHosts are the loopback names accepted when no allow list is given.
var DefaultAllowedHosts = []string{"127.0.0.1", "localhost", "[::1]"}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// normHost lower-cases a host name and strips IPv6 brackets.
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// hostname extracts the host name from a Host header value ("host",
// "host:port", "[v6]" or "[v6]:port"). ok is false for malformed values.
func hostname(hostport string) (string, bool) {
	if hostport == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return normHost(h), h != ""
	}
	// No port. A bare IPv6 address must be bracketed in a Host header.
	if strings.HasPrefix(hostport, "[") {
		if !strings.HasSuffix(hostport, "]") {
			return "", false
		}
		return normHost(hostport), true
	}
	if strings.ContainsAny(hostport, ":[]") {
		return "", false
	}
	return normHost(hostport), true
}

// HostName is hostname for other packages: the lower-cased host name of a
// Host header value, without port or IPv6 brackets. ok is false when the
// value is malformed.
func HostName(hostport string) (string, bool) { return hostname(hostport) }

// WithDefaultHosts returns DefaultAllowedHosts followed by extra, as a new
// slice. LocalOnly replaces the defaults with a non-empty list, so a public
// deployment (ALLOWED_HOSTS) passes this to keep loopback access working.
// Origins are checked against the same list, for http and https.
func WithDefaultHosts(extra []string) []string {
	out := make([]string, 0, len(DefaultAllowedHosts)+len(extra))
	out = append(out, DefaultAllowedHosts...)
	return append(out, extra...)
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// LocalOnly rejects requests that do not come from a local browser tab or
// a local client:
//   - 403 forbidden_host when the Host header's name is not in allowedHosts
//     (nil or empty means DefaultAllowedHosts; any port is accepted). This
//     defeats DNS rebinding.
//   - for methods other than GET/HEAD/OPTIONS: 403 forbidden_origin when an
//     Origin header is present and is not http(s)://<allowed host>[:port],
//     or when Sec-Fetch-Site is present and is not same-origin or none.
//   - for POST: 415 unsupported_media_type unless Content-Type is
//     application/json, so a cross-site HTML form cannot post.
func LocalOnly(next http.Handler, allowedHosts []string) http.Handler {
	if len(allowedHosts) == 0 {
		allowedHosts = DefaultAllowedHosts
	}
	allowed := make(map[string]bool, len(allowedHosts))
	for _, h := range allowedHosts {
		allowed[normHost(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := hostname(r.Host); !ok || !allowed[h] {
			writeError(w, http.StatusForbidden, "forbidden_host", "host not allowed")
			return
		}
		if !safeMethod(r.Method) {
			if origin, present := r.Header["Origin"]; present && !originAllowed(origin, allowed) {
				writeError(w, http.StatusForbidden, "forbidden_origin", "cross-origin request not allowed")
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeError(w, http.StatusForbidden, "forbidden_origin", "cross-site request not allowed")
				return
			}
		}
		if r.Method == http.MethodPost {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(values []string, allowed map[string]bool) bool {
	if len(values) != 1 {
		return false
	}
	u, err := url.Parse(values[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	h, ok := hostname(u.Host)
	return ok && allowed[h]
}

// LocalOnlyFor is LocalOnly as a Middleware for Chain.
func LocalOnlyFor(allowedHosts []string) Middleware {
	return func(next http.Handler) http.Handler { return LocalOnly(next, allowedHosts) }
}

// ContentSecurityPolicy is the policy SecurityHeaders sets; it fits a built
// Vite React SPA served from the same origin as the API.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// SecurityHeaders sets nosniff, frame denial, no referrer and a strict CSP on
// every response, and Cache-Control: no-store on /api/ paths.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// recorder tracks status, byte count and whether the header was sent.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (rw *recorder) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.status = code
		// 1xx informational responses are not the final header.
		if code >= 200 {
			rw.wroteHeader = true
		}
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recorder) Write(p []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(p)
	rw.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and friends.
func (rw *recorder) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func wrap(w http.ResponseWriter) *recorder {
	if rw, ok := w.(*recorder); ok {
		return rw
	}
	return &recorder{ResponseWriter: w}
}

// Recover turns a handler panic into 500 {"error":{"code":"internal",...}}
// and logs the panic value and stack (never the request body or query).
// http.ErrAbortHandler is re-panicked so net/http can abort the connection.
// If the response header was already sent, only the log entry is written.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := wrap(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				if logger != nil {
					logger.Error("panic",
						"method", r.Method,
						"path", r.URL.Path,
						"panic", fmt.Sprint(v),
						"stack", string(debug.Stack()),
					)
				}
				if !rw.wroteHeader {
					writeError(rw, http.StatusInternalServerError, "internal", "internal error")
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// AccessLog writes one log record per request with method, path (no query
// string), status, duration_ms and bytes. Headers and bodies are never logged.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := wrap(w)
			defer func() {
				status := rw.status
				if status == 0 {
					status = http.StatusOK
				}
				if logger != nil {
					logger.Info("http",
						"method", r.Method,
						"path", r.URL.Path,
						"status", status,
						"duration_ms", time.Since(start).Milliseconds(),
						"bytes", rw.bytes,
					)
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}
