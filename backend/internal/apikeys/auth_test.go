package apikeys

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	localPeer  = "127.0.0.1:50000"
	remotePeer = "203.0.113.7:50000"
	localHost  = "127.0.0.1:8787"
	publicHost = "regressionmap.glenox.net"
)

func TestIsLocal(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		host    string
		headers map[string]string
		want    bool
	}{
		{"loopback v4", "127.0.0.1:1234", "127.0.0.1:8787", nil, true},
		{"loopback v4 other 127", "127.0.0.9:1234", "localhost", nil, true},
		{"loopback v6", "[::1]:1234", "[::1]:8787", nil, true},
		{"v4-mapped loopback", "[::ffff:127.0.0.1]:1234", "localhost:5173", nil, true},
		{"localhost upper", "127.0.0.1:1", "LOCALHOST:8787", nil, true},
		{"remote peer", "203.0.113.7:1234", "127.0.0.1:8787", nil, false},
		{"LAN peer", "192.168.1.2:1234", "localhost", nil, false},
		{"public host via local proxy", "127.0.0.1:1234", "regressionmap.glenox.net", nil, false},
		{"other loopback ip as host", "127.0.0.1:1234", "127.0.0.2:8787", nil, false},
		{"x-forwarded-for", "127.0.0.1:1234", "127.0.0.1:8787", map[string]string{"X-Forwarded-For": "203.0.113.7"}, false},
		{"empty x-forwarded-for still counts", "127.0.0.1:1234", "127.0.0.1:8787", map[string]string{"X-Forwarded-For": ""}, false},
		{"forwarded", "127.0.0.1:1234", "localhost", map[string]string{"Forwarded": "for=203.0.113.7"}, false},
		{"garbage remote addr", "garbage", "localhost", nil, false},
		{"empty remote addr", "", "localhost", nil, false},
		{"empty host", "127.0.0.1:1", "", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/repos", nil)
			r.RemoteAddr = tc.remote
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header[http.CanonicalHeaderKey(k)] = []string{v}
			}
			if got := IsLocal(r); got != tc.want {
				t.Fatalf("IsLocal = %v, want %v", got, tc.want)
			}
		})
	}
}

// fixture is the auth middleware around a stub API that records the
// identity it was given.
type fixture struct {
	t       *testing.T
	store   *Store
	clock   *fakeClock
	h       http.Handler
	lastID  Identity
	reached int
	admin   string // secret
	adminK  Key
	analyze string
	anaK    Key
}

func newFixture(t *testing.T, limit int) *fixture {
	t.Helper()
	return newFixtureWith(t, limit, false)
}

// newFixtureWith is newFixture with PublicDemo set to public.
func newFixtureWith(t *testing.T, limit int, public bool) *fixture {
	t.Helper()
	f := &fixture{t: t, store: NewStore(t.TempDir()), clock: newFakeClock()}
	f.store.now = f.clock.Now
	var err error
	if f.adminK, f.admin, err = f.store.Create("admin", ScopeAdmin); err != nil {
		t.Fatal(err)
	}
	if f.anaK, f.analyze, err = f.store.Create("ci", ScopeAnalyze); err != nil {
		t.Fatal(err)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reached++
		f.lastID, _ = FromContext(r.Context())
		if r.URL.Path == "/api/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Inner", "yes")
			io.WriteString(w, `{"ok":true,"model_configured":false,"model_id":"muse-spark-1.3","n":12345678901234567}`+"\n")
			return
		}
		w.Header().Set("X-Inner", "yes")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "inner:"+r.URL.Path)
	})
	f.h = Middleware(Options{Store: f.store, RateLimitPerHour: limit, Now: f.clock.Now, PublicDemo: public})(inner)
	return f
}

