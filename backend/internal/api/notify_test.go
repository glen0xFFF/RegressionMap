package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"regressionmap/internal/model"
	"regressionmap/internal/notify"
	"regressionmap/internal/store"
)

const (
	testWebhookSecret = "0123456789abcdef0123456789abcdef"
	// Path and query carry the secrets of a Slack-style webhook URL.
	secretNotifyURL = "https://hooks.example.com/services/T000/B000/PATHSECRET?token=QUERYSECRET"
)

var urlLeaks = []string{"PATHSECRET", "QUERYSECRET", "/services/", "token="}

func assertNoLeak(t *testing.T, where, s string) {
	t.Helper()
	for _, l := range urlLeaks {
		if strings.Contains(s, l) {
			t.Fatalf("%s contains %q:\n%s", where, l, s)
		}
	}
}

type fakeCall struct {
	url, format string
	payload     notify.Payload
}

// fakeNotifier records deliveries. With block set, Send waits until
// release is closed, so tests can observe the pending state.
type fakeNotifier struct {
	mu      sync.Mutex
	calls   []fakeCall
	block   bool
	release chan struct{}
	result  notify.Result
}

func newFake(block bool, res notify.Result) *fakeNotifier {
	return &fakeNotifier{block: block, release: make(chan struct{}), result: res}
}

func (f *fakeNotifier) Send(ctx context.Context, raw, format string, p notify.Payload) notify.Result {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{raw, format, p})
	f.mu.Unlock()
	if f.block {
		<-f.release
	}
	return f.result
}

func (f *fakeNotifier) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall{}, f.calls...)
}

func notifyBody(repo, base, cmd, url, format string) map[string]any {
	b := startBody(repo, base, cmd)
	b["notify_url"] = url
	if format != "" {
		b["notify_format"] = format
	}
	return b
}

