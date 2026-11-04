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
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// The httptest TLS certificate is valid for example.com, so test URLs use it.
const testHost = "example.com"

// publicIP stands in for the receiver's public address; the harness dialer
// routes every dial to the local receiver. (The TEST-NET ranges are not
// public under the v3 addendum, so they cannot stand in.)
const publicIP = "93.184.216.34"

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
	cases := []struct {
		addr   string
		public bool
		why    string
	}{
		// Public controls.
		{"93.184.216.34", true, "public IPv4"},
		{"8.8.8.8", true, "public IPv4"},
		{"1.1.1.1", true, "public IPv4"},
		{"2606:4700:4700::1111", true, "public IPv6"},
		{"2001:4860:4860::8888", true, "public IPv6 outside the Teredo /32"},
		{"::ffff:8.8.8.8", true, "IPv4-mapped: judged by the public IPv4 it carries"},

		// v3 ranges.
		{"127.0.0.1", false, "loopback"},
		{"127.255.0.1", false, "loopback"},
		{"10.0.0.5", false, "private"},
		{"172.16.0.1", false, "private"},
		{"192.168.1.1", false, "private"},
		{"169.254.169.254", false, "link-local, cloud metadata"},
		{"169.254.0.1", false, "link-local"},
		{"100.64.1.1", false, "CGNAT"},
		{"100.127.255.254", false, "CGNAT"},
		{"0.0.0.0", false, "unspecified"},
		{"0.1.2.3", false, "this network"},
		{"192.0.0.8", false, "IETF protocol assignments"},
		{"198.18.0.1", false, "benchmarking"},
		{"198.19.255.255", false, "benchmarking"},
		{"240.0.0.1", false, "reserved"},
		{"255.255.255.255", false, "broadcast"},
		{"224.0.0.1", false, "multicast"},
		{"239.1.1.1", false, "multicast"},
		{"::1", false, "loopback"},
		{"::", false, "unspecified"},
		{"fd00::1", false, "unique local"},
		{"fc00::1", false, "unique local"},
		{"fe80::1", false, "link-local"},
		{"ff02::1", false, "multicast"},
		{"ff05::2", false, "multicast"},
		{"64:ff9b::a00:5", false, "NAT64"},
		{"fd00:ec2::254", false, "metadata (unique local)"},

		// IPv4-mapped: judged by the IPv4 address it carries.
		{"::ffff:127.0.0.1", false, "mapped loopback"},
		{"::ffff:10.0.0.5", false, "mapped private"},
		{"::ffff:169.254.169.254", false, "mapped metadata"},
		{"::ffff:100.64.1.1", false, "mapped CGNAT"},
		{"::ffff:192.0.2.1", false, "mapped documentation range"},

		// Addendum: IPv6 that embeds IPv4.
		{"2002:7f00:1::1", false, "6to4 embedding 127.0.0.1"},
		{"2002:a9fe:a9fe::1", false, "6to4 embedding 169.254.169.254"},
		{"2002:a00:5::1", false, "6to4 embedding 10.0.0.5"},
		{"2002:808:808::1", false, "6to4, even embedding a public IPv4"},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false, "Teredo"},
		{"2001::1", false, "Teredo"},
		{"::ffff:0:7f00:1", false, "IPv4-translated 127.0.0.1"},
		{"::ffff:0:808:808", false, "IPv4-translated, even of a public IPv4"},

		// Addendum: documentation ranges.
		{"192.0.2.1", false, "TEST-NET-1"},
		{"198.51.100.1", false, "TEST-NET-2"},
		{"203.0.113.10", false, "TEST-NET-3"},
		{"2001:db8::1", false, "IPv6 documentation"},
		{"2001:db8:ffff::1", false, "IPv6 documentation"},
	}
	for _, c := range cases {
		if got := IsPublic(netip.MustParseAddr(c.addr)); got != c.public {
			t.Errorf("IsPublic(%s) = %v, want %v (%s)", c.addr, got, c.public, c.why)
		}
	}
	if IsPublic(netip.Addr{}) {
		t.Error("the zero Addr must not be public")
	}
	if IsPublic(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("a zoned address must not be public")
	}
}

