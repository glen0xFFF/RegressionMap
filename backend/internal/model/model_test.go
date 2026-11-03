package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"regressionmap/internal/evidence"
)

var ev = []evidence.Item{
	{ID: "E1", Type: "test_failure", File: "test/pagination.test.ts", LineStart: 8, LineEnd: 8, Excerpt: "first page"},
	{ID: "E2", Type: "diff_hunk", File: "src/pagination.ts", LineStart: 7, LineEnd: 13, Excerpt: "-  const end = start + pageSize;"},
	{ID: "E3", Type: "diff_hunk", File: "src/format.ts", LineStart: 1, LineEnd: 3, Excerpt: "replace"},
}

func hyp(rank int, file string, ls, le int, ids ...string) map[string]any {
	return map[string]any{
		"rank": rank, "title": "t", "file": file, "line_start": ls, "line_end": le,
		"reason": "r", "supporting_evidence_ids": ids, "counter_evidence_ids": []string{},
		"verification_suggestion": "run tests", "confidence_label": "medium",
	}
}

func content(hs ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"summary": "s", "hypotheses": hs, "limitations": []string{"l"}})
	return string(b)
}

// server returns a fake chat-completions endpoint answering with content,
// and records the last request.
func server(t *testing.T, status int, content string, last *map[string]any, auth *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if auth != nil {
			*auth = r.Header.Get("Authorization")
		}
		b, _ := io.ReadAll(r.Body)
		if last != nil {
			_ = json.Unmarshal(b, last)
		}
		w.WriteHeader(status)
		if status != 200 {
			w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
	}))
}

func TestDisabledWithoutKey(t *testing.T) {
	c := Client{BaseURL: "http://127.0.0.1:1", Model: "muse-spark-1.3"}
	r := c.Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "disabled" || r.Result != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestRequestShapeAndAuth(t *testing.T) {
	var req map[string]any
	var auth string
	s := server(t, 200, content(hyp(1, "src/pagination.ts", 10, 10, "E1", "E2")), &req, &auth)
	defer s.Close()
	c := Client{BaseURL: s.URL, APIKey: "secret-key-123", Model: "muse-spark-1.3"}
	r := c.Analyze(context.Background(), Input{Evidence: ev, Command: "node --test"})
	if r.Status != "ok" {
		t.Fatalf("status %s: %s", r.Status, r.Message)
	}
	if auth != "Bearer secret-key-123" {
		t.Fatalf("auth %q", auth)
	}
	if req["model"] != "muse-spark-1.3" {
		t.Fatalf("model %v", req["model"])
	}
	rf := req["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != true {
		t.Fatalf("response_format %v", rf)
	}
	b, _ := json.Marshal(req["messages"])
	if !strings.Contains(string(b), "E2") || !strings.Contains(string(b), "untrusted") {
		t.Fatal("prompt must include evidence and untrusted-data warning")
	}
}

func TestAPIErrorDoesNotLeakKey(t *testing.T) {
	s := server(t, 500, "", nil, nil)
	defer s.Close()
	r := retryClient(s.URL, &fakeSleeper{}).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" || strings.Contains(r.Message, "secret-key-123") {
		t.Fatalf("got %+v", r)
	}
}

// A server that echoes the key across the 300-byte message boundary must not
// leak any part of it: scrubbing has to happen before truncation.
func TestAPIErrorEchoingKeyAcrossTruncationDoesNotLeakKey(t *testing.T) {
	const key = "sk-meta-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": strings.Repeat("x", 257) + "Incorrect API key provided: " + key}})
	}))
	defer s.Close()
	c := retryClient(s.URL, &fakeSleeper{})
	c.APIKey = key
	r := c.Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" {
		t.Fatalf("got %+v", r)
	}
	if strings.Contains(r.Message, "sk-meta-") || strings.Contains(r.Message, key[:8]) {
		t.Fatalf("message leaks part of the key: %q", r.Message)
	}
	// the same holds for a non-JSON body that echoes the key
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(strings.Repeat("y", 290) + key))
	}))
	defer s2.Close()
	c.BaseURL = s2.URL
	r = c.Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" || strings.Contains(r.Message, "sk-meta-") {
		t.Fatalf("raw body leaks part of the key: %q", r.Message)
	}
}