func (f *fixture) do(method, path string, local bool, secret, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if local {
		r.RemoteAddr, r.Host = localPeer, localHost
	} else {
		r.RemoteAddr, r.Host = remotePeer, publicHost
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("error body not JSON: %q", rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || b.Error.Message == "" {
		t.Fatalf("bad error response: %v %q", rec.Header(), rec.Body.String())
	}
	return b.Error.Code
}

func TestRemoteAPIRequiresKey(t *testing.T) {
	f := newFixture(t, 20)
	revokedK, revoked, _ := f.store.Create("old", ScopeAdmin)
	f.store.Revoke(revokedK.ID)

	for name, auth := range map[string]string{
		"missing":   "",
		"malformed": "Bearer",
		"wrong":     "Bearer rm_live_" + strings.Repeat("Z", 32),
		"revoked":   "Bearer " + revoked,
		"basic":     "Basic " + f.admin,
		"no scheme": f.admin,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/repos", nil)
			r.RemoteAddr, r.Host = remotePeer, publicHost
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			before := f.reached
			f.h.ServeHTTP(rec, r)
			if rec.Code != 401 || errCode(t, rec) != "unauthorized" {
				t.Fatalf("got %d %q", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
			if f.reached != before {
				t.Fatal("unauthorized request reached the API")
			}
			if strings.Contains(rec.Body.String(), f.admin) {
				t.Fatal("secret echoed in response")
			}
		})
	}

	// Scheme is case-insensitive.
	r := httptest.NewRequest("GET", "/api/repos", nil)
	r.RemoteAddr, r.Host = remotePeer, publicHost
	r.Header.Set("Authorization", "bearer "+f.analyze)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("lower-case bearer: %d", rec.Code)
	}

	// A forwarded request from a local proxy is not local.
	r = httptest.NewRequest("GET", "/api/repos", nil)
	r.RemoteAddr, r.Host = localPeer, localHost
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("forwarded request without key: %d", rec.Code)
	}
}

func TestValidKeyReachesAPIWithIdentity(t *testing.T) {
	f := newFixture(t, 20)
	rec := f.do("GET", "/api/analyses/a1", false, f.analyze, "")
	if rec.Code != http.StatusAccepted || rec.Body.String() != "inner:/api/analyses/a1" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if f.lastID.Local || f.lastID.Key.ID != f.anaK.ID || f.lastID.Key.Scope != ScopeAnalyze {
		t.Fatalf("identity = %+v", f.lastID)
	}

	rec = f.do("GET", "/api/repos", true, "", "")
	if rec.Code != http.StatusAccepted || !f.lastID.Local || f.lastID.Key.ID != "" {
		t.Fatalf("local: %d identity %+v", rec.Code, f.lastID)
	}
}