func rawDo(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()
	var rd io.Reader = bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// captureLog redirects the standard logger for the test's duration.
func captureLog(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitNotification(t *testing.T, url, id, status string) Analysis {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var a Analysis
		do(t, "GET", url+"/api/analyses/"+id, nil, &a)
		if a.Notification != nil && a.Notification.Status == status {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("notification of %s never reached %q", id, status)
	return Analysis{}
}

func TestNotifyRejectedWhenWebhooksDisabled(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
	code, body := rawDo(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, ""))
	var er apiErr
	json.Unmarshal([]byte(body), &er)
	if code != 400 || er.Error.Code != "webhooks_disabled" {
		t.Fatalf("%d %s", code, body)
	}
	assertNoLeak(t, "error body", body)

	// Without notify_url nothing changes, and notification is null.
	code, body = rawDo(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"))
	if code != 202 || !strings.Contains(body, `"notification":null`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestNotifyRequestValidation(t *testing.T) {
	fake := newFake(false, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	base := shas["r1"].Baseline
	cases := []struct {
		url, format, code string
	}{
		{"http://hooks.example.com/services/PATHSECRET", "", "invalid_notify_url"},
		{"https://user:PATHSECRET@hooks.example.com/x", "", "invalid_notify_url"},
		{"https://hooks.example.com:8443/services/PATHSECRET", "", "invalid_notify_url"},
		{"https://hooks.example.com/" + strings.Repeat("a", 2049), "", "invalid_notify_url"},
		{"not a url /services/PATHSECRET", "", "invalid_notify_url"},
		{secretNotifyURL, "xml", "bad_request"},
		{secretNotifyURL, "JSON", "bad_request"},
	}
	for _, c := range cases {
		code, body := rawDo(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", base, "slow", c.url, c.format))
		var er apiErr
		json.Unmarshal([]byte(body), &er)
		if code != 400 || er.Error.Code != c.code {
			t.Errorf("%.50q format=%q: %d %s, want 400 %s", c.url, c.format, code, body, c.code)
		}
		assertNoLeak(t, "error body", body)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("rejected requests must not notify, got %d calls", n)
	}
}

// A notify_url whose host holds a control, format or space character, raw
// or percent-encoded, or is not valid UTF-8, is refused as invalid_notify_url
// before anything is stored or sent, so such a host never reaches
// notification.host, DATA_DIR or the log.
func TestNotifyURLHostWithControlOrFormatCharacterIsRefused(t *testing.T) {
	data := t.TempDir()
	fake := newFake(false, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	for _, u := range []string{
		"https://h\u0085ooks.example.com/x",
		"https://h%C2%85ooks.example.com/x",
		"https://h\u009b2Kooks.example.com/x",
		"https://h%E2%80%AEooks.example.com/x",
		"https://h%FFooks.example.com/x",
	} {
		code, body := rawDo(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", u, "json"))
		var er apiErr
		json.Unmarshal([]byte(body), &er)
		if code != 400 || er.Error.Code != "invalid_notify_url" || er.Error.Message != "notify_url is not a valid URL" {
			t.Errorf("%+q: %d %s, want 400 invalid_notify_url", u, code, body)
		}
	}
	if ids, err := store.New(data).List(); err != nil || len(ids) != 0 {
		t.Fatalf("refused requests stored %d analysis(es) (%v)", len(ids), err)
	}
	var list struct{ Analyses []Summary }
	if code := do(t, "GET", srv.URL+"/api/analyses", nil, &list); code != 200 || len(list.Analyses) != 0 {
		t.Fatalf("GET /api/analyses after refused requests: %d %+v", code, list.Analyses)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("refused requests must not notify, got %d calls", n)
	}
}

func TestNotifyDeliveredAsyncAndURLNeverExposed(t *testing.T) {
	logs := captureLog(t)
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	released := false
	defer func() {
		if !released {
			close(fake.release)
		}
	}()

	code, body := rawDo(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"))
	if code != 202 {
		t.Fatalf("POST %d %s", code, body)
	}
	assertNoLeak(t, "202 body", body)
	var a Analysis
	json.Unmarshal([]byte(body), &a)
	if n := a.Notification; n == nil || n.Host != "hooks.example.com" || n.Format != "json" || n.Status != "pending" || n.Attempts != 0 {
		t.Fatalf("initial notification %+v", a.Notification)
	}

	// The job completes while the delivery is still blocked.
	done := pollDone(t, srv.URL, a.ID)
	if done.Status != "done" {
		t.Fatalf("status %s %s", done.Status, done.Error)
	}
	if done.Notification == nil || done.Notification.Status != "pending" {
		t.Fatalf("notification while delivering %+v", done.Notification)
	}
	fin, err := time.Parse(time.RFC3339, done.FinishedAt)
	if err != nil || fin.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("finished_at %q: %v", done.FinishedAt, err)
	}
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), nil); code != 202 {
		t.Fatalf("repo slot not freed while delivering: %d", code)
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(fake.Calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
	c := calls[0]
	if c.url != secretNotifyURL || c.format != "json" {
		t.Fatalf("call url/format %q %q", c.url, c.format)
	}
	p := c.payload
	if p.ID != a.ID || p.Repo != "r1" || p.Status != "done" || p.Error != "" || p.Baseline != done.Baseline || p.Current != done.Current ||
		!p.BaselineRan || !p.CurrentRan || p.BaselinePassed != done.BaselineRun.Passed || p.CurrentPassed != done.CurrentRun.Passed ||
		p.ModelStatus != "disabled" || p.TopHypothesis != nil || p.CreatedAt != done.CreatedAt || p.FinishedAt != done.FinishedAt {
		t.Fatalf("payload %+v\nanalysis %+v", p, done)
	}

	released = true
	close(fake.release)
	got := waitNotification(t, srv.URL, a.ID, "delivered")
	if n := got.Notification; n.Attempts != 1 || n.LastStatusCode != 200 || n.LastError != "" || n.Host != "hooks.example.com" {
		t.Fatalf("delivered notification %+v", n)
	}
	waitIdle(t, s)

	_, getBody := rawDo(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil)
	assertNoLeak(t, "GET body", getBody)
	_, listBody := rawDo(t, "GET", srv.URL+"/api/analyses", nil)
	assertNoLeak(t, "GET list body", listBody)

	raw, err := os.ReadFile(filepath.Join(data, "analyses", a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "persisted file", string(raw))
	if !strings.Contains(string(raw), `"status":"delivered"`) || !strings.Contains(string(raw), `"finished_at"`) {
		t.Fatalf("persisted file lacks the delivery status: %s", raw)
	}
	assertNoLeak(t, "log", logs.String())

	s.mu.Lock()
	left := len(s.notifyURLs)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("notify URL kept in memory after delivery (%d)", left)
	}
}

func TestNotifyFailureIsRecorded(t *testing.T) {
	logs := captureLog(t)
	data := t.TempDir()
	fake := newFake(false, notify.Result{Attempts: 3, StatusCode: 503, Err: "receiver returned HTTP 503"})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "slack"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	got := waitNotification(t, srv.URL, a.ID, "failed")
	if n := got.Notification; n.Attempts != 3 || n.LastStatusCode != 503 || n.LastError != "receiver returned HTTP 503" || n.Format != "slack" {
		t.Fatalf("failed notification %+v", n)
	}
	waitIdle(t, s)
	st, ok := loadStoredAnalysis(t, data, a.ID)
	if !ok || st.Notification == nil || st.Notification.Status != "failed" {
		t.Fatalf("persisted %+v", st.Notification)
	}
	assertNoLeak(t, "log", logs.String())
	if !strings.Contains(logs.String(), "hooks.example.com") {
		t.Fatalf("the failure log should name the host: %s", logs.String())
	}
}

func TestNotifyAfterErrorStatusExactlyOnce(t *testing.T) {
	fake := newFake(false, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(20000)}
		c.JobTimeout = 300 * time.Millisecond
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, ""), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	if a.Notification == nil || a.Notification.Format != "json" {
		t.Fatalf("default format: %+v", a.Notification)
	}
	got := waitNotification(t, srv.URL, a.ID, "delivered")
	if got.Status != "error" || got.FinishedAt == "" {
		t.Fatalf("%s %q", got.Status, got.FinishedAt)
	}
	waitIdle(t, s)
	time.Sleep(200 * time.Millisecond)
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("delivered %d times, want once", len(calls))
	}
	if p := calls[0].payload; p.Status != "error" || p.Error != msgTimedOut || p.FinishedAt != got.FinishedAt {
		t.Fatalf("payload %+v", p)
	}
}

// tlsReceiver is a TLS webhook receiver plus a real notify.Sender whose
// resolver maps the receiver's certificate host (example.com) to a public
// address. The injected dialer is called only with an address that passed
// the public-IP check, and routes it to the local receiver. (The TEST-NET
// ranges are not public under the v3 addendum, so they cannot stand in.)
type tlsReceiver struct {
	srv    *httptest.Server
	sender *notify.Sender
	mu     sync.Mutex
	bodies [][]byte
	sigs   []string
}

func newTLSReceiver(t *testing.T) *tlsReceiver {
	t.Helper()
	r := &tlsReceiver{}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, b)
		r.sigs = append(r.sigs, req.Header.Get("X-RegressionMap-Signature"))
		r.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(r.srv.Close)
	vetted := netip.MustParseAddr("93.184.216.34")
	r.sender = &notify.Sender{
		Secret:   []byte(testWebhookSecret),
		Resolver: staticResolver{"example.com": {vetted}},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "93.184.216.34:443" {
				t.Errorf("dial to unexpected address %s", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", r.srv.Listener.Addr().String())
		},
		TLSConfig: r.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
	}
	return r
}

func (r *tlsReceiver) received() ([][]byte, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte{}, r.bodies...), append([]string{}, r.sigs...)
}

type staticResolver map[string][]netip.Addr

func (m staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := m[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// Integration: the real Sender over TLS, a real fixture analysis with model
// hypotheses, both formats.
func TestNotifyIntegrationWithRealSender(t *testing.T) {
	logs := captureLog(t)
	fakeModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"summary":"s","limitations":[],"hypotheses":[{"rank":1,"title":"end index off by one","file":"src/pagination.ts","line_start":11,"line_end":11,"reason":"r","supporting_evidence_ids":["E1"],"counter_evidence_ids":[],"verification_suggestion":"rerun","confidence_label":"high"}]}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer fakeModel.Close()
	rcv := newTLSReceiver(t)
	data := t.TempDir()
	s, srv, _, shas := newServer(t, []string{"pagination-demo"}, func(c *Config) {
		c.DataDir = data
		c.Model = &model.Client{BaseURL: fakeModel.URL, APIKey: "k", Model: "muse-spark-1.3"}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = rcv.sender
	})
	const hookURL = "https://example.com/services/T000/B000/PATHSECRET?token=QUERYSECRET"

	// slack
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("pagination-demo", shas["pagination-demo"].Baseline, "node-test", hookURL, "slack"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	got := waitNotification(t, srv.URL, a.ID, "delivered")
	if got.Model == nil || got.Model.Status != "ok" || len(got.Evidence) == 0 {
		t.Fatalf("analysis %+v", got.Model)
	}
	bodies, sigs := rcv.received()
	if len(bodies) != 1 || sigs[0] != "" {
		t.Fatalf("slack deliveries %d sig %q", len(bodies), sigs)
	}
	var msg struct{ Text string }
	if err := json.Unmarshal(bodies[0], &msg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pagination-demo", "Baseline PASS", "HEAD FAIL", "end index off by one", "src/pagination.ts:11", "confidence high"} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("slack text lacks %q: %s", want, msg.Text)
		}
	}
	if !strings.HasSuffix(msg.Text, "AI-generated hypothesis and review, not verified.") {
		t.Fatalf("slack text: %s", msg.Text)
	}
	for _, ev := range got.Evidence {
		for _, line := range strings.Split(ev.Excerpt, "\n") {
			if line = strings.TrimSpace(line); len(line) >= 12 && strings.Contains(msg.Text, line) {
				t.Fatalf("slack text leaks evidence %s excerpt line %q", ev.ID, line)
			}
		}
	}
	waitIdle(t, s)

	// json
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("pagination-demo", shas["pagination-demo"].Baseline, "node-test", hookURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	got = waitNotification(t, srv.URL, a.ID, "delivered")
	bodies, sigs = rcv.received()
	if len(bodies) != 2 {
		t.Fatalf("deliveries %d", len(bodies))
	}
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write(bodies[1])
	if sigs[1] != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature %q does not verify", sigs[1])
	}
	var p map[string]any
	if err := json.Unmarshal(bodies[1], &p); err != nil {
		t.Fatal(err)
	}
	top, _ := p["top_hypothesis"].(map[string]any)
	if p["event"] != "analysis.completed" || p["id"] != a.ID || p["status"] != "done" || p["finished_at"] != got.FinishedAt ||
		p["baseline_passed"] != true || p["current_passed"] != false || p["model_status"] != "ok" ||
		top == nil || top["title"] != "end index off by one" || top["line_start"] != float64(11) {
		t.Fatalf("json payload %s", bodies[1])
	}
	waitIdle(t, s)
	for _, id := range []string{a.ID} {
		raw, _ := os.ReadFile(filepath.Join(data, "analyses", id+".json"))
		assertNoLeak(t, "persisted file", string(raw))
	}
	assertNoLeak(t, "log", logs.String())
}

// A delivery cannot survive a restart (the URL was in memory only), so a
// stored pending notification is reported as failed.
func TestStoredPendingNotificationIsReportedFailed(t *testing.T) {
	data := t.TempDir()
	id := store.NewID()
	b, _ := json.Marshal(Analysis{ID: id, Status: "done", Repo: "r1", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Notification: &Notification{Host: "hooks.example.com", Format: "json", Status: "pending"}})
	if err := store.New(data).Save(id, b); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{Root: t.TempDir(), Model: &model.Client{}, DataDir: data}))
	defer srv.Close()
	var a Analysis
	if code := do(t, "GET", srv.URL+"/api/analyses/"+id, nil, &a); code != 200 || a.Notification == nil ||
		a.Notification.Status != "failed" || !strings.Contains(a.Notification.LastError, "interrupted") {
		t.Fatalf("%d %+v", code, a.Notification)
	}
}

// ---- v3 addendum ---------------------------------------------------------

// releaser returns a func that unblocks f once. The test's cleanup calls it
// too, so a failing test never leaves a delivery blocked.
func releaser(t *testing.T, f *fakeNotifier) func() {
	var once sync.Once
	rel := func() { once.Do(func() { close(f.release) }) }
	t.Cleanup(rel)
	return rel
}

func waitCalls(t *testing.T, f *fakeNotifier, n int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for len(f.Calls()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("notifier called %d time(s), want %d", len(f.Calls()), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// captureJSONLog makes a JSON slog handler the process default for the
// test, as main does with LOG_FORMAT=json, and restores the previous slog
// and log package settings afterwards. Call it before newServer, so the
// server is idle before the settings are restored.
func captureJSONLog(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	prev, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf
}

// jsonLines parses every log line and fails on one that is not JSON.
func jsonLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON (%v): %q", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func assertNoControl(t *testing.T, where, s string) {
	t.Helper()
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("%s keeps control character %U: %q", where, r, s)
		}
	}
}

// goroutineBlocked reports whether a goroutine whose stack contains every
// marker is parked on a sync.Mutex.
func goroutineBlocked(markers ...string) bool {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if !strings.Contains(g, "[sync.Mutex.Lock") && !strings.Contains(g, "lockSlow") {
			continue
		}
		all := true
		for _, m := range markers {
			all = all && strings.Contains(g, m)
		}
		if all {
			return true
		}
	}
	return false
}

func waitBlocked(t *testing.T, what string, markers ...string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !goroutineBlocked(markers...) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never blocked on a mutex", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// POST /api/analyses checks the notification fields in the addendum's
// order: webhooks_disabled, then invalid_notify_url, then bad_request for
// notify_format, with or without notify_url.
func TestNotifyRequestCheckOrder(t *testing.T) {
	_, off, _, offShas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
	fake := newFake(false, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	_, on, _, onShas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	body := func(base, url, format string) map[string]any {
		b := startBody("r1", base, "slow")
		if url != "" {
			b["notify_url"] = url
		}
		if format != "" {
			b["notify_format"] = format
		}
		return b
	}
	cases := []struct {
		name, srv, base, url, format, code string
	}{
		{"disabled, bad format", off.URL, offShas["r1"].Baseline, secretNotifyURL, "xml", "webhooks_disabled"},
		{"disabled, bad URL, bad format", off.URL, offShas["r1"].Baseline, "http://hooks.example.com/services/PATHSECRET", "xml", "webhooks_disabled"},
		{"disabled, no URL, bad format", off.URL, offShas["r1"].Baseline, "", "xml", "bad_request"},
		{"bad URL, bad format", on.URL, onShas["r1"].Baseline, "http://hooks.example.com/services/PATHSECRET", "xml", "invalid_notify_url"},
		{"valid URL, bad format", on.URL, onShas["r1"].Baseline, secretNotifyURL, "xml", "bad_request"},
		{"no URL, bad format", on.URL, onShas["r1"].Baseline, "", "Slack", "bad_request"},
	}
	for _, c := range cases {
		code, resp := rawDo(t, "POST", c.srv+"/api/analyses", body(c.base, c.url, c.format))
		var er apiErr
		json.Unmarshal([]byte(resp), &er)
		if code != 400 || er.Error.Code != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, code, resp, c.code)
		}
		assertNoLeak(t, "error body", resp)
	}
	// A valid notify_format without notify_url is accepted and ignored.
	code, resp := rawDo(t, "POST", on.URL+"/api/analyses", body(onShas["r1"].Baseline, "", "slack"))
	if code != 202 || !strings.Contains(resp, `"notification":null`) {
		t.Fatalf("format without URL: %d %s", code, resp)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("no notification was requested, got %d calls", n)
	}
}

// MaxJobs=1: a second analysis evicts A from memory while A's delivery is
// still in flight. GET keeps showing "pending", because this process is
// still delivering, and the real outcome is recorded when it ends (through
// the load of the evicted analysis from DATA_DIR).
func TestNotifyPendingWhileDeliveringAfterEviction(t *testing.T) {
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.DataDir = data
		c.MaxJobs = 1
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	release := releaser(t, fake)
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST A %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r2", shas["r2"].Baseline, "slow"), nil); code != 202 {
		t.Fatalf("POST B %d", code)
	}
	s.mu.Lock()
	_, inMemory := s.jobs[a.ID]
	s.mu.Unlock()
	if inMemory {
		t.Fatal("fixture: A must have been evicted by B")
	}
	var got Analysis
	if code := do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got); code != 200 {
		t.Fatalf("GET A %d", code)
	}
	if n := got.Notification; n == nil || n.Status != "pending" || n.LastError != "" {
		t.Fatalf("in-flight delivery of an evicted analysis shown as %+v, want pending", got.Notification)
	}

	release()
	fin := waitNotification(t, srv.URL, a.ID, "delivered")
	if n := fin.Notification; n.Attempts != 1 || n.LastStatusCode != 200 || n.LastError != "" {
		t.Fatalf("delivered notification %+v", n)
	}
	waitIdle(t, s)
	if st, ok := loadStoredAnalysis(t, data, a.ID); !ok || st.Notification == nil || st.Notification.Status != "delivered" {
		t.Fatalf("stored notification %+v", st.Notification)
	}
}

// A GET of an evicted analysis reads the stored copy, "pending", while the
// delivery is in flight; the delivery then records "delivered" and leaves
// the in-flight set before the GET decides what that "pending" means. The
// seam, loadStoredRead, wraps the real store read and holds the GET inside
// its read of the stored file, after the bytes ("pending") are read and
// before the read returns, until the delivery has recorded "delivered" and
// left the in-flight set. This proves that loadStored takes its in-flight
// snapshot before the read: the GET then serves "pending" or "delivered",
// never a delivered webhook as "failed" with msgDeliveryInterrupted. A
// snapshot taken at any point after the read returns, inside the "pending"
// check as at 2741b38 or just after the read, sees the delivery ended and
// serves "failed".
func TestGetWhileDeliveryEndsNeverShowsItInterrupted(t *testing.T) {
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.DataDir = data
		c.MaxJobs = 1
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	release := releaser(t, fake)
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST A %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)
	var b Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r2", shas["r2"].Baseline, "slow"), &b); code != 202 {
		t.Fatalf("POST B %d", code)
	}
	pollDone(t, srv.URL, b.ID)
	s.mu.Lock()
	_, inMemory := s.jobs[a.ID]
	s.mu.Unlock()
	if inMemory {
		t.Fatal("fixture: A must have been evicted by B")
	}
	if st, ok := loadStoredAnalysis(t, data, a.ID); !ok || st.Notification == nil || st.Notification.Status != "pending" {
		t.Fatalf("fixture: the GET must read a stored \"pending\", stored %+v", st.Notification)
	}

	// The first read of A's stored file from here on (the GET's) reads the
	// bytes through the real store and then waits, before it returns them,
	// until resume is closed; later reads (deliver's own load, later GETs)
	// go on.
	paused, resume := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	s.loadStoredRead = func(id string) ([]byte, error) {
		b, err := s.store.Load(id)
		if id == a.ID && reads.Add(1) == 1 {
			close(paused)
			<-resume
		}
		return b, err
	}
	var resumed sync.Once
	resumeGET := func() { resumed.Do(func() { close(resume) }) }
	t.Cleanup(resumeGET)

	type getResult struct {
		a    Analysis
		code int
		err  error
	}
	got := make(chan getResult, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/api/analyses/" + a.ID)
		if err != nil {
			got <- getResult{err: err}
			return
		}
		defer resp.Body.Close()
		r := getResult{code: resp.StatusCode}
		r.err = json.NewDecoder(resp.Body).Decode(&r.a)
		got <- r
	}()
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		t.Fatal("the GET never reached loadStored")
	}
	release() // the receiver answered 200
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		_, inFlight := s.delivering[a.ID]
		s.mu.Unlock()
		if !inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery never left the in-flight set")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st, ok := loadStoredAnalysis(t, data, a.ID); !ok || st.Notification == nil || st.Notification.Status != "delivered" {
		t.Fatalf("fixture: the outcome must be recorded before the GET goes on, stored %+v", st.Notification)
	}
	resumeGET()
	var r getResult
	select {
	case r = <-got:
	case <-time.After(30 * time.Second):
		t.Fatal("the GET never answered")
	}
	if r.err != nil || r.code != 200 {
		t.Fatalf("GET A: %d %v", r.code, r.err)
	}
	if n := r.a.Notification; n == nil || (n.Status != "pending" && n.Status != "delivered") || n.LastError != "" {
		t.Fatalf("a GET that read the stored copy while the delivery was in flight served %+v, want pending or delivered", r.a.Notification)
	}
	t.Logf("the held GET served %q", r.a.Notification.Status)
	fin := waitNotification(t, srv.URL, a.ID, "delivered")
	if n := fin.Notification; n.Attempts != 1 || n.LastStatusCode != 200 || n.LastError != "" {
		t.Fatalf("a later GET served %+v, want delivered", n)
	}
}