// The message for a refused host is fixed: it never names the address the
// host resolved to, and a name that does not resolve at all gives the same
// answer, so notify_url cannot map internal DNS names to addresses.
func TestBlockedErrorNeverCarriesAddress(t *testing.T) {
	const want = "host does not resolve to a public address"
	cases := map[string]struct {
		res  mapResolver
		hide []string
	}{
		"private":          {mapResolver{testHost: addrs(t, "10.20.30.40")}, []string{"10.20.30.40"}},
		"public+private":   {mapResolver{testHost: addrs(t, publicIP, "172.16.5.6")}, []string{"172.16.5.6", publicIP}},
		"mapped private":   {mapResolver{testHost: addrs(t, "::ffff:10.0.0.5")}, []string{"10.0.0.5"}},
		"6to4 of loopback": {mapResolver{testHost: addrs(t, "2002:7f00:1::1")}, []string{"2002:7f00:1::1", "127.0.0.1"}},
		"no such host":     {mapResolver{}, nil},
		"no addresses":     {mapResolver{testHost: {}}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
			h.sender.Resolver = c.res
			res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
			if res.Delivered || res.Err != want {
				t.Fatalf("Err = %q, want %q", res.Err, want)
			}
			for _, ip := range c.hide {
				if strings.Contains(res.Err, ip) {
					t.Fatalf("Err %q carries the resolved address %s", res.Err, ip)
				}
			}
			if res.Attempts != 1 || h.dials.Load() != 0 || len(h.sleeps) != 0 {
				t.Fatalf("a refused host must not be dialled or retried: attempts=%d dials=%d sleeps=%v", res.Attempts, h.dials.Load(), h.sleeps)
			}
			// The resolver check itself returns the bare error.
			if _, err := h.sender.vet(context.Background(), testHost); !errors.Is(err, ErrBlockedAddress) || err.Error() != want {
				t.Fatalf("vet error %v, want exactly %q", err, want)
			}
		})
	}

	// The connect-time check refuses inside net.Dialer, whose *net.OpError
	// names the address. Send reports the fixed text only.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP)
	h.sender.DialContext = func(_ context.Context, network, _ string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Addr: &net.TCPAddr{IP: net.ParseIP("10.9.8.7"), Port: 443}, Err: ErrBlockedAddress}
	}
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Err != want || res.Attempts != 1 {
		t.Fatalf("connect-time refusal: Err = %q attempts=%d, want %q after 1 attempt", res.Err, res.Attempts, want)
	}
}

// Error text can come from the receiver (a TLS certificate name, for
// example). Control characters are removed so it cannot forge log records
// or break the JSON log.
func TestErrorTextHasNoControlCharacters(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP)
	h.sender.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("certificate is valid for evil.example\n{\"level\":\"INFO\",\"msg\":\"forged\"}\r\x1b[2J\x00\x7f\u0085\u009fend")
	}
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Err == "" {
		t.Fatalf("result %+v", res)
	}
	for _, r := range res.Err {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("Err keeps control character %U: %q", r, res.Err)
		}
	}
	if !strings.Contains(res.Err, "evil.example") || !strings.Contains(res.Err, "[2Jend") {
		t.Fatalf("Err lost its printable text: %q", res.Err)
	}
}

// A host (as url.Parse decodes it) that holds a control, format or space
// character, or that is not valid UTF-8, is refused with the generic error,
// so it never reaches notification.host, DATA_DIR or the log. Other
// non-ASCII letters (IDN hosts) stay accepted.
func TestValidateRefusesControlFormatAndSpaceInHost(t *testing.T) {
	const want = "notify_url is not a valid URL"
	bad := map[string]string{
		"U+0085 raw":           "https://h\u0085ooks.example.com/x",
		"U+0085 as %C2%85":     "https://h%C2%85ooks.example.com/x",
		"U+009B raw":           "https://h\u009b2Kooks.example.com/x",
		"U+009B as %C2%9B":     "https://h%C2%9B2Kooks.example.com/x",
		"U+202E raw":           "https://h\u202eooks.example.com/x",
		"U+202E as %E2%80%AE":  "https://h%E2%80%AEooks.example.com/x",
		"U+200B raw":           "https://h\u200books.example.com/x",
		"U+FEFF raw":           "https://\ufeffhooks.example.com/x",
		"U+00A0 raw":           "https://hooks\u00a0.example.com/x",
		"U+2028 raw":           "https://hooks.example.com\u2028/x",
		"U+2029 raw":           "https://hooks\u2029.example.com/x",
		"U+3000 raw":           "https://hooks\u3000.example.com/x",
		"invalid UTF-8 %FF":    "https://h%FFooks.example.com/x",
		"invalid UTF-8 raw":    "https://h\xffooks.example.com/x",
		"truncated UTF-8 %C2":  "https://h%C2ooks.example.com/x",
		"tab (url.Parse)":      "https://hooks\t.example.com/x",
		"space (url.Parse)":    "https://hooks .example.com/x",
		"before the https one": "http://h\u0085ooks.example.com/x",
		"with port 443":        "https://h\u202eooks.example.com:443/x",
	}
	for name, u := range bad {
		_, err := Validate(u)
		if err == nil || err.Error() != want {
			t.Errorf("%s: %+q gave %v, want %q", name, u, err, want)
		}
	}
	ok := []string{
		"https://bücher.example/x",
		"https://b%C3%BCcher.example/x",
		"https://hooks.example.com/x",
		"https://HOOKS.Example.COM:443/x",
		"https://xn--bcher-kva.example/x",
		"https://[2606:4700:4700::1111]/x",
		"https://93.184.216.34/x",
	}
	for _, u := range ok {
		if _, err := Validate(u); err != nil {
			t.Errorf("%+q must be accepted: %v", u, err)
		}
	}
}

