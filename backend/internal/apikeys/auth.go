package apikeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"regressionmap/internal/httpx"
)

// DefaultRateLimitPerHour is the per-key POST /api/analyses limit when
// Options.RateLimitPerHour is not positive.
const DefaultRateLimitPerHour = 20

// Identity is who made a request: a local caller, the public demo identity
// (Options.PublicDemo, a remote request without an Authorization header),
// or the holder of Key.
type Identity struct {
	Local  bool
	Public bool // the public demo identity: analyze scope, no key
	Key    Key  // zero when Local or Public
}

// Scope is the scope the identity acts with: analyze for the public demo
// identity, admin for a local caller, otherwise the key's scope ("" for the
// zero Identity). Public is checked first, so the public demo identity is
// never admin, whatever else the value holds.
func (id Identity) Scope() string {
	switch {
	case id.Public:
		return ScopeAnalyze
	case id.Local:
		return ScopeAdmin
	}
	return id.Key.Scope
}

// publicBucket is the one rate-limit bucket that every public demo request
// shares.
const publicBucket = "public"

// bucket is the rate-limit bucket of a remote identity. Key buckets are
// "key:" followed by the key id, so no key id, not even one written into
// keys.json by hand, can name the public bucket.
func (id Identity) bucket() string {
	if id.Public {
		return publicBucket
	}
	return "key:" + id.Key.ID
}

type ctxKey struct{}

// FromContext returns the identity the middleware attached to the request.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// loopbackHosts are the Host names a local request may use.
var loopbackHosts = map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}

// IsLocal implements the local-trust rule: the TCP peer is a loopback
// address, the Host header names 127.0.0.1, localhost or ::1, and the
// request carries no X-Forwarded-For or Forwarded header (a reverse proxy
// on the same machine makes the peer loopback, but the client is not).
func IsLocal(r *http.Request) bool {
	if _, ok := r.Header["X-Forwarded-For"]; ok {
		return false
	}
	if _, ok := r.Header["Forwarded"]; ok {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return false
	}
	name, ok := httpx.HostName(r.Host)
	return ok && loopbackHosts[name]
}

// Options configures Middleware.
type Options struct {
	Store            *Store // nil: no keys exist; only local requests reach /api
	RateLimitPerHour int    // per key on POST /api/analyses; <= 0 means DefaultRateLimitPerHour
	Now              func() time.Time

	// PublicDemo (PUBLIC_DEMO=1) serves a remote /api request that carries
	// no Authorization header as the public demo identity, with the analyze
	// scope. A request that carries one is checked as without PublicDemo.
	PublicDemo bool
}

type auth struct {
	next   http.Handler
	store  *Store
	lim    *limiter
	now    func() time.Time
	public bool
}

// Middleware enforces the v2 contract, and the v4 public demo mode, in
// front of the API:
//   - paths outside /api pass through without a key (the static UI);
//   - GET /api/health is public and gains "auth_required";
//   - other /api requests that are not local need a valid Bearer key, else
//     401 unauthorized with WWW-Authenticate: Bearer; with PublicDemo, one
//     without an Authorization header runs as the public demo identity;
//   - GET /api/me and /api/keys* are answered here (admin or local only for
//     /api/keys*, else 403 forbidden_scope);
//   - POST /api/analyses is rate limited per key, and in one shared bucket
//     for the public demo identity (429 rate_limited).
//
// The identity is attached to the request context (FromContext).
func Middleware(opts Options) func(http.Handler) http.Handler {
	limit := opts.RateLimitPerHour
	if limit <= 0 {
		limit = DefaultRateLimitPerHour
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	lim := newLimiter(limit, time.Hour)
	return func(next http.Handler) http.Handler {
		return &auth{next: next, store: opts.Store, lim: lim, now: now, public: opts.PublicDemo}
	}
}

func isAPI(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}

func (a *auth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isAPI(r.URL.Path) {
		a.next.ServeHTTP(w, r)
		return
	}
	local := IsLocal(r)
	if r.URL.Path == "/api/health" {
		switch r.Method {
		case http.MethodGet:
			a.health(w, r, local)
			return
		case http.MethodHead:
			a.next.ServeHTTP(w, r)
			return
		}
	}

	id := Identity{Local: true}
	switch {
	case local:
	case a.public && len(r.Header.Values("Authorization")) == 0:
		// No Authorization header: the public demo identity. A header that
		// is present, even empty or repeated, is checked as a key below.
		id = Identity{Public: true}
	default:
		k, err := a.authenticate(r)
		switch {
		case err == nil:
			id = Identity{Key: k}
		case errors.Is(err, ErrInvalidKey):
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required: Authorization: Bearer <key>")
			return
		default:
			fail(w, http.StatusServiceUnavailable, "keys_unavailable", "the API key store cannot be read")
			return
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))

	switch p := r.URL.Path; {
	case p == "/api/me":
		a.me(w, r, id)
		return
	case p == "/api/keys" || strings.HasPrefix(p, "/api/keys/"):
		a.keys(w, r, id)
		return
	case p == "/api/analyses" && r.Method == http.MethodPost && !id.Local:
		if ok, retry := a.lim.allow(id.bucket(), a.now()); !ok {
			secs := int((retry + time.Second - 1) / time.Second)
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			msg := "analysis rate limit for this key reached; retry later"
			if id.Public {
				msg = "analysis rate limit of the public demo reached; retry later"
			}
			fail(w, http.StatusTooManyRequests, "rate_limited", msg)
			return
		}
	}
	a.next.ServeHTTP(w, r)
}

// authenticate returns the key named by a single "Authorization: Bearer"
// header, or ErrInvalidKey.
func (a *auth) authenticate(r *http.Request) (Key, error) {
	vals := r.Header.Values("Authorization")
	if a.store == nil || len(vals) != 1 {
		return Key{}, ErrInvalidKey
	}
	scheme, token, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return Key{}, ErrInvalidKey
	}
	return a.store.Verify(strings.TrimSpace(token))
}

