package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The httptest TLS certificate is valid for example.com, so test URLs use it.
const testHost = "example.com"

// A secret-bearing URL: neither the path nor the query may ever leak.
const testURL = "https://" + testHost + "/services/T000/B000/PATHSECRET?token=QUERYSECRET"

var testSecret = []byte("0123456789abcdef0123456789abcdef")

type mapResolver map[string][]netip.Addr

func (m mapResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := m[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func addrs(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// harness is a TLS receiver plus a Sender whose resolver maps testHost to
// the given addresses and whose dialer routes every dial to the receiver.
// If the vetting were missing, a dial to a blocked address would therefore
// reach the receiver and be counted.
type harness struct {
	srv      *httptest.Server
	sender   *Sender
	requests atomic.Int32
	dials    atomic.Int32
	mu       sync.Mutex
	dialed   []string
	sleeps   []time.Duration
	failDial atomic.Int32 // number of upcoming dials to fail
}

func newHarness(t *testing.T, handler http.HandlerFunc, ips ...string) *harness {
	t.Helper()
	h := &harness{}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(h.srv.Close)
	tlsCfg := h.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	h.sender = &Sender{
		Secret:   testSecret,
		Resolver: mapResolver{testHost: addrs(t, ips...)},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.dials.Add(1)
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			if h.failDial.Load() > 0 {
				h.failDial.Add(-1)
				return nil, errors.New("connection refused")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", h.srv.Listener.Addr().String())
		},
		TLSConfig: tlsCfg,
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.mu.Unlock()
			return nil
		},
	}
	return h
}

func samplePayload() Payload {
	return Payload{
		ID: "a123", Repo: "pagination-demo", Status: "done",
		Baseline: strings.Repeat("b", 40), Current: strings.Repeat("c", 40),
		BaselinePassed: true, CurrentPassed: false, BaselineRan: true, CurrentRan: true,
		ModelStatus: "ok",
		TopHypothesis: &Hypothesis{Rank: 1, Title: "end index off by one", File: "src/pagination.ts",
			LineStart: 7, LineEnd: 13, ConfidenceLabel: "high"},
		CreatedAt: "2026-11-04T10:00:00Z", FinishedAt: "2026-11-04T10:01:00Z",
	}
}

func TestValidate(t *testing.T) {
	ok := []string{
		"https://hooks.example.com/services/T0/B0/x",
		"https://hooks.example.com:443/x?y=1",
		"HTTPS://hooks.example.com/x",
		"https://hooks.example.com",
		"https://" + testHost + "/" + strings.Repeat("a", MaxURLLength-len("https://"+testHost+"/")),
	}
	for _, u := range ok {
		if _, err := Validate(u); err != nil {
			t.Errorf("%.60q must be accepted: %v", u, err)
		}
	}
	bad := map[string]string{
		"http scheme":     "http://hooks.example.com/x",
		"userinfo":        "https://user:pass@hooks.example.com/x",
		"user only":       "https://user@hooks.example.com/x",
		"port 8443":       "https://hooks.example.com:8443/x",
		"port 80":         "https://hooks.example.com:80/x",
		"too long":        "https://" + testHost + "/" + strings.Repeat("a", MaxURLLength),
		"no host":         "https:///x",
		"port only":       "https://:443/x",
		"empty":           "",
		"ftp":             "ftp://hooks.example.com/x",
		"javascript":      "javascript:alert(1)",
		"opaque":          "https:hooks.example.com",
		"relative":        "/hook",
		"control char":    "https://hooks.example.com/\x00",
		"space in host":   "https://hooks example.com/x",
		"non-numeric prt": "https://hooks.example.com:abc/x",
	}
	for name, u := range bad {
		_, err := Validate(u)
		if err == nil {
			t.Errorf("%s: %.60q must be rejected", name, u)
			continue
		}
		if strings.Contains(err.Error(), "/x") || strings.Contains(err.Error(), "pass") {
			t.Errorf("%s: error echoes the URL: %v", name, err)
		}
	}
}

func TestIsPublic(t *testing.T) {
	public := []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range public {
		if !IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s must be public", s)
		}
	}
	blocked := []string{
		"127.0.0.1", "127.255.0.1", "10.0.0.5", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "100.64.1.1", "100.127.255.254",
		"0.0.0.0", "0.1.2.3", "192.0.0.8", "198.18.0.1", "198.19.255.255",
		"240.0.0.1", "255.255.255.255", "224.0.0.1", "239.1.1.1",
		"::1", "::", "fd00::1", "fc00::1", "fe80::1", "ff02::1", "ff05::2",
		"::ffff:127.0.0.1", "::ffff:10.0.0.5", "::ffff:169.254.169.254", "::ffff:100.64.1.1",
		"64:ff9b::a00:5", "fd00:ec2::254",
	}
	for _, s := range blocked {
		if IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s must not be public", s)
		}
	}
	if IsPublic(netip.Addr{}) {
		t.Error("the zero Addr must not be public")
	}
}