func TestNonAPIPathsNeedNoKey(t *testing.T) {
	f := newFixture(t, 20)
	for _, p := range []string{"/", "/index.html", "/assets/app.js", "/analyses/a1", "/apix"} {
		rec := f.do("GET", p, false, "", "")
		if rec.Code != http.StatusAccepted || rec.Body.String() != "inner:"+p {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do("GET", "/api", false, "", ""); rec.Code != 401 {
		t.Errorf("/api itself must be protected: %d", rec.Code)
	}
}

func TestHealthIsPublicAndReportsAuthRequired(t *testing.T) {
	f := newFixture(t, 20)
	for _, tc := range []struct {
		local  bool
		secret string
		want   bool
	}{{true, "", false}, {false, "", true}, {false, "rm_live_bogus", true}, {false, f.admin, true}} {
		rec := f.do("GET", "/api/health", tc.local, tc.secret, "")
		if rec.Code != 200 {
			t.Fatalf("local=%v: %d %q", tc.local, rec.Code, rec.Body.String())
		}
		dec := json.NewDecoder(rec.Body)
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m["auth_required"] != tc.want {
			t.Fatalf("local=%v: auth_required = %v, want %v (%v)", tc.local, m["auth_required"], tc.want, m)
		}
		if m["ok"] != true || m["model_id"] != "muse-spark-1.3" || m["model_configured"] != false {
			t.Fatalf("inner fields lost: %v", m)
		}
		if n, _ := m["n"].(json.Number); n.String() != "12345678901234567" {
			t.Fatalf("number precision lost: %v", m["n"])
		}
		if rec.Header().Get("X-Inner") != "yes" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("inner headers lost: %v", rec.Header())
		}
		if cl := rec.Header().Get("Content-Length"); cl != "" && cl != strconv.Itoa(rec.Body.Len()+0) && cl != strconv.Itoa(len(rec.Body.Bytes())) {
			t.Fatalf("stale Content-Length %s", cl)
		}
	}
	// Other methods on /api/health are not public.
	if rec := f.do("POST", "/api/health", false, "", `{}`); rec.Code != 401 {
		t.Fatalf("POST /api/health remote without key: %d", rec.Code)
	}
}

func TestHealthPassesThroughNonJSON(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, "down")
	})
	h := Middleware(Options{})(inner)
	r := httptest.NewRequest("GET", "/api/health", nil)
	r.RemoteAddr, r.Host = remotePeer, publicHost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 503 || rec.Body.String() != "down" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestMe(t *testing.T) {
	f := newFixture(t, 20)
	rec := f.do("GET", "/api/me", true, "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"mode":"local"}` {
		t.Fatalf("local me: %d %q", rec.Code, rec.Body.String())
	}
	rec = f.do("GET", "/api/me", false, f.analyze, "")
	var me struct {
		Mode string                     `json:"mode"`
		Key  map[string]json.RawMessage `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || rec.Code != 200 {
		t.Fatalf("key me: %d %q", rec.Code, rec.Body.String())
	}
	if me.Mode != "key" || string(me.Key["id"]) != `"`+f.anaK.ID+`"` || string(me.Key["scope"]) != `"analyze"` ||
		string(me.Key["name"]) != `"ci"` || string(me.Key["prefix"]) != `"`+f.analyze[:12]+`"` || len(me.Key) != 4 {
		t.Fatalf("me = %+v", me)
	}
	if rec := f.do("GET", "/api/me", false, "", ""); rec.Code != 401 {
		t.Fatalf("remote me without key: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/me", true, "", `{}`); rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST me: %d %v", rec.Code, rec.Header())
	}
}

func TestKeysRoutesScope(t *testing.T) {
	f := newFixture(t, 20)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/keys", ""},
		{"POST", "/api/keys", `{"name":"x","scope":"admin"}`},
		{"DELETE", "/api/keys/" + f.adminK.ID, ""},
		{"DELETE", "/api/keys/" + f.anaK.ID, ""},
		{"PUT", "/api/keys", `{}`},
		{"GET", "/api/keys/", ""},
	} {
		rec := f.do(tc.method, tc.path, false, f.analyze, tc.body)
		if rec.Code != 403 || errCode(t, rec) != "forbidden_scope" {
			t.Errorf("%s %s with analyze key: %d %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if keys, _ := f.store.List(); len(keys) != 2 || keys[0].Revoked || keys[1].Revoked {
		t.Fatalf("analyze key changed the store: %+v", keys)
	}
	if f.reached != 0 {
		t.Fatal("keys routes must not reach the API")
	}
}

func TestKeysCRUDAsAdminAndLocal(t *testing.T) {
	for _, mode := range []string{"admin", "local"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, 20)
			local, secret := mode == "local", ""
			if !local {
				secret = f.admin
			}
			rec := f.do("POST", "/api/keys", local, secret, `{"name":"deploy","scope":"analyze"}`)
			if rec.Code != 201 {
				t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
			}
			var created struct {
				Key    Key    `json:"key"`
				Secret string `json:"secret"`
			}
			json.Unmarshal(rec.Body.Bytes(), &created)
			if !secretRe.MatchString(created.Secret) || created.Key.Name != "deploy" || created.Key.Prefix != created.Secret[:12] {
				t.Fatalf("create body: %q", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "sha256") {
				t.Fatal("hash leaked in response")
			}
			// The new key works immediately.
			if rec := f.do("GET", "/api/repos", false, created.Secret, ""); rec.Code != http.StatusAccepted {
				t.Fatalf("new key rejected: %d", rec.Code)
			}

			rec = f.do("GET", "/api/keys", local, secret, "")
			var list struct {
				Keys []map[string]json.RawMessage `json:"keys"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != 200 || len(list.Keys) != 3 {
				t.Fatalf("list: %d %q", rec.Code, rec.Body.String())
			}
			for _, k := range list.Keys {
				for _, field := range []string{"id", "name", "scope", "prefix", "created_at", "last_used_at", "revoked"} {
					if _, ok := k[field]; !ok {
						t.Fatalf("list item lacks %s: %v", field, k)
					}
				}
				if len(k) != 7 {
					t.Fatalf("list item has extra fields: %v", k)
				}
			}
			if strings.Contains(rec.Body.String(), "sha256") {
				t.Fatal("hash leaked in list")
			}

			rec = f.do("DELETE", "/api/keys/"+created.Key.ID, local, secret, "")
			if rec.Code != 204 || rec.Body.Len() != 0 {
				t.Fatalf("delete: %d %q", rec.Code, rec.Body.String())
			}
			if rec := f.do("GET", "/api/repos", false, created.Secret, ""); rec.Code != 401 {
				t.Fatalf("revoked key still works: %d", rec.Code)
			}
			rec = f.do("DELETE", "/api/keys/k_doesnotexi", local, secret, "")
			if rec.Code != 404 || errCode(t, rec) != "not_found" {
				t.Fatalf("delete unknown: %d %q", rec.Code, rec.Body.String())
			}
			for _, body := range []string{`{"name":"","scope":"analyze"}`, `{"name":"x","scope":"root"}`, `not json`, `{"name":"x","scope":"admin","extra":1}`, `{"name":"x","scope":"admin"} {}`} {
				rec = f.do("POST", "/api/keys", local, secret, body)
				if rec.Code != 400 || errCode(t, rec) != "bad_request" {
					t.Fatalf("create %s: %d %q", body, rec.Code, rec.Body.String())
				}
			}
			if rec := f.do("PATCH", "/api/keys", local, secret, ""); rec.Code != 405 {
				t.Fatalf("PATCH /api/keys: %d", rec.Code)
			}
			if rec := f.do("GET", "/api/keys/"+f.anaK.ID, local, secret, ""); rec.Code != 405 {
				t.Fatalf("GET /api/keys/{id}: %d", rec.Code)
			}
		})
	}
}