// "At most 2048 characters" counts Unicode characters, not bytes.
func TestValidateCountsCharactersNotBytes(t *testing.T) {
	prefix := "https://" + testHost + "/"
	exact := prefix + strings.Repeat("é", MaxURLLength-utf8.RuneCountInString(prefix))
	if n := utf8.RuneCountInString(exact); n != MaxURLLength || len(exact) <= MaxURLLength {
		t.Fatalf("fixture: %d characters, %d bytes", n, len(exact))
	}
	if _, err := Validate(exact); err != nil {
		t.Fatalf("a URL of exactly %d characters (%d bytes) must be accepted: %v", MaxURLLength, len(exact), err)
	}
	_, err := Validate(exact + "é")
	if err == nil || err.Error() != "notify_url is longer than 2048 characters" {
		t.Fatalf("a URL of %d characters must be rejected as too long, got %v", MaxURLLength+1, err)
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
		"mixed public+priv": {publicIP, "10.0.0.5"},
		"mixed priv first":  {"192.168.0.1", publicIP},
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
			if res.Err != "host does not resolve to a public address" {
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
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }, publicIP)
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if !res.Delivered {
		t.Fatalf("not delivered: %+v", res)
	}
	if len(h.dialed) != 1 || h.dialed[0] != publicIP+":443" {
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
	}, publicIP)
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if !res.Delivered || res.Attempts != 1 || res.StatusCode != 200 || res.Err != "" {
		t.Fatalf("result %+v", res)
	}
	want := `{"event":"analysis.completed","id":"a123","repo":"pagination-demo","status":"done","error":"",` +
		`"baseline":"` + strings.Repeat("b", 40) + `","current":"` + strings.Repeat("c", 40) + `",` +
		`"baseline_passed":true,"current_passed":false,"model_status":"ok",` +
		`"top_hypothesis":{"rank":1,"title":"end index off by one","file":"src/pagination.ts","line_start":7,"line_end":13,"confidence_label":"high"},` +
		`"created_at":"2026-11-04T10:00:00Z","finished_at":"2026-11-04T10:01:00Z","review":null}`
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
	}, publicIP)
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
	}, publicIP)
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
	}, publicIP)
	if res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload()); !res.Delivered || res.Attempts != 2 {
		t.Fatalf("429: %+v", res)
	}

	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP)
	h2.failDial.Store(1)
	if res := h2.sender.Send(context.Background(), testURL, FormatJSON, samplePayload()); !res.Delivered || res.Attempts != 2 {
		t.Fatalf("network error: %+v", res)
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }, publicIP)
	res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
	if res.Delivered || res.Attempts != 3 || res.StatusCode != 502 || h.requests.Load() != 3 {
		t.Fatalf("result %+v requests=%d", res, h.requests.Load())
	}
	if !strings.Contains(res.Err, "502") {
		t.Fatalf("error %q", res.Err)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }, publicIP)
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
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP)
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
	}, publicIP)
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
	}, publicIP)
	res := h.sender.Send(context.Background(), testURL, FormatSlack, samplePayload())
	if !res.Delivered {
		t.Fatalf("result %+v", res)
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil || len(msg) != 1 {
		t.Fatalf("slack body %s: %v", body, err)
	}
	text, _ := msg["text"].(string)
	for _, want := range []string{"pagination-demo", "done", "Baseline PASS", "HEAD FAIL", "end index off by one", "`src/pagination.ts:7`", "confidence high"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q: %s", want, text)
		}
	}
	if !strings.HasSuffix(text, "AI-generated hypothesis and review, not verified.") {
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
	if !strings.HasSuffix(text, "AI-generated hypothesis and review, not verified.") || !strings.Contains(text, "error") || !strings.Contains(text, "not run") {
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
	if !strings.HasSuffix(text, "AI-generated hypothesis and review, not verified.") {
		t.Fatalf("text must end with the disclaimer: %s", text)
	}
}

func TestJSONBodyCarriesReviewLast(t *testing.T) {
	p := samplePayload()
	p.Review = &ReviewSummary{Status: "ok", Confirmed: 1, Refuted: 2, TopFinding: &ReviewFinding{
		ID: "R1", Lens: "security", Severity: "high", Title: "Token compared with ==",
		File: "src/auth.ts", LineStart: 12, LineEnd: 12, Verdict: "confirmed"}}
	b, err := JSONBody(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `"finished_at":"2026-11-04T10:01:00Z","review":{"status":"ok","confirmed":1,"disputed":0,"refuted":2,"unchecked":0,` +
		`"top_finding":{"id":"R1","lens":"security","severity":"high","title":"Token compared with ==","file":"src/auth.ts",` +
		`"line_start":12,"line_end":12,"verdict":"confirmed"}}}`
	if !strings.HasSuffix(string(b), want) {
		t.Fatalf("body\n got %s\nwant suffix %s", b, want)
	}
	p.Review.TopFinding = nil
	if b, err = JSONBody(p); err != nil || !strings.HasSuffix(string(b), `"unchecked":0,"top_finding":null}}`) {
		t.Fatalf("nil top finding: %s %v", b, err)
	}
}

func TestSlackTextContractExample(t *testing.T) {
	p := samplePayload()
	p.Review = &ReviewSummary{Status: "ok", Confirmed: 1, Disputed: 1, TopFinding: &ReviewFinding{
		Lens: "code", Severity: "high", Title: "Last page drops one item", File: "src/pagination.ts", LineStart: 7}}
	want := "RegressionMap analysis of pagination-demo finished with status done. Baseline PASS, HEAD FAIL. " +
		"Top hypothesis: `end index off by one` (`src/pagination.ts:7`, confidence high). " +
		"Muse review: 1 confirmed, 1 disputed. Top: [code, high] `Last page drops one item` (`src/pagination.ts:7`). " +
		"AI-generated hypothesis and review, not verified."
	if got := SlackText(p); got != want {
		t.Fatalf("slack text\n got %s\nwant %s", got, want)
	}
}

func TestSlackTextReviewSentence(t *testing.T) {
	const head = "RegressionMap analysis of pagination-demo finished with status done. Baseline PASS, HEAD FAIL. " +
		"Top hypothesis: `end index off by one` (`src/pagination.ts:7`, confidence high)."
	const tail = " AI-generated hypothesis and review, not verified."
	for _, c := range []struct {
		name   string
		review *ReviewSummary
		want   string
	}{
		{"counts in display order and a top finding with no file",
			&ReviewSummary{Status: "ok", Confirmed: 2, Refuted: 3, Unchecked: 1,
				TopFinding: &ReviewFinding{Lens: "security", Severity: "high", Title: "x"}},
			" Muse review: 2 confirmed, 1 unchecked, 3 refuted. Top: [security, high] `x`."},
		{"every count above 0 in display order",
			&ReviewSummary{Status: "ok", Confirmed: 1, Unchecked: 2, Disputed: 3, Refuted: 4},
			" Muse review: 1 confirmed, 2 unchecked, 3 disputed, 4 refuted."},
		{"a top finding on a whole file has no :0",
			&ReviewSummary{Status: "ok", Confirmed: 1,
				TopFinding: &ReviewFinding{Lens: "code", Severity: "low", Title: "x", File: "src/a.ts", LineStart: 0}},
			" Muse review: 1 confirmed. Top: [code, low] `x` (`src/a.ts`)."},
		{"only refuted", &ReviewSummary{Status: "ok", Refuted: 3}, " Muse review: 3 refuted."},
		{"no findings", &ReviewSummary{Status: "ok"}, " Muse review: no findings."},
		{"not available", &ReviewSummary{Status: "error"}, " Muse review: not available (error)."},
		{"disabled is not available", &ReviewSummary{Status: "disabled"}, " Muse review: not available (disabled)."},
		{"invalid is not available", &ReviewSummary{Status: "invalid"}, " Muse review: not available (invalid)."},
		{"skipped is not available", &ReviewSummary{Status: "skipped"}, " Muse review: not available (skipped)."},
		{"not ok with counts is not available", &ReviewSummary{Status: "error", Confirmed: 2, Refuted: 1},
			" Muse review: not available (error)."},
		{"no review", nil, ""},
	} {
		p := samplePayload()
		p.Review = c.review
		if got := SlackText(p); got != head+c.want+tail {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, head+c.want+tail)
		}
	}
}

// Every model-written string and every location sits in a closed code span
// with its backticks removed, so Slack makes no link and no mention of it.
func TestSlackTextReviewCodeSpans(t *testing.T) {
	p := samplePayload()
	p.TopHypothesis.Title = "a`b <https://evil.example|x> & http://evil.example"
	p.Review = &ReviewSummary{Status: "ok", Confirmed: 1, TopFinding: &ReviewFinding{
		Lens: "security", Severity: "high", Title: "<!channel>", File: "src/a`b.ts", LineStart: 7}}
	text := SlackText(p)
	for _, want := range []string{
		"Top hypothesis: `ab &lt;https://evil.example|x&gt; &amp; http://evil.example` (`src/pagination.ts:7`, confidence high).",
		"Top: [security, high] `&lt;!channel&gt;` (`src/ab.ts:7`).",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "`"); n != 8 || strings.ContainsAny(text, "<>\n") {
		t.Fatalf("%d backticks, want 8 (four closed spans), and no < > or newline:\n%s", n, text)
	}

	p.Review.TopFinding.Title = " ``\n "
	text = SlackText(p)
	if !strings.Contains(text, "Top: [security, high] untitled (`src/ab.ts:7`).") || strings.Count(text, "`") != 6 {
		t.Fatalf("an empty title must be the plain word untitled:\n%s", text)
	}
}

func TestUnknownFormatRejected(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP)
	res := h.sender.Send(context.Background(), testURL, "xml", samplePayload())
	if res.Delivered || res.Attempts != 0 || h.requests.Load() != 0 {
		t.Fatalf("result %+v", res)
	}
	if !ValidFormat("json") || !ValidFormat("slack") || ValidFormat("xml") || ValidFormat("") {
		t.Fatal("ValidFormat")
	}
}