func TestBlockedAddressesAreNeverDialed(t *testing.T) {
	cases := map[string][]string{
		"loopback v4":       {"127.0.0.1"},
		"private":           {"10.0.0.5"},
		"metadata":          {"169.254.169.254"},
		"loopback v6":       {"::1"},
		"mapped loopback":   {"::ffff:127.0.0.1"},
		"cgnat":             {"100.64.1.1"},
		"ula":               {"fd00::1"},
		"mixed public+priv": {"203.0.113.10", "10.0.0.5"},
		"mixed priv first":  {"192.168.0.1", "203.0.113.10"},
	}
	for name, ips := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, ips...)
			res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
			if res.Delivered {
				t.Fatal("delivery to a non-public address must fail")
			}
			if n := h.requests.Load(); n != 0 {
				t.Fatalf("receiver got %d request(s), want 0", n)
			}
			if n := h.dials.Load(); n != 0 {
				t.Fatalf("dialer called %d time(s), want 0", n)
			}
			if !strings.Contains(res.Err, "non-public") {
				t.Fatalf("error %q does not say why", res.Err)
			}
			if res.Attempts != 1 || len(h.sleeps) != 0 {
				t.Fatalf("a blocked address must not be retried: attempts=%d sleeps=%v", res.Attempts, h.sleeps)
			}
			assertNoURLSecrets(t, res.Err)
		})
	}
}

// The production resolver (nil Resolver) must reject loopback too.
func TestDefaultResolverRejectsLoopback(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1/x?token=QUERYSECRET", "https://[::1]/x", "https://localhost/x"} {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
		h.sender.Resolver = nil
		res := h.sender.Send(context.Background(), u, FormatJSON, samplePayload())
		if res.Delivered || h.requests.Load() != 0 || h.dials.Load() != 0 {
			t.Fatalf("%s: delivered=%v requests=%d dials=%d err=%q", u, res.Delivered, h.requests.Load(), h.dials.Load(), res.Err)
		}
	}
}

func TestDialsOnlyTheVettedAddressOnPort443(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if !res.Delivered {
		t.Fatalf("not delivered: %+v", res)
	}
	if len(h.dialed) != 1 || h.dialed[0] != "203.0.113.10:443" {
		t.Fatalf("dialed %v, want the vetted IP on 443", h.dialed)
	}
}

