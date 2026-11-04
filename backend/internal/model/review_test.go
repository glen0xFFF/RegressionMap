package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"regressionmap/internal/evidence"
)

// evR is ev plus a source item and a commit item, for the review tests.
var evR = append(append([]evidence.Item{}, ev...),
	evidence.Item{ID: "E4", Type: "source", File: "src/pagination.ts", LineStart: 1, LineEnd: 20, Excerpt: "export function page() {}"},
	evidence.Item{ID: "E5", Type: "commit", Title: "commit 0123456: fix the page end", Excerpt: "fix the page end"},
)

// finding is one finding of a review answer, with the title "t".
func finding(lens, sev, file string, ls, le int, ids ...string) map[string]any {
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{
		"lens": lens, "severity": sev, "title": "t", "detail": "d", "suggestion": "s",
		"file": file, "line_start": ls, "line_end": le, "evidence_ids": ids,
	}
}

func titled(f map[string]any, title string) map[string]any {
	f["title"] = title
	return f
}

// reviewContent is a review answer with the summary "s".
func reviewContent(fs ...map[string]any) string {
	if fs == nil {
		fs = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"summary": "s", "findings": fs})
	return string(b)
}

// ck is one check of a check answer.
type ck struct {
	id, verdict, reason string
	ids                 []string
}

func checksContent(cs ...ck) string {
	out := []map[string]any{}
	for _, c := range cs {
		ids := c.ids
		if ids == nil {
			ids = []string{}
		}
		out = append(out, map[string]any{"id": c.id, "verdict": c.verdict, "reason": c.reason, "evidence_ids": ids})
	}
	b, _ := json.Marshal(map[string]any{"checks": out})
	return string(b)
}

// three is three valid code findings, titled a, b and c (R1, R2 and R3).
func three() []map[string]any {
	return []map[string]any{
		titled(finding("code", "high", "src/pagination.ts", 7, 8, "E2"), "a"),
		titled(finding("code", "high", "src/pagination.ts", 9, 10, "E2"), "b"),
		titled(finding("code", "high", "src/pagination.ts", 11, 12, "E2"), "c"),
	}
}

// owed runs Review against a server that answers fs, and returns the review,
// which must owe its check.
func owed(t *testing.T, fs ...map[string]any) Review {
	t.Helper()
	s := server(t, 200, reviewContent(fs...), nil, nil)
	defer s.Close()
	rv := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Review(context.Background(), Input{Evidence: evR})
	if rv.Status != "ok" || rv.CheckStatus != "" {
		t.Fatalf("the review does not owe a check: %+v", rv)
	}
	return rv
}

// checkWith runs Check against a server that answers status and content.
func checkWith(t *testing.T, status int, content string, rv Review, req *map[string]any) Review {
	t.Helper()
	s := server(t, status, content, req, nil)
	defer s.Close()
	return retryClient(s.URL, &fakeSleeper{}).Check(context.Background(), Input{Evidence: evR}, rv)
}

func obj(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, not an object", what, v)
	}
	return m
}

func strList(v any) []string {
	a, _ := v.([]any)
	out := []string{}
	for _, x := range a {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func sorted(s []string) []string {
	s = append([]string{}, s...)
	sort.Strings(s)
	return s
}

func messages(t *testing.T, req map[string]any) (system, user string) {
	t.Helper()
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages %v", req["messages"])
	}
	system, _ = obj(t, msgs[0], "system message")["content"].(string)
	user, _ = obj(t, msgs[1], "user message")["content"].(string)
	return system, user
}

// verdicts lists each finding as id:verdict.
func verdicts(fs []Finding) string {
	var b []string
	for _, f := range fs {
		b = append(b, f.ID+":"+f.Check.Verdict)
	}
	return strings.Join(b, " ")
}

func allUnchecked(t *testing.T, fs []Finding) {
	t.Helper()
	for _, f := range fs {
		if f.Check.Verdict != "unchecked" || f.Check.Reason != "" || f.Check.EvidenceIDs == nil || len(f.Check.EvidenceIDs) != 0 {
			t.Errorf("%s: check %+v, want unchecked with no reason and []", f.ID, f.Check)
		}
	}
}

func TestReviewDisabledWithoutKey(t *testing.T) {
	if MsgNotConfigured != "Meta model not configured: MODEL_API_KEY is not set on the backend." {
		t.Fatalf("MsgNotConfigured %q", MsgNotConfigured)
	}
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 200})
	defer s.Close()
	c := &Client{BaseURL: s.URL, Model: "muse-spark-1.3"}
	rv := c.Review(context.Background(), Input{Evidence: evR})
	if n.Load() != 0 {
		t.Fatalf("%d request(s) reached the server", n.Load())
	}
	if rv.Status != "disabled" || rv.Message != MsgNotConfigured || rv.ModelID != "muse-spark-1.3" ||
		rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" || rv.Summary != "" || rv.Counts != (ReviewCounts{}) {
		t.Fatalf("got %+v", rv)
	}
	if rv.Findings == nil || len(rv.Findings) != 0 || rv.Rejected == nil || len(rv.Rejected) != 0 {
		t.Fatalf("findings %#v, rejected %#v: want non-nil empty", rv.Findings, rv.Rejected)
	}
	// Contract rule 5: the review states the hypothesis call's text.
	if r := c.Analyze(context.Background(), Input{Evidence: evR}); r.Message != MsgNotConfigured {
		t.Fatalf("the hypothesis call says %q", r.Message)
	}
}