func TestStripControl(t *testing.T) {
	var in strings.Builder
	for r := rune(0); r <= 0xa0; r++ {
		in.WriteRune(r)
	}
	in.WriteString("é…✓")
	got := StripControl(in.String())
	var want strings.Builder
	for r := rune(0x20); r < 0x7f; r++ {
		want.WriteRune(r)
	}
	want.WriteString(" é…✓")
	if got != want.String() {
		t.Fatalf("StripControl\n got %q\nwant %q", got, want.String())
	}
}

// publicOnlyControl is the connect-time check of the production dialer.
func TestPublicOnlyControl(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"8.8.8.8:443", true},
		{"[2606:4700:4700::1111]:443", true},
		{"[::ffff:8.8.8.8]:443", true},
		{"127.0.0.1:443", false},
		{"10.0.0.5:443", false},
		{"169.254.169.254:443", false},
		{"[::1]:443", false},
		{"[::ffff:10.0.0.5]:443", false},
		{"[2002:7f00:1::1]:443", false},
		{"[2001:0:4136:e378:8000:63bf:3fff:fdd2]:443", false},
		{"192.0.2.1:443", false},
		{"[fe80::1%eth0]:443", false},
		{"not-an-address", false},
		{"example.com:443", false},
	}
	for _, c := range cases {
		err := publicOnlyControl("tcp", c.addr, nil)
		if c.ok && err != nil {
			t.Errorf("publicOnlyControl(%s) = %v, want nil", c.addr, err)
		}
		if !c.ok && !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("publicOnlyControl(%s) = %v, want ErrBlockedAddress", c.addr, err)
		}
	}
}