func TestMalformedJSONIsInvalid(t *testing.T) {
	s := server(t, 200, "not json {", nil, nil)
	defer s.Close()
	r := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "invalid" || r.Result != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestValidateRejectsBadHypotheses(t *testing.T) {
	raw := content(
		hyp(1, "src/pagination.ts", 10, 10, "E2", "E1"),
		hyp(2, "src/pagination.ts", 10, 10, "E99"), // unknown id
		hyp(3, "../etc/passwd", 1, 1, "E1"),        // traversal
		hyp(4, "src/other.ts", 1, 1, "E1"),         // file not in evidence
		hyp(5, "src/pagination.ts", 9, 3, "E2"),    // inverted range
		hyp(6, "src/format.ts", 1, 3),              // no support
	)
	res, rejected, err := Validate(raw, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hypotheses) != 1 || res.Hypotheses[0].Rank != 1 {
		t.Fatalf("kept %+v", res.Hypotheses)
	}
	if len(rejected) != 5 {
		t.Fatalf("rejected %v", rejected)
	}
}

func TestValidateRejectsBadConfidence(t *testing.T) {
	h := hyp(1, "src/pagination.ts", 10, 10, "E2")
	h["confidence_label"] = "certain"
	res, rejected, _ := Validate(content(h), ev)
	if len(res.Hypotheses) != 0 || len(rejected) != 1 {
		t.Fatalf("kept=%v rejected=%v", res.Hypotheses, rejected)
	}
}

// reply is one scripted response of the sequence server.
type reply struct {
	status     int
	retryAfter string
}

// seqServer answers with replies in order (the last one repeats forever) and
// counts the requests it received.
func seqServer(t *testing.T, n *atomic.Int32, replies ...reply) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		rp := replies[i]
		if rp.retryAfter != "" {
			w.Header().Set("Retry-After", rp.retryAfter)
		}
		w.WriteHeader(rp.status)
		if rp.status != 200 {
			w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": content(hyp(1, "src/pagination.ts", 10, 10, "E1", "E2"))}}}})
	}))
}

// fakeSleeper records requested delays instead of sleeping.
type fakeSleeper struct{ delays []time.Duration }

func (f *fakeSleeper) sleep(ctx context.Context, d time.Duration) error {
	f.delays = append(f.delays, d)
	return ctx.Err()
}

func retryClient(url string, fs *fakeSleeper) *Client {
	return &Client{
		BaseURL: url, APIKey: "secret-key-123", Model: "m",
		MaxAttempts: 3, BackoffBase: 100 * time.Millisecond, BackoffCap: time.Second,
		rand: func() float64 { return 0.5 }, sleep: fs.sleep,
	}
}

func TestRetries429ThenOK(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 429}, reply{status: 429}, reply{status: 200})
	defer s.Close()
	fs := &fakeSleeper{}
	r := retryClient(s.URL, fs).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "ok" {
		t.Fatalf("status %s: %s", r.Status, r.Message)
	}
	if n.Load() != 3 {
		t.Fatalf("requests %d, want 3", n.Load())
	}
	// full jitter with rand=0.5: 0.5*100ms, then 0.5*200ms
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
	if fmt.Sprint(fs.delays) != fmt.Sprint(want) {
		t.Fatalf("delays %v, want %v", fs.delays, want)
	}
}

func TestNoRetryOn400(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 400})
	defer s.Close()
	fs := &fakeSleeper{}
	r := retryClient(s.URL, fs).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" || n.Load() != 1 || len(fs.delays) != 0 {
		t.Fatalf("status=%s requests=%d delays=%v msg=%s", r.Status, n.Load(), fs.delays, r.Message)
	}
	if !strings.Contains(r.Message, "HTTP 400") || !strings.Contains(r.Message, "1 attempt") {
		t.Fatalf("message %q", r.Message)
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 503})
	defer s.Close()
	fs := &fakeSleeper{}
	c := retryClient(s.URL, fs)
	c.MaxAttempts = 4
	r := c.Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" || n.Load() != 4 {
		t.Fatalf("status=%s requests=%d", r.Status, n.Load())
	}
	if !strings.Contains(r.Message, "HTTP 503") || !strings.Contains(r.Message, "4 attempts") || strings.Contains(r.Message, "secret-key-123") {
		t.Fatalf("message %q", r.Message)
	}
	// 50ms, 100ms, 200ms: exponential, never above the cap
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}
	if fmt.Sprint(fs.delays) != fmt.Sprint(want) {
		t.Fatalf("delays %v, want %v", fs.delays, want)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 502})
	defer s.Close()
	fs := &fakeSleeper{}
	c := retryClient(s.URL, fs)
	c.MaxAttempts = 5
	c.BackoffCap = 300 * time.Millisecond
	c.rand = func() float64 { return 0.999 }
	c.Analyze(context.Background(), Input{Evidence: ev})
	for _, d := range fs.delays {
		if d > c.BackoffCap {
			t.Fatalf("delay %v above cap %v (all %v)", d, c.BackoffCap, fs.delays)
		}
	}
	if len(fs.delays) != 4 {
		t.Fatalf("delays %v", fs.delays)
	}
}