// The analysis leaves memory between the end of its delivery and the
// recording of the outcome: create, queued on saveMu behind a persist in
// progress, evicts it (MaxJobs=1) just before deliver records. The outcome
// is still recorded, in DATA_DIR and in what GET serves.
func TestDeliveryOutcomeRecordedWhenEvictedWhileRecording(t *testing.T) {
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.DataDir = data
		c.MaxJobs = 1
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	release := releaser(t, fake)
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST A %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)

	s.saveMu.Lock() // a persist in progress elsewhere
	locked := true
	defer func() {
		if locked {
			s.saveMu.Unlock()
		}
	}()
	bDone := make(chan int, 1)
	go func() {
		var b Analysis
		bDone <- do(t, "POST", srv.URL+"/api/analyses", startBody("r2", shas["r2"].Baseline, "slow"), &b)
	}()
	waitBlocked(t, "create", "(*Server).create")
	release() // the receiver answered 200
	waitBlocked(t, "deliver", "(*Server).deliver")
	locked = false
	s.saveMu.Unlock() // create runs first and evicts A, then deliver records
	if code := <-bDone; code != 202 {
		t.Fatalf("POST B %d", code)
	}
	waitIdle(t, s)

	var got Analysis
	do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got)
	if n := got.Notification; n == nil || n.Status != "delivered" || n.Attempts != 1 || n.LastStatusCode != 200 {
		t.Fatalf("GET serves %+v after a successful delivery", got.Notification)
	}
	if st, ok := loadStoredAnalysis(t, data, a.ID); !ok || st.Notification == nil || st.Notification.Status != "delivered" {
		t.Fatalf("stored notification %+v after a successful delivery", st.Notification)
	}
}