// The JSON tags and their order are the contract's.
func TestReviewJSONShape(t *testing.T) {
	b, _ := json.Marshal((&Client{Model: "m"}).NoReview("disabled", "x"))
	want := `{"status":"disabled","model_id":"m","message":"x","check_status":"skipped","check_message":"no review to check","summary":"",` +
		`"counts":{"confirmed":0,"disputed":0,"refuted":0,"unchecked":0},"findings":[],"rejected":[]}`
	if string(b) != want {
		t.Fatalf("NoReview JSON\n got %s\nwant %s", b, want)
	}
	rv := owed(t, finding("security", "high", "src/pagination.ts", 7, 8, "E2", "E4"))
	b, _ = json.Marshal(rv.Findings)
	want = `[{"id":"R1","lens":"security","severity":"high","title":"t","detail":"d","suggestion":"s","file":"src/pagination.ts",` +
		`"line_start":7,"line_end":8,"evidence_ids":["E2","E4"],"check":{"verdict":"unchecked","reason":"","evidence_ids":[]}}]`
	if string(b) != want {
		t.Fatalf("finding JSON\n got %s\nwant %s", b, want)
	}
}

func TestReviewRequestShape(t *testing.T) {
	var req map[string]any
	s := server(t, 200, reviewContent(finding("code", "high", "src/pagination.ts", 7, 8, "E2")), &req, nil)
	defer s.Close()
	warn := "diff larger than 8 MiB: only the first 8 MiB were analysed"
	(&Client{BaseURL: s.URL, APIKey: "k", Model: "muse-spark-1.3"}).Review(context.Background(), Input{Evidence: evR, Warnings: []string{warn}})
	if req["model"] != "muse-spark-1.3" {
		t.Fatalf("model %v", req["model"])
	}
	rf := obj(t, req["response_format"], "response_format")
	js := obj(t, rf["json_schema"], "json_schema")
	if rf["type"] != "json_schema" || js["name"] != "ChangeReview" || js["strict"] != true {
		t.Fatalf("response_format %v", rf)
	}
	sc := obj(t, js["schema"], "schema")
	if got := strList(sc["required"]); !reflect.DeepEqual(got, []string{"summary", "findings"}) || sc["additionalProperties"] != false {
		t.Fatalf("top level required %v, additionalProperties %v", got, sc["additionalProperties"])
	}
	props := obj(t, sc["properties"], "schema properties")
	if len(props) != 2 {
		t.Fatalf("top level properties %v", props)
	}
	items := obj(t, obj(t, props["findings"], "findings")["items"], "finding schema")
	want9 := []string{"detail", "evidence_ids", "file", "lens", "line_end", "line_start", "severity", "suggestion", "title"}
	if got := sorted(strList(items["required"])); !reflect.DeepEqual(got, want9) || items["additionalProperties"] != false {
		t.Fatalf("finding required %v, additionalProperties %v", got, items["additionalProperties"])
	}
	fp := obj(t, items["properties"], "finding properties")
	if len(fp) != 9 {
		t.Fatalf("finding properties %v", fp)
	}
	if got := strList(obj(t, fp["lens"], "lens")["enum"]); !reflect.DeepEqual(got, []string{"code", "feature", "security"}) {
		t.Fatalf("lens enum %v", got)
	}
	if got := strList(obj(t, fp["severity"], "severity")["enum"]); !reflect.DeepEqual(got, []string{"high", "medium", "low"}) {
		t.Fatalf("severity enum %v", got)
	}
	sys, user := messages(t, req)
	for _, w := range []string{
		"code", "feature", "security", "at most 5", "untrusted", "diff_hunk", "within 3 lines", "commit messages included",
		"title at most 200 characters", "detail at most 2000 characters", "suggestion at most 500 characters",
		"summary at most 2000 characters", "No style or naming remarks",
	} {
		if !strings.Contains(sys, w) {
			t.Errorf("system prompt lacks %q:\n%s", w, sys)
		}
	}
	if !strings.Contains(user, `"E2"`) || !strings.Contains(user, `"E5"`) {
		t.Errorf("user prompt lacks the evidence:\n%s", user)
	}
	if !strings.Contains(user, "The server cut this input before analysis") || !strings.Contains(user, "- "+warn+"\n") {
		t.Errorf("user prompt lacks the warnings sentence:\n%s", user)
	}
}