// Without the DialContext seam, the production dialer re-checks the address
// at connect time: a loopback address that somehow got past the resolver
// check is still never connected to.
func TestProductionDialerRechecksAtConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	var s Sender // production configuration: no seams
	conn, err := s.dialVetted(context.Background(), "tcp", addrs(t, "127.0.0.1"), port)
	if err == nil {
		conn.Close()
		t.Fatal("the production dialer connected to a loopback address")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("dial error %v, want ErrBlockedAddress", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := accepted.Load(); n != 0 {
		t.Fatalf("listener accepted %d connection(s)", n)
	}
}

// The production client never uses a proxy (one would bypass the vetted
// dial, for example through HTTPS_PROXY) and never follows a redirect.
func TestProductionClientHasNoProxyAndNoRedirects(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	var s Sender
	c := s.client()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("the transport must not use a proxy")
	}
	if tr.DialContext == nil {
		t.Fatal("the transport must dial through the vetted dialer")
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects must not be followed")
	}
}

// Without the Sleep seam the backoff really waits, and gives up as soon as
// the delivery's context ends.
func TestDefaultSleepWaitsAndHonoursContext(t *testing.T) {
	var s Sender
	const d = 80 * time.Millisecond
	start := time.Now()
	if err := s.sleep(context.Background(), d); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if el := time.Since(start); el < d {
		t.Fatalf("sleep returned after %v, want at least %v", el, d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	if err := s.sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep on an ended context = %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("sleep on an ended context took %v", el)
	}
}

// ---- addresses in connection errors (v3 addendum) --------------------------

const publicIPv6 = "2606:4700:4700::1111"

// serverAddr stands in for the server's own address, which the local side
// of a connection error names.
var serverAddr = &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 51515}