// A verification takes its snapshot of an evicted analysis while the
// delivery is in flight; the delivery records "delivered" while the
// verification command runs. The verification must not write its stale
// snapshot back over the recorded outcome.
func TestVerifyKeepsDeliveryOutcomeRecordedMeanwhile(t *testing.T) {
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.DataDir = data
		c.MaxJobs = 1
		c.Commands = []Command{slowCmd(50), slowCmd(20000), slowCmd(2500)}
		c.Commands[1].ID = "stuck"
		c.Commands[2].ID = "check"
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	release := releaser(t, fake)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.Drain(ctx) // ends the stuck analysis
	}()
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST A %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)
	// B keeps running, so A cannot stay in memory once re-inserted.
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r2", shas["r2"].Baseline, "stuck"), nil); code != 202 {
		t.Fatalf("POST B %d", code)
	}

	verified := make(chan int, 1)
	go func() {
		verified <- do(t, "POST", srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "check", "approved": true}, nil)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		busy := s.verifying["r1"]
		s.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the verification never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	for {
		if st, ok := loadStoredAnalysis(t, data, a.ID); ok && st.Notification != nil && st.Notification.Status == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the delivery outcome was never recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	stillVerifying := s.verifying["r1"]
	s.mu.Unlock()
	if !stillVerifying {
		t.Fatal("fixture: the verification ended before the delivery was recorded")
	}
	if code := <-verified; code != 200 {
		t.Fatalf("verify %d", code)
	}

	var got Analysis
	do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got)
	if n := got.Notification; n == nil || n.Status != "delivered" || len(got.Verifications) != 1 {
		t.Fatalf("after the verification: notification %+v, %d verification(s)", got.Notification, len(got.Verifications))
	}
	if st, _ := loadStoredAnalysis(t, data, a.ID); st.Notification == nil || st.Notification.Status != "delivered" || len(st.Verifications) != 1 {
		t.Fatalf("stored after the verification: notification %+v, %d verification(s)", st.Notification, len(st.Verifications))
	}
}