func TestJSONDeliverySignedAndExact(t *testing.T) {
	var got struct {
		body                          []byte
		sig, event, ua, ctype, sni, h string
		method, path                  string
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.sig = r.Header.Get("X-RegressionMap-Signature")
		got.event = r.Header.Get("X-RegressionMap-Event")
		got.ua = r.Header.Get("User-Agent")
		got.ctype = r.Header.Get("Content-Type")
		got.sni = r.TLS.ServerName
		got.h = r.Host
		got.method = r.Method
		got.path = r.URL.RequestURI()
		w.WriteHeader(200)
	}, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if !res.Delivered || res.Attempts != 1 || res.StatusCode != 200 || res.Err != "" {
		t.Fatalf("result %+v", res)
	}
	want := `{"event":"analysis.completed","id":"a123","repo":"pagination-demo","status":"done","error":"",` +
		`"baseline":"` + strings.Repeat("b", 40) + `","current":"` + strings.Repeat("c", 40) + `",` +
		`"baseline_passed":true,"current_passed":false,"model_status":"ok",` +
		`"top_hypothesis":{"rank":1,"title":"end index off by one","file":"src/pagination.ts","line_start":7,"line_end":13,"confidence_label":"high"},` +
		`"created_at":"2026-11-04T10:00:00Z","finished_at":"2026-11-04T10:01:00Z"}`
	if string(got.body) != want {
		t.Fatalf("body\n got %s\nwant %s", got.body, want)
	}
	mac := hmac.New(sha256.New, testSecret)
	mac.Write(got.body)
	if got.sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %q does not verify over the received body", got.sig)
	}
	if got.event != "analysis.completed" || got.ua != "RegressionMap-Webhook/1" || got.ctype != "application/json" || got.method != "POST" {
		t.Fatalf("headers: event=%q ua=%q ctype=%q method=%q", got.event, got.ua, got.ctype, got.method)
	}
	if got.sni != testHost || got.h != testHost {
		t.Fatalf("TLS ServerName %q / Host %q, want the original host %q", got.sni, got.h, testHost)
	}
	if got.path != "/services/T000/B000/PATHSECRET?token=QUERYSECRET" {
		t.Fatalf("request URI %q", got.path)
	}
}