// connAddrs are the address texts no delivery error may contain.
var connAddrs = []string{publicIP, "2606:4700", "10.1.2.3", "51515"}

func assertNoAddress(t *testing.T, where, s string, hide ...string) {
	t.Helper()
	for _, a := range append(append([]string{}, connAddrs...), hide...) {
		if strings.Contains(s, a) {
			t.Fatalf("%s carries the address %s: %q", where, a, s)
		}
	}
}

// last_error never contains a resolved address. A dial to a vetted address
// that is refused, and a connection the receiver resets during the TLS
// handshake, report their cause without the vetted address and without the
// server's own address.
func TestConnectionErrorsNeverCarryAddresses(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP, publicIPv6)
		h.sender.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			ta, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				return nil, err
			}
			return nil, &net.OpError{Op: "dial", Net: network, Source: serverAddr, Addr: ta, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}
		res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
		if res.Delivered || res.Attempts != 3 || !strings.Contains(res.Err, "dial tcp") || !strings.Contains(res.Err, "connect: ") {
			t.Fatalf("result %+v, want 3 attempts and the cause of the refusal", res)
		}
		assertNoAddress(t, "Result.Err", res.Err)
	})

	t.Run("reset", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 512)
				c.Read(buf) // the ClientHello
				c.(*net.TCPConn).SetLinger(0)
				c.Close() // RST
			}
		}()
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }, publicIP)
		h.sender.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		}
		res := h.sender.Send(context.Background(), testURL, FormatJSON, samplePayload())
		if res.Delivered || res.Attempts != 3 || res.Err == "" {
			t.Fatalf("result %+v, want 3 failed attempts", res)
		}
		// Both ends of this connection are 127.0.0.1; in production they are
		// the server's own address and the vetted address.
		assertNoAddress(t, "Result.Err", res.Err, "127.0.0.1")
	})
}

// The production dialer names the vetted address in its *net.OpError; the
// error dialVetted returns does not, and keeps the cause.
func TestProductionDialErrorCarriesNoAddress(t *testing.T) {
	var s Sender // production configuration: no seams
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := s.dialVetted(ctx, "tcp", addrs(t, publicIP, publicIPv6), "443")
	if err == nil {
		conn.Close()
		t.Fatal("dial on an ended context succeeded")
	}
	assertNoAddress(t, "dial error", err.Error())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error %v lost its cause", err)
	}
}

// opErrConn fails every operation with the *net.OpError a real connection
// returns, which names both ends of the connection.
type opErrConn struct{}