// With LOG_FORMAT=json every log line is JSON. A failed delivery logs
// exactly one record with its fields; text from the receiver can neither
// add a record nor carry control characters, and the URL never appears.
func TestNotifyFailureLogsOneJSONLine(t *testing.T) {
	logs := captureJSONLog(t)
	data := t.TempDir()
	forged := `{"time":"2026-11-04T00:00:00Z","level":"INFO","msg":"http","method":"DELETE","path":"/api/keys/k_forged","status":204}`
	errText := "x509: certificate is valid for evil.example\n" + forged + "\r\n\x1b[0m\u0085 while posting to " + secretNotifyURL
	fake := newFake(false, notify.Result{Attempts: 3, StatusCode: 0, Err: errText})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	got := waitNotification(t, srv.URL, a.ID, "failed")
	waitIdle(t, s)

	wantErr := "x509: certificate is valid for evil.example" + forged + "[0m while posting to "
	if n := got.Notification; n.LastError != wantErr || n.Attempts != 3 {
		t.Fatalf("last_error %q attempts %d, want %q", n.LastError, n.Attempts, wantErr)
	}
	assertNoControl(t, "last_error", got.Notification.LastError)
	assertNoLeak(t, "last_error", got.Notification.LastError)

	recs := jsonLines(t, logs.String())
	if len(recs) != 1 {
		t.Fatalf("want exactly one log line, got %d:\n%s", len(recs), logs.String())
	}
	r := recs[0]
	if r["level"] != "WARN" || r["msg"] != "notification failed" || r["analysis"] != a.ID || r["host"] != "hooks.example.com" ||
		r["attempts"] != float64(3) || r["status_code"] != float64(0) || r["error"] != wantErr {
		t.Fatalf("log record %v", r)
	}
	assertNoLeak(t, "log", logs.String())
}

