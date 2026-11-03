package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	r := (&Client{BaseURL: s.URL, APIKey: "secret-key-123", Model: "m"}).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "error" || strings.Contains(r.Message, "secret-key-123") {
		t.Fatalf("got %+v", r)
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

func TestAllRejectedIsInvalid(t *testing.T) {
	s := server(t, 200, content(hyp(1, "x.ts", 1, 1, "E42")), nil, nil)
	defer s.Close()
	r := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Analyze(context.Background(), Input{Evidence: ev})
	if r.Status != "invalid" || len(r.Rejected) != 1 {
		t.Fatalf("got %+v", r)
	}
}