func (opErrConn) fail(op string) error {
	return &net.OpError{Op: op, Net: "tcp", Source: serverAddr,
		Addr: &net.TCPAddr{IP: net.ParseIP(publicIP), Port: 443}, Err: os.ErrDeadlineExceeded}
}
func (c opErrConn) Read([]byte) (int, error)         { return 0, c.fail("read") }
func (c opErrConn) Write([]byte) (int, error)        { return 0, c.fail("write") }
func (c opErrConn) Close() error                     { return c.fail("close") }
func (opErrConn) LocalAddr() net.Addr                { return serverAddr }
func (opErrConn) RemoteAddr() net.Addr               { return &net.TCPAddr{IP: net.ParseIP(publicIP), Port: 443} }
func (c opErrConn) SetDeadline(time.Time) error      { return c.fail("set") }
func (c opErrConn) SetReadDeadline(time.Time) error  { return c.fail("set") }
func (c opErrConn) SetWriteDeadline(time.Time) error { return c.fail("set") }

// Every error of a connection dialVetted returns is free of addresses, and
// keeps its operation, its cause and its timeout flag.
func TestConnectionOperationErrorsCarryNoAddress(t *testing.T) {
	s := &Sender{DialContext: func(context.Context, string, string) (net.Conn, error) { return opErrConn{}, nil }}
	conn, err := s.dialVetted(context.Background(), "tcp", addrs(t, publicIP), "443")
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := conn.Read(make([]byte, 1))
	_, werr := conn.Write([]byte("x"))
	ops := map[string]error{
		"read": rerr, "write": werr, "close": conn.Close(),
		"set": conn.SetDeadline(time.Now()), "set read": conn.SetReadDeadline(time.Now()), "set write": conn.SetWriteDeadline(time.Now()),
	}
	for name, err := range ops {
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		assertNoAddress(t, name+" error", err.Error())
		var ne net.Error
		if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("%s error %v lost its cause or its timeout flag", name, err)
		}
		if op := strings.SplitN(name, " ", 2)[0]; !strings.HasPrefix(err.Error(), op+" tcp: ") {
			t.Fatalf("%s error %q, want it to start with %q", name, err, op+" tcp: ")
		}
	}
}

// clean is the last guard: an IP address, or an address with its port, that
// reached the error text some other way is removed. Text that only looks
// like an address is kept.
func TestCleanDropsAddresses(t *testing.T) {
	u, _ := url.Parse(testURL)
	cases := map[string]string{
		"read tcp 10.1.2.3:51515->93.184.216.34:443: connection reset":              "connection reset",
		"dial tcp [2606:4700:4700::1111]:443: connect: connection refused":          "connection refused",
		"dial tcp 2606:4700:4700::1111: connect: no route to host":                  "no route to host",
		"x509: certificate is valid for 10.0.0.7, ::ffff:10.0.0.8, not example.com": "certificate is valid for",
		"x509: certificate is valid for abcd::ef, not example.com":                  "not example.com",
	}
	for msg, keep := range cases {
		got := clean(errors.New(msg), u)
		assertNoAddress(t, "clean", got, "10.0.0.7", "10.0.0.8", "abcd::ef")
		if !strings.Contains(got, keep) {
			t.Fatalf("clean(%q) = %q, lost %q", msg, got, keep)
		}
	}
	const plain = "receiver returned HTTP 503; x509: deadbeef; at 12:34:56; v1.2.3; ::"
	if got := clean(errors.New(plain), u); got != plain {
		t.Fatalf("clean(%q) = %q, want it unchanged", plain, got)
	}
}

// The URL parts are removed both before and after the control characters,
// so neither a decoded path that holds one nor a control character inserted
// into a URL part lets the part through.
func TestCleanRemovesURLAroundControlCharacters(t *testing.T) {
	u, _ := url.Parse("https://" + testHost + "/services/T0/B0/PATH%0ASECRET")
	if got := clean(errors.New("receiver said: /services/T0/B0/PATH\nSECRET was refused"), u); strings.Contains(got, "SECRET") || strings.Contains(got, "/services/") || !strings.Contains(got, "was refused") {
		t.Fatalf("decoded path with a control character: %q", got)
	}
	u, _ = url.Parse(testURL)
	if got := clean(errors.New("refused: /services/T000/B000/PATH\x00SECRET"), u); strings.Contains(got, "PATHSECRET") || strings.Contains(got, "/services/") {
		t.Fatalf("path split by a control character: %q", got)
	}
}