// scrubURL is the second guard behind the Notifier: whatever error text a
// Notifier returns, the URL, its path and its query never reach last_error,
// DATA_DIR or the log.
func TestNotifyErrorWithURLIsScrubbed(t *testing.T) {
	logs := captureJSONLog(t)
	data := t.TempDir()
	errText := `Post "` + secretNotifyURL + `": refused; path /services/T000/B000/PATHSECRET; query token=QUERYSECRET`
	fake := newFake(false, notify.Result{Attempts: 1, Err: errText})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	waitNotification(t, srv.URL, a.ID, "failed")
	waitIdle(t, s)
	_, body := rawDo(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil)
	assertNoLeak(t, "GET body", body)
	if !strings.Contains(body, "refused") {
		t.Fatalf("the rest of the error text is kept: %s", body)
	}
	raw, err := os.ReadFile(filepath.Join(data, "analyses", a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "persisted file", string(raw))
	assertNoLeak(t, "log", logs.String())
}

func TestScrubURL(t *testing.T) {
	msg := `Post "` + secretNotifyURL + "\": refused\n; path /services/T000/B000/PATHSECRET; query token=QUERYSECRET\x1b"
	got := scrubURL(msg, secretNotifyURL)
	assertNoLeak(t, "scrubURL", got)
	assertNoControl(t, "scrubURL", got)
	if !strings.Contains(got, "refused") {
		t.Fatalf("scrubURL lost the text: %q", got)
	}
	if n := utf8.RuneCountInString(scrubURL(strings.Repeat("é", 600), secretNotifyURL)); n != 500 {
		t.Fatalf("scrubURL keeps %d characters, want 500", n)
	}
	// The URL parts are removed before and after the control characters: a
	// decoded path can hold one, and one inserted into a part would
	// otherwise hide it.
	got = scrubURL("receiver said: /services/T0/B0/PATH\nSECRET was refused", "https://hooks.example.com/services/T0/B0/PATH%0ASECRET")
	if strings.Contains(got, "SECRET") || strings.Contains(got, "/services/") || !strings.Contains(got, "was refused") {
		t.Fatalf("decoded path with a control character: %q", got)
	}
	got = scrubURL("refused: /services/T000/B000/PATH\x00SECRET", secretNotifyURL)
	assertNoLeak(t, "scrubURL of a path split by a control character", got)
}

// last_error never contains a resolved address: with the real Sender, a
// receiver that resets the connection during the TLS handshake gives an
// error without either end of the connection, in GET, DATA_DIR and the log.
func TestNotifyConnectionErrorCarriesNoAddress(t *testing.T) {
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
	logs := captureJSONLog(t)
	data := t.TempDir()
	sender := &notify.Sender{
		Secret:   []byte(testWebhookSecret),
		Resolver: staticResolver{"hooks.example.com": {netip.MustParseAddr("93.184.216.34")}},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = sender
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	got := waitNotification(t, srv.URL, a.ID, "failed")
	waitIdle(t, s)
	if n := got.Notification; n.Attempts != 3 || n.LastError == "" {
		t.Fatalf("notification %+v, want 3 failed attempts", n)
	}
	raw, err := os.ReadFile(filepath.Join(data, "analyses", a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// Both ends of this connection are 127.0.0.1; in production they are
	// the server's own address and the vetted address.
	for where, text := range map[string]string{"last_error": got.Notification.LastError, "persisted file": string(raw), "log": logs.String()} {
		for _, addr := range []string{"127.0.0.1", "93.184.216.34", ln.Addr().String()} {
			if strings.Contains(text, addr) {
				t.Fatalf("%s carries the address %s:\n%s", where, addr, text)
			}
		}
	}
}

// panicTransport panics on every request, so the model step of an
// analysis panics with a value the test chooses.
type panicTransport struct{ value string }

func (p panicTransport) RoundTrip(*http.Request) (*http.Response, error) { panic(p.value) }

// A panicking analysis ends as error with "internal error", and its panic
// is logged as one JSON record whose text has no control characters.
func TestAnalysisPanicIsLoggedWithoutControlCharacters(t *testing.T) {
	logs := captureJSONLog(t)
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.Model = &model.Client{BaseURL: "http://model.invalid", APIKey: "k", Model: "m",
			HTTP: &http.Client{Transport: panicTransport{"boom\n{\"level\":\"INFO\",\"msg\":\"forged\"}\r\x1b[0m\u0085end"}}}
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	if got := pollDone(t, srv.URL, a.ID); got.Status != "error" || got.Error != "internal error" {
		t.Fatalf("after a panic: status %q error %q", got.Status, got.Error)
	}
	waitIdle(t, s)
	var panics []map[string]any
	for _, r := range jsonLines(t, logs.String()) {
		if r["msg"] == "analysis panicked" {
			panics = append(panics, r)
		}
		if r["msg"] == "forged" {
			t.Fatalf("the panic text forged a log record:\n%s", logs.String())
		}
	}
	const want = `boom{"level":"INFO","msg":"forged"}[0mend`
	if len(panics) != 1 || panics[0]["level"] != "ERROR" || panics[0]["analysis"] != a.ID || panics[0]["panic"] != want {
		t.Fatalf("panic log records %v, want one with panic %q\n%s", panics, want, logs.String())
	}
}

// blockTransport holds every request until release is closed and ignores
// the request's context, like a model call that does not stop in time.
type blockTransport struct {
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (b blockTransport) RoundTrip(*http.Request) (*http.Response, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil, errors.New("released")
}

// When the budget runs out, Drain's error says what it was still waiting
// for: here an analysis run that has not stopped, and no notification.
func TestDrainErrorNamesWhatIsLeft(t *testing.T) {
	bt := blockTransport{entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.Model = &model.Client{BaseURL: "http://model.invalid", APIKey: "k", Model: "m", HTTP: &http.Client{Transport: bt}}
	})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(bt.release) }) })
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	select {
	case <-bt.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the model step never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := s.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want context.DeadlineExceeded", err)
	}
	const want = "1 analysis run(s) still stopping, 0 notification(s) still being sent"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Drain error %q, want it to contain %q", err, want)
	}
}