func TestNullTopHypothesis(t *testing.T) {
	p := samplePayload()
	p.TopHypothesis = nil
	b, err := JSONBody(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"top_hypothesis":null`) {
		t.Fatalf("body %s", b)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+testHost+"/elsewhere", http.StatusFound)
	}, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.StatusCode != 302 || res.Attempts != 1 {
		t.Fatalf("result %+v", res)
	}
	if n := h.requests.Load(); n != 1 {
		t.Fatalf("receiver got %d requests; the redirect was followed", n)
	}
	assertNoURLSecrets(t, res.Err)
}

func TestRetriesOn503ThenSucceeds(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if !res.Delivered || res.Attempts != 3 || res.StatusCode != 200 || res.Err != "" {
		t.Fatalf("result %+v", res)
	}
	if len(h.sleeps) != 2 || h.sleeps[0] != time.Second || h.sleeps[1] != 4*time.Second {
		t.Fatalf("backoff %v, want [1s 4s]", h.sleeps)
	}
}

func TestRetriesOn429AndNetworkError(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}, "203.0.113.10")
	if res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload()); !res.Delivered || res.Attempts != 2 {
		t.Fatalf("429: %+v", res)
	}

	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, "203.0.113.10")
	h2.failDial.Store(1)
	if res := h2.sender.Send(context.Background(), testURL, FormatJSON, samplePayload()); !res.Delivered || res.Attempts != 2 {
		t.Fatalf("network error: %+v", res)
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Attempts != 3 || res.StatusCode != 502 || h.requests.Load() != 3 {
		t.Fatalf("result %+v requests=%d", res, h.requests.Load())
	}
	if !strings.Contains(res.Err, "502") {
		t.Fatalf("error %q", res.Err)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Attempts != 1 || res.StatusCode != 400 || h.requests.Load() != 1 || len(h.sleeps) != 0 {
		t.Fatalf("result %+v requests=%d sleeps=%v", res, h.requests.Load(), h.sleeps)
	}
}

func assertNoURLSecrets(t *testing.T, s string) {
	t.Helper()
	for _, leak := range []string{"PATHSECRET", "QUERYSECRET", "/services", "token="} {
		if strings.Contains(s, leak) {
			t.Fatalf("error leaks %q: %s", leak, s)
		}
	}
}

func TestErrorsNeverContainPathOrQuery(t *testing.T) {
	// Network errors on every attempt: net/http wraps them in *url.Error,
	// whose message contains the full URL.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, "203.0.113.10")
	h.failDial.Store(10)
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Attempts != 3 || res.Err == "" {
		t.Fatalf("result %+v", res)
	}
	assertNoURLSecrets(t, res.Err)

	// DNS failure.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h2.sender.Resolver = mapResolver{}
	res = h2.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Err == "" {
		t.Fatalf("dns: %+v", res)
	}
	assertNoURLSecrets(t, res.Err)

	// Invalid URL handed directly to Send.
	res = h.sender.Send(context.Background(), "http://"+testHost+"/services/PATHSECRET?token=QUERYSECRET", FormatJSON, samplePayload())
	if res.Delivered || res.Attempts != 0 {
		t.Fatalf("invalid url: %+v", res)
	}
	assertNoURLSecrets(t, res.Err)
}

func TestPerAttemptTimeout(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, "203.0.113.10")
	defer close(release)
	h.sender.Timeout = 150 * time.Millisecond
	start := time.Now()
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Attempts != 3 {
		t.Fatalf("result %+v", res)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("took %v", el)
	}
	assertNoURLSecrets(t, res.Err)
	if DefaultTimeout != 5*time.Second {
		t.Fatalf("DefaultTimeout %v", DefaultTimeout)
	}
}

func TestSlackDelivery(t *testing.T) {
	var body []byte
	var sig string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		sig = r.Header.Get("X-RegressionMap-Signature")
		w.WriteHeader(200)
	}, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, FormatSlack, samplePayload())
	if !res.Delivered {
		t.Fatalf("result %+v", res)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil || len(msg) != 1 {
		t.Fatalf("slack body %s: %v", body, err)
	}
	text, _ := msg["text"].(string)
	for _, want := range []string{"pagination-demo", "done", "Baseline PASS", "HEAD FAIL", "end index off by one", "src/pagination.ts:7", "confidence high"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q: %s", want, text)
		}
	}
	if !strings.HasSuffix(text, "AI-generated hypothesis, not verified.") {
		t.Fatalf("text must end with the disclaimer: %s", text)
	}
	if strings.Contains(text, "\n") {
		t.Fatalf("text must be one paragraph: %q", text)
	}
	if sig != "" {
		t.Fatalf("slack format must not be signed, got %q", sig)
	}
}

func TestSlackTextEdgeCases(t *testing.T) {
	p := samplePayload()
	p.TopHypothesis = nil
	p.Status, p.CurrentRan, p.BaselineRan = "error", false, false
	text := SlackText(p)
	if !strings.HasSuffix(text, "AI-generated hypothesis, not verified.") || !strings.Contains(text, "error") || !strings.Contains(text, "not run") {
		t.Fatalf("text %s", text)
	}
	// Model output is untrusted: Slack control sequences are escaped and
	// newlines flattened.
	p = samplePayload()
	p.TopHypothesis.Title = "x <!channel> <https://evil.example|click> & more\nsecond line"
	text = SlackText(p)
	if strings.ContainsAny(text, "<>\n") || !strings.Contains(text, "&lt;!channel&gt;") || !strings.Contains(text, "&amp;") {
		t.Fatalf("not escaped: %s", text)
	}
}

func TestUnknownFormatRejected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, "203.0.113.10")
	res := h.sender.Send(context.Background(), testURL, "xml", samplePayload())
	if res.Delivered || res.Attempts != 0 || h.requests.Load() != 0 {
		t.Fatalf("result %+v", res)
	}
	if !ValidFormat("json") || !ValidFormat("slack") || ValidFormat("xml") || ValidFormat("") {
		t.Fatal("ValidFormat")
	}
}
