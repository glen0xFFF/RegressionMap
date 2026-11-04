package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
// TEST-NET-3 address. The injected dialer is called only with an address
// that passed the public-IP check, and routes it to the local receiver.
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
	vetted := netip.MustParseAddr("203.0.113.7")
	r.sender = &notify.Sender{
		Secret:   []byte(testWebhookSecret),
		Resolver: staticResolver{"example.com": {vetted}},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "203.0.113.7:443" {
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
	if !strings.HasSuffix(msg.Text, "AI-generated hypothesis, not verified.") {
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