type panicNotifier struct{ value any }

func (p panicNotifier) Send(context.Context, string, string, notify.Payload) notify.Result {
	panic(p.value)
}

// A panicking Notifier does not take the server down: the notification
// ends failed with "internal error", the delivery is no longer counted as
// in flight, and the panic is logged (without the URL) as one JSON line.
func TestNotifyPanicIsRecovered(t *testing.T) {
	logs := captureJSONLog(t)
	data := t.TempDir()
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = panicNotifier{"boom while posting to " + secretNotifyURL + "\nforged"}
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	got := waitNotification(t, srv.URL, a.ID, "failed")
	if n := got.Notification; n.LastError != "internal error" || n.Attempts != 0 {
		t.Fatalf("notification after a panic %+v", n)
	}
	waitIdle(t, s)
	if code := do(t, "GET", srv.URL+"/api/health", nil, nil); code != 200 {
		t.Fatalf("server after a panic: %d", code)
	}
	var panics []map[string]any
	for _, r := range jsonLines(t, logs.String()) {
		if r["msg"] == "notification panicked" {
			panics = append(panics, r)
		}
	}
	if len(panics) != 1 || panics[0]["analysis"] != a.ID || panics[0]["host"] != "hooks.example.com" ||
		panics[0]["panic"] != "boom while posting to forged" {
		t.Fatalf("panic log records %v\n%s", panics, logs.String())
	}
	assertNoLeak(t, "log", logs.String())
}

// The top hypothesis is the one with the lowest rank, whatever the order.
func TestPayloadTopHypothesisIsLowestRank(t *testing.T) {
	a := &Analysis{ID: "a1", Repo: "r1", Status: "done", Model: &model.Result{Status: "ok", Result: &model.Output{
		Hypotheses: []model.Hypothesis{
			{Rank: 2, Title: "second", File: "b.ts"},
			{Rank: 1, Title: "first", File: "a.ts", LineStart: 3, LineEnd: 4, ConfidenceLabel: "high"},
			{Rank: 3, Title: "third", File: "c.ts"},
		},
	}}}
	p := payloadOf(a)
	if h := p.TopHypothesis; h == nil || h.Rank != 1 || h.Title != "first" || h.File != "a.ts" || h.LineStart != 3 || h.LineEnd != 4 || h.ConfidenceLabel != "high" {
		t.Fatalf("top hypothesis %+v", p.TopHypothesis)
	}
	if p.ModelStatus != "ok" {
		t.Fatalf("model_status %q", p.ModelStatus)
	}
	a.Model.Result.Hypotheses = nil
	if p := payloadOf(a); p.TopHypothesis != nil {
		t.Fatalf("no hypothesis must give null, got %+v", p.TopHypothesis)
	}
}