func TestCannotRevokeSelf(t *testing.T) {
	f := newFixture(t, 20)
	rec := f.do("DELETE", "/api/keys/"+f.adminK.ID, false, f.admin, "")
	if rec.Code != 409 || errCode(t, rec) != "cannot_revoke_self" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", "/api/me", false, f.admin, ""); rec.Code != 200 {
		t.Fatalf("admin key revoked itself: %d", rec.Code)
	}
}

func TestKeysWithoutStore(t *testing.T) {
	h := Middleware(Options{})(http.NotFoundHandler())
	r := httptest.NewRequest("GET", "/api/keys", nil)
	r.RemoteAddr, r.Host = localPeer, localHost
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 503 || errCode(t, rec) != "keys_unavailable" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	r = httptest.NewRequest("GET", "/api/repos", nil)
	r.RemoteAddr, r.Host = remotePeer, publicHost
	r.Header.Set("Authorization", "Bearer rm_live_"+strings.Repeat("a", 32))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("no store must reject remote: %d", rec.Code)
	}
}

func TestRateLimitPerKey(t *testing.T) {
	f := newFixture(t, 2)
	post := func(secret string, local bool) *httptest.ResponseRecorder {
		return f.do("POST", "/api/analyses", local, secret, `{"repo":"x"}`)
	}
	for i := 0; i < 2; i++ {
		if rec := post(f.analyze, false); rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
		f.clock.Add(10 * time.Minute)
	}
	reached := f.reached
	rec := post(f.analyze, false)
	if rec.Code != 429 || errCode(t, rec) != "rate_limited" {
		t.Fatalf("over limit: %d %q", rec.Code, rec.Body.String())
	}
	// The first request was 20 minutes ago, so it leaves the window in 40 minutes.
	if ra := rec.Header().Get("Retry-After"); ra != "2400" {
		t.Fatalf("Retry-After = %q, want 2400", ra)
	}
	if f.reached != reached {
		t.Fatal("rate-limited request reached the API")
	}
	// Other keys have their own budget; other routes are not limited.
	if rec := post(f.admin, false); rec.Code != http.StatusAccepted {
		t.Fatalf("admin key: %d", rec.Code)
	}
	if rec := f.do("GET", "/api/analyses", false, f.analyze, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("GET analyses limited: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/analyses/a1/verify", false, f.analyze, `{}`); rec.Code != http.StatusAccepted {
		t.Fatalf("verify limited: %d", rec.Code)
	}
	// Local requests are never limited.
	for i := 0; i < 5; i++ {
		if rec := post("", true); rec.Code != http.StatusAccepted {
			t.Fatalf("local request %d limited: %d", i, rec.Code)
		}
	}
	// Sliding window: once the first request is an hour old, one slot frees.
	f.clock.Add(40 * time.Minute)
	if rec := post(f.analyze, false); rec.Code != http.StatusAccepted {
		t.Fatalf("after window: %d", rec.Code)
	}
	if rec := post(f.analyze, false); rec.Code != 429 {
		t.Fatalf("second slot must still be taken: %d", rec.Code)
	}
}

func TestRateLimitDefaultIs20(t *testing.T) {
	f := newFixture(t, 0)
	for i := 0; i < 20; i++ {
		if rec := f.do("POST", "/api/analyses", false, f.analyze, `{}`); rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if rec := f.do("POST", "/api/analyses", false, f.analyze, `{}`); rec.Code != 429 {
		t.Fatalf("21st request: %d", rec.Code)
	}
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	l := newLimiter(3, time.Hour)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		l.allow("k_a", now)
		now = now.Add(time.Second)
	}
	if n := l.size(); n > 3 {
		t.Fatalf("limiter holds %d timestamps for a limit of 3", n)
	}
	l.allow("k_b", now)
	now = now.Add(2 * time.Hour)
	l.allow("k_c", now)
	if n := l.size(); n != 1 {
		t.Fatalf("timestamps older than the window must be dropped, holding %d", n)
	}
}

// remoteReq sends a remote request from peer with the given Authorization
// header values (none when auth is nil).
func (f *fixture) remoteReq(method, path, peer string, auth []string, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr, r.Host = peer, publicHost
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if auth != nil {
		r.Header["Authorization"] = auth
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func TestPublicDemoKeylessRemoteRunsAsPublicIdentity(t *testing.T) {
	f := newFixtureWith(t, 20, true)
	rec := f.do("GET", "/api/repos", false, "", "")
	if rec.Code != http.StatusAccepted || rec.Body.String() != "inner:/api/repos" {
		t.Fatalf("keyless remote: %d %q", rec.Code, rec.Body.String())
	}
	if id := f.lastID; !id.Public || id.Local || id.Key != (Key{}) || id.Scope() != ScopeAnalyze {
		t.Fatalf("identity = %+v, scope %q; want the public demo identity with the analyze scope", id, id.Scope())
	}
	// Everything the analyze scope allows reaches the API.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/repos/demo/commits", ""},
		{"GET", "/api/commands", ""},
		{"GET", "/api/analyses", ""},
		{"POST", "/api/analyses", `{"repo":"demo"}`},
		{"GET", "/api/analyses/a1", ""},
		{"POST", "/api/analyses/a1/verify", `{}`},
	} {
		if rec := f.do(tc.method, tc.path, false, "", tc.body); rec.Code != http.StatusAccepted || !f.lastID.Public {
			t.Errorf("%s %s: %d identity %+v", tc.method, tc.path, rec.Code, f.lastID)
		}
	}
	// A forwarded request from a local proxy is not local, so it is public.
	r := httptest.NewRequest("GET", "/api/repos", nil)
	r.RemoteAddr, r.Host = localPeer, localHost
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted || !f.lastID.Public || f.lastID.Local {
		t.Fatalf("forwarded keyless request: %d identity %+v", rec.Code, f.lastID)
	}
	// Local requests do not change.
	if rec := f.do("GET", "/api/repos", true, "", ""); rec.Code != http.StatusAccepted || !f.lastID.Local || f.lastID.Public {
		t.Fatalf("local: %d identity %+v", rec.Code, f.lastID)
	}
}

func TestPublicDemoChecksEveryAuthorizationHeader(t *testing.T) {
	f := newFixtureWith(t, 20, true)
	revokedK, revoked, _ := f.store.Create("old", ScopeAdmin)
	f.store.Revoke(revokedK.ID)

	for name, auth := range map[string][]string{
		"empty":       {""},
		"malformed":   {"Bearer"},
		"wrong":       {"Bearer rm_live_" + strings.Repeat("Z", 32)},
		"revoked":     {"Bearer " + revoked},
		"basic":       {"Basic " + f.admin},
		"no scheme":   {f.admin},
		"two headers": {"Bearer " + f.analyze, "Bearer " + f.analyze},
		"two keys":    {"Bearer " + f.analyze, "Bearer " + f.admin},
	} {
		t.Run(name, func(t *testing.T) {
			before := f.reached
			rec := f.remoteReq("GET", "/api/repos", remotePeer, auth, "")
			if rec.Code != 401 || errCode(t, rec) != "unauthorized" {
				t.Fatalf("got %d %q", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
			if f.reached != before {
				t.Fatal("a request with an invalid key reached the API as the public identity")
			}
		})
	}

	// A valid key runs as that key, not as the public identity.
	rec := f.do("GET", "/api/analyses/a1", false, f.analyze, "")
	if rec.Code != http.StatusAccepted || f.lastID.Public || f.lastID.Local || f.lastID.Key.ID != f.anaK.ID {
		t.Fatalf("analyze key: %d identity %+v", rec.Code, f.lastID)
	}
	if rec := f.do("GET", "/api/keys", false, f.admin, ""); rec.Code != 200 {
		t.Fatalf("admin key on /api/keys: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", "/api/me", false, f.analyze, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mode":"key"`) {
		t.Fatalf("analyze key me: %d %q", rec.Code, rec.Body.String())
	}
}

func TestPublicDemoOffKeylessRemoteStillNeedsKey(t *testing.T) {
	f := newFixtureWith(t, 20, false)
	for _, p := range []string{"/api/repos", "/api/me", "/api/keys"} {
		rec := f.do("GET", p, false, "", "")
		if rec.Code != 401 || errCode(t, rec) != "unauthorized" || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s: %d %v %q", p, rec.Code, rec.Header(), rec.Body.String())
		}
	}
	if f.reached != 0 {
		t.Fatal("a keyless remote request reached the API with the public demo off")
	}
}

func TestPublicDemoHealthAuthRequired(t *testing.T) {
	for _, public := range []bool{true, false} {
		f := newFixtureWith(t, 20, public)
		for _, tc := range []struct {
			name   string
			local  bool
			secret string
		}{{"local", true, ""}, {"remote", false, ""}, {"remote bogus key", false, "rm_live_bogus"}, {"remote admin key", false, f.admin}} {
			rec := f.do("GET", "/api/health", tc.local, tc.secret, "")
			var m map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
				t.Fatalf("public=%v %s: %d %q", public, tc.name, rec.Code, rec.Body.String())
			}
			want := !public && !tc.local
			if m["auth_required"] != want {
				t.Errorf("public=%v %s: auth_required = %v, want %v", public, tc.name, m["auth_required"], want)
			}
			if m["ok"] != true || m["model_id"] != "muse-spark-1.3" {
				t.Errorf("public=%v %s: inner fields lost: %v", public, tc.name, m)
			}
		}
	}
}

func TestPublicDemoMe(t *testing.T) {
	f := newFixtureWith(t, 20, true)
	rec := f.do("GET", "/api/me", false, "", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"mode":"public"}` {
		t.Fatalf("public me: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do("GET", "/api/me", true, "", ""); strings.TrimSpace(rec.Body.String()) != `{"mode":"local"}` {
		t.Fatalf("local me: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do("POST", "/api/me", false, "", `{}`); rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST me: %d %v", rec.Code, rec.Header())
	}
}

func TestPublicDemoCannotManageKeys(t *testing.T) {
	f := newFixtureWith(t, 20, true)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/keys", ""},
		{"POST", "/api/keys", `{"name":"x","scope":"admin"}`},
		{"DELETE", "/api/keys/" + f.adminK.ID, ""},
		{"DELETE", "/api/keys/" + f.anaK.ID, ""},
		{"PUT", "/api/keys", `{}`},
		{"GET", "/api/keys/", ""},
	} {
		rec := f.do(tc.method, tc.path, false, "", tc.body)
		if rec.Code != 403 || errCode(t, rec) != "forbidden_scope" {
			t.Errorf("%s %s as the public identity: %d %q", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "public demo") || !strings.Contains(rec.Body.String(), "admin key") {
			t.Errorf("%s %s: the message must say the public demo needs an admin key: %q", tc.method, tc.path, rec.Body.String())
		}
	}
	if keys, _ := f.store.List(); len(keys) != 2 || keys[0].Revoked || keys[1].Revoked {
		t.Fatalf("the public identity changed the store: %+v", keys)
	}
	if f.reached != 0 {
		t.Fatal("keys routes must not reach the API")
	}
}

func TestPublicDemoIdentityIsNeverAKeyOrAdmin(t *testing.T) {
	pub := Identity{Public: true}
	if pub.Scope() != ScopeAnalyze {
		t.Fatalf("public scope = %q, want analyze", pub.Scope())
	}
	// Even with a key value attached by mistake, the public identity is never admin.
	if s := (Identity{Public: true, Key: Key{ID: "k_admin00001", Scope: ScopeAdmin}}).Scope(); s != ScopeAnalyze {
		t.Fatalf("public identity with an admin key value has scope %q", s)
	}
	if s := (Identity{}).Scope(); s == ScopeAdmin {
		t.Fatal("the zero identity must not be admin")
	}
	if s := (Identity{Local: true}).Scope(); s != ScopeAdmin {
		t.Fatalf("local scope = %q, want admin", s)
	}
	if s := (Identity{Key: Key{ID: "k_ci00000001", Scope: ScopeAnalyze}}).Scope(); s != ScopeAnalyze {
		t.Fatalf("analyze key scope = %q", s)
	}

	// The public bucket is never a key's bucket, whatever the key id.
	b := pub.bucket()
	if b == "" || strings.HasPrefix(b, "k_") || idRe.MatchString(b) {
		t.Fatalf("public bucket %q looks like a key id", b)
	}
	for _, kid := range []string{"k_ci00000001", "", b, "public", "k_", "key:", "key:public"} {
		if kb := (Identity{Key: Key{ID: kid}}).bucket(); kb == b {
			t.Fatalf("key id %q shares the public bucket %q", kid, b)
		}
	}
	if (Identity{Key: Key{ID: "k_a000000001"}}).bucket() == (Identity{Key: Key{ID: "k_b000000001"}}).bucket() {
		t.Fatal("two keys share a bucket")
	}
}

func TestPublicDemoSharesOneRateLimitBucket(t *testing.T) {
	f := newFixtureWith(t, 2, true)
	const body = `{"repo":"x"}`
	// Two different clients, one shared bucket.
	if rec := f.remoteReq("POST", "/api/analyses", "203.0.113.7:1000", nil, body); rec.Code != http.StatusAccepted || !f.lastID.Public {
		t.Fatalf("first public request: %d %+v", rec.Code, f.lastID)
	}
	f.clock.Add(10 * time.Minute)
	if rec := f.remoteReq("POST", "/api/analyses", "198.51.100.9:2000", nil, body); rec.Code != http.StatusAccepted {
		t.Fatalf("second public request: %d", rec.Code)
	}
	f.clock.Add(10 * time.Minute)
	reached := f.reached
	rec := f.remoteReq("POST", "/api/analyses", "192.0.2.44:3000", nil, body)
	if rec.Code != 429 || errCode(t, rec) != "rate_limited" {
		t.Fatalf("third public request: %d %q", rec.Code, rec.Body.String())
	}
	// The first request was 20 minutes ago, so it leaves the window in 40 minutes.
	if ra := rec.Header().Get("Retry-After"); ra != "2400" {
		t.Fatalf("Retry-After = %q, want 2400", ra)
	}
	if f.reached != reached {
		t.Fatal("rate-limited public request reached the API")
	}

	// A real key keeps its own bucket: the public demo did not use it up, and
	// using it up does not free the public bucket.
	for i := 0; i < 2; i++ {
		if rec := f.do("POST", "/api/analyses", false, f.analyze, body); rec.Code != http.StatusAccepted || f.lastID.Key.ID != f.anaK.ID {
			t.Fatalf("analyze key request %d: %d", i, rec.Code)
		}
	}
	if rec := f.do("POST", "/api/analyses", false, f.analyze, body); rec.Code != 429 {
		t.Fatalf("analyze key over its own limit: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/analyses", false, "", body); rec.Code != 429 {
		t.Fatalf("public bucket freed by key traffic: %d", rec.Code)
	}
	// Other routes are not limited for the public identity.
	if rec := f.do("POST", "/api/analyses/a1/verify", false, "", `{}`); rec.Code != http.StatusAccepted {
		t.Fatalf("public verify limited: %d", rec.Code)
	}
	// Local requests are never limited.
	if rec := f.do("POST", "/api/analyses", true, "", body); rec.Code != http.StatusAccepted {
		t.Fatalf("local request limited: %d", rec.Code)
	}
	// Sliding window: once the first public request is an hour old, one slot frees.
	f.clock.Add(40 * time.Minute)
	if rec := f.do("POST", "/api/analyses", false, "", body); rec.Code != http.StatusAccepted {
		t.Fatalf("public after window: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/analyses", false, "", body); rec.Code != 429 {
		t.Fatalf("second public slot must still be taken: %d", rec.Code)
	}
}