// bufferedWriter captures the inner health response so a field can be added.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header { return b.header }
func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// health runs the inner GET /api/health and adds "auth_required" (true when
// the caller is not local and PublicDemo is off). A non-200 or non-object
// response passes through unchanged.
func (a *auth) health(w http.ResponseWriter, r *http.Request, local bool) {
	bw := &bufferedWriter{header: http.Header{}}
	a.next.ServeHTTP(bw, r)
	if bw.status == 0 {
		bw.status = http.StatusOK
	}
	out := bw.body.Bytes()
	if bw.status == http.StatusOK {
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err == nil && m != nil {
			if _, err := dec.Token(); err == io.EOF {
				// With PublicDemo, a caller that is not local can use the
				// API without a key, so no caller is required to sign in.
				m["auth_required"] = !local && !a.public
				if b, err := json.Marshal(m); err == nil {
					out = append(b, '\n')
				}
			}
		}
	}
	h := w.Header()
	for k, v := range bw.header {
		h[k] = v
	}
	h.Del("Content-Length")
	w.WriteHeader(bw.status)
	w.Write(out)
}

type meKey struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Prefix string `json:"prefix"`
}

func (a *auth) me(w http.ResponseWriter, r *http.Request, id Identity) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	switch {
	case id.Public:
		writeJSON(w, http.StatusOK, map[string]any{"mode": "public"})
		return
	case id.Local:
		writeJSON(w, http.StatusOK, map[string]any{"mode": "local"})
		return
	}
	k := id.Key
	writeJSON(w, http.StatusOK, map[string]any{"mode": "key", "key": meKey{ID: k.ID, Name: k.Name, Scope: k.Scope, Prefix: k.Prefix}})
}

func (a *auth) keys(w http.ResponseWriter, r *http.Request, id Identity) {
	if id.Scope() != ScopeAdmin {
		msg := "this key's scope does not allow managing API keys"
		if id.Public {
			msg = "the public demo cannot manage API keys; use an admin key"
		}
		fail(w, http.StatusForbidden, "forbidden_scope", msg)
		return
	}
	if a.store == nil {
		fail(w, http.StatusServiceUnavailable, "keys_unavailable", "API keys need DATA_DIR to be set")
		return
	}
	if r.URL.Path == "/api/keys" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a.listKeys(w)
		case http.MethodPost:
			a.createKey(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET or POST")
		}
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", "use DELETE")
		return
	}
	keyID := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	if keyID == "" || strings.Contains(keyID, "/") {
		fail(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	if !id.Local && keyID == id.Key.ID {
		fail(w, http.StatusConflict, "cannot_revoke_self", "a key cannot revoke itself")
		return
	}
	switch err := a.store.Revoke(keyID); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", "key not found")
	default:
		fail(w, http.StatusInternalServerError, "keys_unavailable", "the API key store cannot be written")
	}
}

func (a *auth) listKeys(w http.ResponseWriter) {
	keys, err := a.store.List()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "keys_unavailable", "the API key store cannot be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (a *auth) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "body must be {\"name\": string, \"scope\": \"admin\"|\"analyze\"}")
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		fail(w, http.StatusBadRequest, "bad_request", "body must be a single JSON object")
		return
	}
	k, secret, err := a.store.Create(req.Name, req.Scope)
	switch {
	case errors.Is(err, ErrInvalidName), errors.Is(err, ErrInvalidScope):
		fail(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case err != nil:
		fail(w, http.StatusInternalServerError, "keys_unavailable", "the API key store cannot be written")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}