// Shutdown: a delivery still in flight completes before Drain returns.
func TestDrainWaitsForDeliveryInFlight(t *testing.T) {
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	release := releaser(t, fake)
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drained <- s.Drain(ctx)
	}()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) while the delivery was still in flight", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Drain did not return after the delivery ended")
	}
	if st, ok := loadStoredAnalysis(t, data, a.ID); !ok || st.Notification == nil || st.Notification.Status != "delivered" {
		t.Fatalf("outcome not recorded before Drain returned: %+v", st.Notification)
	}
}

// Shutdown: a running analysis ends as error with the addendum's message,
// which starts its notification, and Drain waits for that delivery.
func TestDrainEndsRunningAnalysisAndNotifies(t *testing.T) {
	data := t.TempDir()
	fake := newFake(false, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(20000)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	time.Sleep(300 * time.Millisecond) // the analysis is under way
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("Drain took %v: it waited for the test command instead of stopping it", el)
	}
	const want = "analysis interrupted: the server stopped while it was running"
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].payload.Status != "error" || calls[0].payload.Error != want {
		t.Fatalf("notification of the interrupted analysis: %+v", calls)
	}
	var got Analysis
	do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got)
	if got.Status != "error" || got.Error != want || got.Notification == nil || got.Notification.Status != "delivered" {
		t.Fatalf("after Drain: status %q error %q notification %+v", got.Status, got.Error, got.Notification)
	}
	if st, ok := loadStoredAnalysis(t, data, a.ID); !ok || st.Status != "error" || st.Error != want || st.Notification.Status != "delivered" {
		t.Fatalf("stored after Drain: %+v", st)
	}
}

// waitState waits until cond, called with s.mu held, is true.
func waitState(t *testing.T, s *Server, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		ok := cond()
		s.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Shutdown: an analysis created after Drain began (its request was still in
// create when the HTTP shutdown ended) ends at once as error with the
// addendum's message, before its test command starts; its notification is
// sent and Drain waits for it. A's delivery stays blocked throughout, so
// nothing else wakes Drain while B is created: only the stopping check in
// supervise ends B.
func TestDrainEndsAnalysisCreatedAfterItBegan(t *testing.T) {
	data := t.TempDir()
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "started"))
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50), {ID: "marker", Argv: []string{"node", "-e",
			fmt.Sprintf(`require("fs").writeFileSync(%q,"1");setTimeout(()=>{},20000)`, marker)}, Description: "marks its start"}}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	release := releaser(t, fake)
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST A %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)
	waitState(t, s, "A's delivery to be the only work left", func() bool {
		return s.active == 0 && len(s.runs) == 0 && len(s.delivering) == 1
	})

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		drained <- s.Drain(ctx)
	}()
	waitState(t, s, "Drain to begin", func() bool { return s.stopping })

	var b Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r2", shas["r2"].Baseline, "marker", secretNotifyURL, "json"), &b); code != 202 {
		t.Fatalf("POST B %d", code)
	}
	created := time.Now()
	var got Analysis
	for {
		got = Analysis{}
		do(t, "GET", srv.URL+"/api/analyses/"+b.ID, nil, &got)
		if got.Status != "running" {
			break
		}
		if el := time.Since(created); el > 2*time.Second {
			t.Fatalf("an analysis created after Drain began is still running after %v", el.Round(time.Millisecond))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != "error" || got.Error != msgInterrupted || got.BaselineRun != nil || got.CurrentRun != nil {
		t.Fatalf("B ended as %s %q (baseline run %v, current run %v), want error %q and no run", got.Status, got.Error, got.BaselineRun, got.CurrentRun, msgInterrupted)
	}
	waitCalls(t, fake, 2)
	if p := fake.Calls()[1].payload; p.ID != b.ID || p.Status != "error" || p.Error != msgInterrupted {
		t.Fatalf("notification of B: %+v", p)
	}
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) while two deliveries were blocked", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("Drain did not return after the deliveries ended")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("B's test command started (marker: %v)", err)
	}
	if st, ok := loadStoredAnalysis(t, data, b.ID); !ok || st.Status != "error" || st.Error != msgInterrupted || st.Notification == nil || st.Notification.Status != "delivered" {
		t.Fatalf("stored B: found=%v %+v", ok, st)
	}
}

// Shutdown: Drain gives up when its context ends. The cut-off delivery
// stays pending while this process runs, and a restarted server over the
// same DATA_DIR reports it as failed.
func TestDrainReturnsWhenContextEnds(t *testing.T) {
	data := t.TempDir()
	fake := newFake(true, notify.Result{Delivered: true, Attempts: 1, StatusCode: 200})
	s, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.WebhookSecret = testWebhookSecret
		c.Notifier = fake
	})
	releaser(t, fake)
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", shas["r1"].Baseline, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	pollDone(t, srv.URL, a.ID)
	waitCalls(t, fake, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain = %v, want context.DeadlineExceeded", err)
	}
	if want := "0 analysis run(s) still stopping, 1 notification(s) still being sent"; !strings.Contains(err.Error(), want) {
		t.Fatalf("Drain error %q, want it to contain %q", err, want)
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 5*time.Second {
		t.Fatalf("Drain returned after %v, want about 300ms", el)
	}
	var got Analysis
	do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got)
	if got.Notification == nil || got.Notification.Status != "pending" {
		t.Fatalf("still delivering, GET shows %+v", got.Notification)
	}

	srv2 := httptest.NewServer(New(Config{Root: root, Model: &model.Client{}, DataDir: data}))
	defer srv2.Close()
	var after Analysis
	do(t, "GET", srv2.URL+"/api/analyses/"+a.ID, nil, &after)
	if n := after.Notification; n == nil || n.Status != "failed" || n.LastError != "delivery interrupted: the server stopped before it finished" {
		t.Fatalf("after a restart: %+v", after.Notification)
	}
}