func TestValidateReviewRejections(t *testing.T) {
	base := func() map[string]any { return finding("code", "high", "src/pagination.ts", 7, 8, "E2") }
	with := func(k string, v any) map[string]any { f := base(); f[k] = v; return f }
	one := func(f map[string]any) []map[string]any { return []map[string]any{f} }
	cases := []struct {
		name string
		fs   []map[string]any
		want string
	}{
		{"lens", one(with("lens", "style")), `finding 1 ("t"): unknown lens "style"`},
		{"severity", one(with("severity", "critical")), `finding 1 ("t"): unknown severity "critical"`},
		{"blank title", one(with("title", "  ")), `finding 1 ("  "): empty title`},
		{"no ids", one(with("evidence_ids", []string{})), `finding 1 ("t"): cites no evidence`},
		{"unknown id", one(with("evidence_ids", []string{"E99"})), `finding 1 ("t"): cites unknown evidence id E99`},
		{"only a test failure", one(with("evidence_ids", []string{"E1"})), `finding 1 ("t"): cites no diff_hunk or source evidence`},
		// A commit message can steer the review: it never stands as the
		// evidence of a finding on its own.
		{"only a commit", one(with("evidence_ids", []string{"E5"})), `finding 1 ("t"): cites no diff_hunk or source evidence`},
		{"only a commit, no file", one(finding("feature", "high", "", 0, 0, "E5")), `finding 1 ("t"): cites no diff_hunk or source evidence`},
		{"a commit and a test failure", one(with("evidence_ids", []string{"E5", "E1"})), `finding 1 ("t"): cites no diff_hunk or source evidence`},
		{"range without file", one(finding("code", "high", "", 3, 4, "E2")), `finding 1 ("t"): has a line range but no file`},
		{"traversal", one(with("file", "../x")), `finding 1 ("t"): invalid file path "../x"`},
		{"file not in evidence", one(with("file", "src/other.ts")), `finding 1 ("t"): file src/other.ts is not in the evidence`},
		{"inverted range", one(finding("code", "high", "src/pagination.ts", 9, 3, "E2")), `finding 1 ("t"): invalid line range 9-3`},
		{"no evidence in file", one(finding("code", "high", "src/format.ts", 1, 2, "E2")), `finding 1 ("t"): cites no evidence in src/format.ts`},
		{"range outside cited evidence", one(finding("code", "high", "src/pagination.ts", 40, 41, "E2")),
			`finding 1 ("t"): line range 40-41 does not overlap any cited evidence for src/pagination.ts (within 3 lines)`},
		// E4 (1-20) covers 18-19, but only the cited E2 (7-13) counts.
		{"range outside the cited item only", one(finding("code", "high", "src/pagination.ts", 18, 19, "E2")),
			`finding 1 ("t"): line range 18-19 does not overlap any cited evidence for src/pagination.ts (within 3 lines)`},
		{"repeat", []map[string]any{base(), base()}, `finding 2 ("t"): repeats finding 1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, kept, rejected, err := ValidateReview(reviewContent(c.fs...), evR)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rejected, []string{c.want}) || len(kept) != len(c.fs)-1 {
				t.Fatalf("kept %d, rejected %q; want %q", len(kept), rejected, c.want)
			}
		})
	}
}

func TestValidateReviewAcceptsEdgeShapes(t *testing.T) {
	cases := []struct {
		name string
		f    map[string]any
	}{
		{"no file", finding("feature", "medium", "", 0, 0, "E2")},
		{"whole file", finding("code", "low", "src/pagination.ts", 0, 0, "E2")},
		{"within 3 lines", finding("code", "low", "src/pagination.ts", 4, 5, "E2")},
		{"cites a commit", finding("feature", "low", "src/pagination.ts", 7, 7, "E5", "E2")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, kept, rejected, err := ValidateReview(reviewContent(c.f), evR)
			if err != nil || len(rejected) != 0 || len(kept) != 1 {
				t.Fatalf("err=%v kept=%+v rejected=%q", err, kept, rejected)
			}
			k := kept[0]
			if k.ID != "R1" || k.File != c.f["file"] || k.LineStart != c.f["line_start"] || k.LineEnd != c.f["line_end"] ||
				!reflect.DeepEqual(k.EvidenceIDs, c.f["evidence_ids"]) {
				t.Fatalf("kept %+v from %v", k, c.f)
			}
		})
	}
}

func TestValidateReviewCutsText(t *testing.T) {
	const mark = "…[truncated]"
	long := func(n int) string { return strings.Repeat("é", n) }
	f := finding("code", "high", "src/pagination.ts", 7, 8, "E2")
	f["title"], f["detail"], f["suggestion"] = long(250), long(2500), long(600)
	b, _ := json.Marshal(map[string]any{"summary": long(2100), "findings": []any{f}})
	summary, kept, rejected, err := ValidateReview(string(b), evR)
	if err != nil || len(kept) != 1 || len(rejected) != 0 {
		t.Fatalf("err=%v kept=%d rejected=%q", err, len(kept), rejected)
	}
	for _, c := range []struct {
		field, got string
		max        int
	}{
		{"title", kept[0].Title, 200}, {"detail", kept[0].Detail, 2000},
		{"suggestion", kept[0].Suggestion, 500}, {"summary", summary, 2000},
	} {
		if utf8.RuneCountInString(c.got) != c.max || c.got != long(c.max-utf8.RuneCountInString(mark))+mark {
			t.Errorf("%s: %d characters, ends %q; want %d ending with %q",
				c.field, utf8.RuneCountInString(c.got), c.got[max(0, len(c.got)-20):], c.max, mark)
		}
	}
}

func TestValidateReviewKeepsFivePerLens(t *testing.T) {
	var fs []map[string]any
	for i, sev := range []string{"low", "high", "low", "medium", "high", "low", "low"} {
		fs = append(fs, titled(finding("code", sev, "src/pagination.ts", 7, 8, "E2"), fmt.Sprintf("t%d", i+1)))
	}
	_, kept, rejected, err := ValidateReview(reviewContent(fs...), evR)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range kept {
		got = append(got, k.ID+":"+k.Title)
	}
	if strings.Join(got, " ") != "R1:t2 R2:t5 R3:t4 R4:t1 R5:t3" {
		t.Fatalf("kept %v", got)
	}
	want := []string{`finding 6 ("t6"): more than 5 code findings`, `finding 7 ("t7"): more than 5 code findings`}
	if !reflect.DeepEqual(rejected, want) {
		t.Fatalf("rejected %q, want %q", rejected, want)
	}
}

// Contract rule 4: the cap keeps the highest severities first, so a high and
// a medium finding after five lows are kept, and the last two lows are not.
func TestValidateReviewCapKeepsHighestSeverityFirst(t *testing.T) {
	var fs []map[string]any
	for i, sev := range []string{"low", "low", "low", "low", "low", "high", "medium"} {
		fs = append(fs, titled(finding("code", sev, "src/pagination.ts", 7, 8, "E2"), fmt.Sprintf("t%d", i+1)))
	}
	_, kept, rejected, err := ValidateReview(reviewContent(fs...), evR)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range kept {
		got = append(got, k.ID+":"+k.Title)
	}
	if strings.Join(got, " ") != "R1:t6 R2:t7 R3:t1 R4:t2 R5:t3" {
		t.Fatalf("kept %v", got)
	}
	want := []string{`finding 4 ("t4"): more than 5 code findings`, `finding 5 ("t5"): more than 5 code findings`}
	if !reflect.DeepEqual(rejected, want) {
		t.Fatalf("rejected %q, want %q", rejected, want)
	}
}

// Contract rule 4: the cap counts each lens on its own, so six code and six
// security findings keep five of each.
func TestValidateReviewCapIsPerLens(t *testing.T) {
	var fs []map[string]any
	for i := 1; i <= 6; i++ {
		fs = append(fs,
			titled(finding("code", "medium", "src/pagination.ts", 7, 8, "E2"), fmt.Sprintf("c%d", i)),
			titled(finding("security", "medium", "src/pagination.ts", 7, 8, "E2"), fmt.Sprintf("s%d", i)))
	}
	_, kept, rejected, err := ValidateReview(reviewContent(fs...), evR)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range kept {
		got = append(got, k.ID+":"+k.Title)
	}
	want := "R1:s1 R2:s2 R3:s3 R4:s4 R5:s5 R6:c1 R7:c2 R8:c3 R9:c4 R10:c5"
	if strings.Join(got, " ") != want {
		t.Fatalf("kept %v, want %s", got, want)
	}
	wantRejected := []string{`finding 11 ("c6"): more than 5 code findings`, `finding 12 ("s6"): more than 5 security findings`}
	if !reflect.DeepEqual(rejected, wantRejected) {
		t.Fatalf("rejected %q, want %q", rejected, wantRejected)
	}
}

func TestValidateReviewOrderAndIDs(t *testing.T) {
	var fs []map[string]any
	for _, ls := range [][2]string{{"feature", "high"}, {"code", "low"}, {"security", "medium"}, {"security", "high"}, {"code", "high"}} {
		fs = append(fs, titled(finding(ls[0], ls[1], "src/pagination.ts", 7, 8, "E2"), ls[0]+"/"+ls[1]))
	}
	_, kept, rejected, err := ValidateReview(reviewContent(fs...), evR)
	if err != nil || len(rejected) != 0 {
		t.Fatalf("err=%v rejected=%q", err, rejected)
	}
	var got []string
	for _, k := range kept {
		got = append(got, k.ID+" "+k.Lens+"/"+k.Severity)
	}
	want := "R1 security/high, R2 code/high, R3 feature/high, R4 security/medium, R5 code/low"
	if strings.Join(got, ", ") != want {
		t.Fatalf("got %s, want %s", strings.Join(got, ", "), want)
	}
}

func TestValidateReviewTopLevel(t *testing.T) {
	extra := finding("code", "high", "src/pagination.ts", 7, 8, "E2")
	extra["id"] = "R1"
	for _, raw := range []string{
		`[]`, `{}`, `{"summary":"s"}`, `{"findings":[]}`, `{"summary":"s","findings":[],"extra":1}`,
		`{"summary":null,"findings":[]}`, `{"summary":"s","findings":null}`, `not json {`,
		reviewContent(extra), // a finding field the schema does not have
	} {
		if _, _, _, err := ValidateReview(raw, evR); err == nil || !strings.HasPrefix(err.Error(), "model output did not match the schema") {
			t.Errorf("%s: err %v", raw, err)
		}
	}
}

func TestReviewAllRejectedIsInvalid(t *testing.T) {
	s := server(t, 200, reviewContent(finding("code", "high", "src/pagination.ts", 7, 8, "E99")), nil, nil)
	defer s.Close()
	rv := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Review(context.Background(), Input{Evidence: evR})
	if rv.Status != "invalid" || rv.Message != "no finding passed validation" || rv.ModelID != "m" ||
		rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" || rv.Summary != "" || rv.Counts != (ReviewCounts{}) {
		t.Fatalf("got %+v", rv)
	}
	if !reflect.DeepEqual(rv.Rejected, []string{`finding 1 ("t"): cites unknown evidence id E99`}) || rv.Findings == nil || len(rv.Findings) != 0 {
		t.Fatalf("findings %#v, rejected %q", rv.Findings, rv.Rejected)
	}
}

func TestReviewWithNoFindingsIsOK(t *testing.T) {
	s := server(t, 200, `{"summary":"clean","findings":[]}`, nil, nil)
	defer s.Close()
	rv := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Review(context.Background(), Input{Evidence: evR})
	if rv.Status != "ok" || rv.Message != "" || rv.Summary != "clean" || rv.ModelID != "m" ||
		rv.CheckStatus != "skipped" || rv.CheckMessage != "no finding to check" || rv.Counts != (ReviewCounts{}) {
		t.Fatalf("got %+v", rv)
	}
	if rv.Findings == nil || len(rv.Findings) != 0 || rv.Rejected == nil || len(rv.Rejected) != 0 {
		t.Fatalf("findings %#v, rejected %#v: want non-nil empty", rv.Findings, rv.Rejected)
	}
}

func TestReviewInvalidAnswers(t *testing.T) {
	s := server(t, 200, `{}`, nil, nil)
	defer s.Close()
	rv := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Review(context.Background(), Input{Evidence: evR})
	if rv.Status != "invalid" || !strings.HasPrefix(rv.Message, "model output did not match the schema") ||
		rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" || rv.Findings == nil || rv.Rejected == nil {
		t.Fatalf("schema mismatch: got %+v", rv)
	}
	nc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer nc.Close()
	rv = (&Client{BaseURL: nc.URL, APIKey: "k", Model: "m"}).Review(context.Background(), Input{Evidence: evR})
	if rv.Status != "invalid" || rv.Message != "model response was not a chat completion" ||
		rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" || rv.Findings == nil || rv.Rejected == nil {
		t.Fatalf("not a chat completion: got %+v", rv)
	}
}

func TestReviewOwesACheck(t *testing.T) {
	rv := owed(t, three()[:2]...)
	if rv.Status != "ok" || rv.Message != "" || rv.CheckStatus != "" || rv.CheckMessage != "" || rv.ModelID != "m" || rv.Summary != "s" {
		t.Fatalf("got %+v", rv)
	}
	if len(rv.Findings) != 2 || rv.Counts != (ReviewCounts{Unchecked: 2}) || rv.Rejected == nil || len(rv.Rejected) != 0 {
		t.Fatalf("findings %+v, counts %+v, rejected %#v", rv.Findings, rv.Counts, rv.Rejected)
	}
	allUnchecked(t, rv.Findings)
}

func TestCheckRequestShape(t *testing.T) {
	f := titled(finding("code", "high", "src/pagination.ts", 7, 8, "E2"), "Last page drops one item")
	f["detail"], f["suggestion"] = "The end index skips the last item.", "SUGGESTION-ONLY-FOR-HUMANS"
	rv := owed(t, f)
	var req map[string]any
	s := server(t, 200, checksContent(ck{"R1", "confirmed", "r", []string{"E2"}}), &req, nil)
	defer s.Close()
	warn := "diff larger than 8 MiB: only the first 8 MiB were analysed"
	out := (&Client{BaseURL: s.URL, APIKey: "k", Model: "m"}).Check(context.Background(), Input{Evidence: evR, Warnings: []string{warn}}, rv)
	if out.CheckStatus != "ok" {
		t.Fatalf("check %s: %s", out.CheckStatus, out.CheckMessage)
	}
	rf := obj(t, req["response_format"], "response_format")
	js := obj(t, rf["json_schema"], "json_schema")
	if rf["type"] != "json_schema" || js["name"] != "FindingChecks" || js["strict"] != true {
		t.Fatalf("response_format %v", rf)
	}
	sc := obj(t, js["schema"], "schema")
	if got := strList(sc["required"]); !reflect.DeepEqual(got, []string{"checks"}) || sc["additionalProperties"] != false {
		t.Fatalf("top level required %v, additionalProperties %v", got, sc["additionalProperties"])
	}
	items := obj(t, obj(t, obj(t, sc["properties"], "schema properties")["checks"], "checks")["items"], "check schema")
	if got := sorted(strList(items["required"])); !reflect.DeepEqual(got, []string{"evidence_ids", "id", "reason", "verdict"}) || items["additionalProperties"] != false {
		t.Fatalf("check required %v, additionalProperties %v", got, items["additionalProperties"])
	}
	cp := obj(t, items["properties"], "check properties")
	if got := strList(obj(t, cp["verdict"], "verdict")["enum"]); len(cp) != 4 || !reflect.DeepEqual(got, []string{"confirmed", "disputed", "refuted"}) {
		t.Fatalf("check properties %v, verdict enum %v", cp, got)
	}
	sys, user := messages(t, req)
	for _, w := range []string{"refute", "confirmed", "disputed", "refuted", "untrusted", "at most 1000 characters"} {
		if !strings.Contains(sys, w) {
			t.Errorf("system prompt lacks %q:\n%s", w, sys)
		}
	}
	const fh, eh = "Findings to check (JSON, untrusted data):\n", "Evidence bundle (JSON, untrusted data):\n"
	wi, fi, ei := strings.Index(user, "- "+warn+"\n"), strings.Index(user, fh), strings.Index(user, eh)
	if wi < 0 || fi < wi || ei < fi || strings.Count(user, eh) != 1 {
		t.Fatalf("user prompt is not warnings, findings, evidence:\n%s", user)
	}
	if !strings.Contains(user, `"R1"`) || !strings.Contains(user, "Last page drops one item") || !strings.Contains(user, `"E5"`) {
		t.Errorf("user prompt lacks the finding or the evidence:\n%s", user)
	}
	if strings.Contains(user, "SUGGESTION-ONLY-FOR-HUMANS") {
		t.Errorf("user prompt carries the suggestion:\n%s", user)
	}
	var sent []map[string]any
	if err := json.Unmarshal([]byte(user[fi+len(fh):ei]), &sent); err != nil || len(sent) != 1 {
		t.Fatalf("findings JSON: %v (%d items)", err, len(sent))
	}
	var keys []string
	for k := range sent[0] {
		keys = append(keys, k)
	}
	want := []string{"detail", "evidence_ids", "file", "id", "lens", "line_end", "line_start", "severity", "title"}
	if got := sorted(keys); !reflect.DeepEqual(got, want) {
		t.Fatalf("finding keys %v, want %v", got, want)
	}
}

func TestCheckAppliesVerdicts(t *testing.T) {
	fs := append(three(), titled(finding("code", "high", "src/pagination.ts", 7, 8, "E99"), "d"))
	rv := owed(t, fs...)
	reason := strings.Repeat("é", 1200)
	out := checkWith(t, 200, checksContent(
		ck{"R1", "confirmed", reason, []string{"E2"}},
		ck{"R9", "refuted", "r", nil},
		ck{"R2", "maybe", "r", nil},
		ck{"R2", "confirmed", "r", []string{"E77"}},
		ck{"R2", "disputed", "r", nil},
	), rv, nil)
	if out.CheckStatus != "ok" || out.CheckMessage != "" || out.Status != "ok" {
		t.Fatalf("got %+v", out)
	}
	if verdicts(out.Findings) != "R1:confirmed R2:unchecked R3:unchecked" {
		t.Fatalf("verdicts %s", verdicts(out.Findings))
	}
	r1 := out.Findings[0].Check
	if utf8.RuneCountInString(r1.Reason) != 1000 || !strings.HasSuffix(r1.Reason, "…[truncated]") || !reflect.DeepEqual(r1.EvidenceIDs, []string{"E2"}) {
		t.Fatalf("R1 check: %d characters, evidence %v", utf8.RuneCountInString(r1.Reason), r1.EvidenceIDs)
	}
	allUnchecked(t, out.Findings[1:])
	want := []string{`finding 4 ("d"): cites unknown evidence id E99`, `check for R2: unknown verdict "maybe"`}
	if !reflect.DeepEqual(out.Rejected, want) {
		t.Fatalf("rejected %q, want %q", out.Rejected, want)
	}
	if out.Counts != (ReviewCounts{Confirmed: 1, Unchecked: 2}) {
		t.Fatalf("counts %+v", out.Counts)
	}
}

func TestCheckRejectsUnknownEvidence(t *testing.T) {
	rv := owed(t, three()[:1]...)
	out := checkWith(t, 200, checksContent(ck{"R1", "confirmed", "r", []string{"E2", "E99"}}), rv, nil)
	if out.CheckStatus != "ok" || verdicts(out.Findings) != "R1:unchecked" {
		t.Fatalf("got %+v", out)
	}
	allUnchecked(t, out.Findings)
	if !reflect.DeepEqual(out.Rejected, []string{"check for R1: cites unknown evidence id E99"}) || out.Counts != (ReviewCounts{Unchecked: 1}) {
		t.Fatalf("rejected %q, counts %+v", out.Rejected, out.Counts)
	}
}

func TestCheckFailureLeavesAllUnchecked(t *testing.T) {
	rv := owed(t, three()[:2]...)
	out := checkWith(t, 500, "", rv, nil)
	if out.Status != "ok" || out.CheckStatus != "error" || !strings.HasPrefix(out.CheckMessage, "model API returned HTTP 500") {
		t.Fatalf("got %+v", out)
	}
	if len(out.Findings) != 2 || out.Counts != (ReviewCounts{Unchecked: 2}) {
		t.Fatalf("findings %+v, counts %+v", out.Findings, out.Counts)
	}
	allUnchecked(t, out.Findings)
}

func TestCheckInvalidAnswer(t *testing.T) {
	for _, raw := range []string{`{"checks":[],"x":1}`, `{}`, `[]`, `{"checks":null}`} {
		rv := owed(t, three()[:2]...)
		out := checkWith(t, 200, raw, rv, nil)
		if out.Status != "ok" || out.CheckStatus != "invalid" || !strings.HasPrefix(out.CheckMessage, "model output did not match the schema") {
			t.Errorf("%s: got %+v", raw, out)
		}
		if len(out.Findings) != 2 || out.Counts != (ReviewCounts{Unchecked: 2}) {
			t.Errorf("%s: findings %+v, counts %+v", raw, out.Findings, out.Counts)
		}
		allUnchecked(t, out.Findings)
	}
}

func TestCheckWithoutOwedCheckMakesNoRequest(t *testing.T) {
	clean := server(t, 200, `{"summary":"clean","findings":[]}`, nil, nil)
	defer clean.Close()
	skipped := (&Client{BaseURL: clean.URL, APIKey: "k", Model: "m"}).Review(context.Background(), Input{Evidence: evR})
	checked := checkWith(t, 200, checksContent(ck{"R1", "confirmed", "r", []string{"E2"}}), owed(t, three()[:1]...), nil)
	var n atomic.Int32
	s := seqServer(t, &n, reply{status: 200})
	defer s.Close()
	c := &Client{BaseURL: s.URL, APIKey: "k", Model: "m"}
	for name, in := range map[string]Review{
		"no review": c.NoReview("error", "x"), "skipped": skipped, "checked": checked,
	} {
		if in.CheckStatus == "" {
			t.Fatalf("%s: the input owes a check: %+v", name, in)
		}
		if out := c.Check(context.Background(), Input{Evidence: evR}, in); !reflect.DeepEqual(out, in) {
			t.Errorf("%s: changed\n got %+v\nwant %+v", name, out, in)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("%d request(s) reached the server", n.Load())
	}
}

func TestEndCheck(t *testing.T) {
	rv := owed(t, append(three(), titled(finding("code", "high", "src/pagination.ts", 7, 8, "E99"), "d"))...)
	checked := checkWith(t, 200, checksContent(ck{"R1", "confirmed", "shown", []string{"E2"}}), rv, nil)
	if verdicts(checked.Findings) != "R1:confirmed R2:unchecked R3:unchecked" {
		t.Fatalf("set-up verdicts %s", verdicts(checked.Findings))
	}
	before, _ := json.Marshal(checked)
	out := checked.EndCheck("error", "m")
	if out.Status != "ok" || out.CheckStatus != "error" || out.CheckMessage != "m" || out.Counts != (ReviewCounts{Unchecked: 3}) ||
		out.Summary != checked.Summary || !reflect.DeepEqual(out.Rejected, checked.Rejected) || len(out.Findings) != 3 {
		t.Fatalf("got %+v", out)
	}
	allUnchecked(t, out.Findings)
	if out.Findings[0].Title != "a" || out.Findings[0].ID != "R1" {
		t.Fatalf("finding R1 changed: %+v", out.Findings[0])
	}
	out.Findings[0].Title = "changed"
	if after, _ := json.Marshal(checked); string(after) != string(before) {
		t.Fatalf("EndCheck changed its receiver:\nbefore %s\nafter  %s", before, after)
	}
}

func TestDisplayOrderAndTopFinding(t *testing.T) {
	mk := func(vs ...string) []Finding {
		fs := []Finding{}
		for i, v := range vs {
			fs = append(fs, Finding{ID: fmt.Sprintf("R%d", i+1), Check: Check{Verdict: v, EvidenceIDs: []string{}}})
		}
		return fs
	}
	ids := func(fs []Finding) string {
		var b []string
		for _, f := range fs {
			b = append(b, f.ID)
		}
		return strings.Join(b, " ")
	}
	fs := mk("refuted", "disputed", "unchecked", "confirmed", "unchecked")
	if got := ids(DisplayOrder(fs)); got != "R4 R3 R5 R2 R1" {
		t.Fatalf("display order %s", got)
	}
	if got := ids(fs); got != "R1 R2 R3 R4 R5" {
		t.Fatalf("DisplayOrder reordered its input: %s", got)
	}
	if top := TopFinding(fs); top == nil || top.ID != "R4" {
		t.Fatalf("top %+v, want R4", top)
	}
	if top := TopFinding(mk("refuted", "disputed", "unchecked", "unchecked")); top == nil || top.ID != "R3" {
		t.Fatalf("top without a confirmed finding %+v, want R3", top)
	}
	if top := TopFinding(mk("disputed", "refuted")); top != nil {
		t.Fatalf("top of disputed and refuted findings %+v, want nil", top)
	}
	if top := TopFinding(nil); top != nil {
		t.Fatalf("top of no findings %+v, want nil", top)
	}
}

func TestReviewErrorDoesNotLeakKey(t *testing.T) {
	const key = "secret-key-123"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "Incorrect API key provided: " + key}})
	}))
	defer s.Close()
	c := retryClient(s.URL, &fakeSleeper{})
	rv := c.Review(context.Background(), Input{Evidence: evR})
	if rv.Status != "error" || !strings.Contains(rv.Message, "HTTP 400") || strings.Contains(rv.Message, key) {
		t.Fatalf("review message %q (status %s)", rv.Message, rv.Status)
	}
	out := c.Check(context.Background(), Input{Evidence: evR}, owed(t, three()[:1]...))
	if out.CheckStatus != "error" || !strings.Contains(out.CheckMessage, "HTTP 400") || strings.Contains(out.CheckMessage, key) {
		t.Fatalf("check message %q (status %s)", out.CheckMessage, out.CheckStatus)
	}
}