func TestRetryAfterHonouredAndCapped(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 429, retryAfter: "1"}, reply{status: 429, retryAfter: "120"}, reply{status: 200})
	defer s.Close()
	fs := &fakeSleeper{}
	c := retryClient(s.URL, fs)
	c.BackoffCap = 5 * time.Second
	r := c.Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "ok" || n.Load() != 3 {
		t.Fatalf("status=%s requests=%d msg=%s", r.Status, n.Load(), r.Message)
	}
	want := []time.Duration{time.Second, 5 * time.Second}
	if fmt.Sprint(fs.delays) != fmt.Sprint(want) {
		t.Fatalf("delays %v, want %v", fs.delays, want)
	}
}

func TestContextCancelStopsRetrying(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 503})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := retryClient(s.URL, nil)
	c.MaxAttempts = 10
	c.sleep = func(ctx context.Context, d time.Duration) error {
		cancel() // the caller gives up while we wait
		<-ctx.Done()
		return ctx.Err()
	}
	r := c.Analyze(ctx, Input{Evidence: ev})
	if r.Status != "error" || n.Load() != 1 {
		t.Fatalf("status=%s requests=%d msg=%s", r.Status, n.Load(), r.Message)
	}
	if !strings.Contains(r.Message, "1 attempt") {
		t.Fatalf("message %q", r.Message)
	}
}

func TestAlreadyCancelledContextMakesNoRetry(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 200})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fs := &fakeSleeper{}
	r := retryClient(s.URL, fs).Analyze(ctx, Input{Evidence: ev})
	if r.Status != "error" || len(fs.delays) != 0 || n.Load() != 0 {
		t.Fatalf("status=%s requests=%d delays=%v", r.Status, n.Load(), fs.delays)
	}
}

func TestRetriesTransportErrors(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 200})
	url := s.URL
	s.Close() // nothing listens any more: every attempt is a transport error
	fs := &fakeSleeper{}
	r := retryClient(url, fs).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" || len(fs.delays) != 2 {
		t.Fatalf("status=%s delays=%v msg=%s", r.Status, fs.delays, r.Message)
	}
	if !strings.Contains(r.Message, "3 attempts") || strings.Contains(r.Message, "secret-key-123") {
		t.Fatalf("message %q", r.Message)
	}
}

func TestOverallTimeoutAcrossAttempts(t *testing.T) {
	block := make(chan struct{})
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-r.Context().Done():
		case <-block:
		}
	}))
	defer s.Close()
	defer close(block)
	c := retryClient(s.URL, &fakeSleeper{})
	c.Timeout = 100 * time.Millisecond
	start := time.Now()
	r := c.Analyze(context.Background(), Input{Evidence: ev})
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %v, overall timeout not applied", el)
	}
	if r.Status != "error" || n.Load() != 1 {
		t.Fatalf("status=%s requests=%d msg=%s", r.Status, n.Load(), r.Message)
	}
	if !strings.Contains(r.Message, "timed out") {
		t.Fatalf("message %q", r.Message)
	}
}

// The production wait (no injected sleep) must really wait between retries,
// otherwise a 429ing API would get a retry storm.
func TestRealBackoffWaitsBetweenRetries(t *testing.T) {
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 503})
	defer s.Close()
	c := &Client{
		BaseURL: s.URL, APIKey: "secret-key-123", Model: "m",
		MaxAttempts: 3, BackoffBase: 20 * time.Millisecond, BackoffCap: 40 * time.Millisecond,
		rand: func() float64 { return 0.999 }, // waits ~20ms then ~40ms; sleep is nil
	}
	start := time.Now()
	r := c.Analyze(context.Background(), Input{Evidence: ev})
	el := time.Since(start)
	if r.Status != "error" || n.Load() != 3 {
		t.Fatalf("status=%s requests=%d msg=%s", r.Status, n.Load(), r.Message)
	}
	// timers never fire early, so the two waits add up to at least ~59.9ms
	if el < 55*time.Millisecond {
		t.Fatalf("3 attempts took %v: backoff did not wait", el)
	}
}

func TestRealSleepStopsOnContextCancel(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.doSleep(ctx, time.Minute)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v, want ctx error promptly", err, time.Since(start))
	}
	start = time.Now()
	if err := c.doSleep(context.Background(), 30*time.Millisecond); err != nil || time.Since(start) < 30*time.Millisecond {
		t.Fatalf("err=%v after %v, want nil after >=30ms", err, time.Since(start))
	}
}

func TestRetryDefaults(t *testing.T) {
	c := &Client{}
	if c.maxAttempts() != 3 || c.backoffBase() != 500*time.Millisecond || c.backoffCap() != 8*time.Second || c.timeout() != 120*time.Second {
		t.Fatalf("defaults %d %v %v %v", c.maxAttempts(), c.backoffBase(), c.backoffCap(), c.timeout())
	}
}

func TestAllRejectedIsInvalid(t *testing.T) {
	s := server(t, 200, content(hyp(1, "x.ts", 1, 1, "E42")), nil, nil)
	defer s.Close()
	r := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "invalid" || len(r.Rejected) != 1 {
		t.Fatalf("got %+v", r)
	}
}
