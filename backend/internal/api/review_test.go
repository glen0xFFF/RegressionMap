package api

// Tests of the Muse change review in the analysis pipeline
// (docs/API_CONTRACT.md, "Muse change review (v4)"): the order of the three
// model calls, the review chain's own deadline, the review an analysis gets
// when it ends early, the commit items, the list rows, analyses stored
// before v4 and the webhook.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"regressionmap/internal/evidence"
	"regressionmap/internal/gitx"
	"regressionmap/internal/model"
	"regressionmap/internal/notify"
	"regressionmap/internal/store"
)

// The default answers of chainModel. $HUNK and $LINE stand for the id and the
// first line of the request's diff_hunk item of src/pagination.ts, so every
// answer cites real evidence and passes validation.
const (
	chainHypotheses = `{"summary":"s","limitations":[],"hypotheses":[{"rank":1,"title":"end index off by one","file":"src/pagination.ts","line_start":$LINE,"line_end":$LINE,"reason":"r","supporting_evidence_ids":["$HUNK"],"counter_evidence_ids":[],"verification_suggestion":"rerun","confidence_label":"high"}]}`
	chainReview     = `{"summary":"one finding","findings":[{"lens":"code","severity":"high","title":"Last page drops one item","detail":"d","suggestion":"s","file":"src/pagination.ts","line_start":$LINE,"line_end":$LINE,"evidence_ids":["$HUNK"]}]}`
	chainChecks     = `{"checks":[{"id":"R1","verdict":"confirmed","reason":"the hunk shows it","evidence_ids":["$HUNK"]}]}`
)

// chainAnswer is how chainModel answers one kind of request.
type chainAnswer struct {
	status  int           // HTTP status; 0 means 200 with content
	content string        // the answer's message content
	hold    chan struct{} // when set, the answer waits until it is closed or the request ends
}

// chainModel is a fake chat-completions endpoint that answers each request
// by its schema name (response_format.json_schema.name): RegressionHypotheses,
// ChangeReview or FindingChecks. It records the names in the order the
// requests arrived, their bodies, and how many requests were ever in flight
// at once.
type chainModel struct {
	srv         *httptest.Server
	mu          sync.Mutex
	answers     map[string]chainAnswer
	names       []string
	bodies      []string
	inFlight    int
	maxInFlight int
}

const evidenceMarker = "Evidence bundle (JSON, untrusted data):\n"

// chainRequest returns the schema name, the user message and the evidence of
// one request body.
func chainRequest(b []byte) (name, user string, ev []evidence.Item) {
	var req struct {
		Messages       []struct{ Role, Content string } `json:"messages"`
		ResponseFormat struct {
			JSONSchema struct {
				Name string `json:"name"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if json.Unmarshal(b, &req) != nil || len(req.Messages) != 2 {
		return "", "", nil
	}
	user = req.Messages[1].Content
	if i := strings.Index(user, evidenceMarker); i >= 0 {
		var bundle struct {
			Evidence []evidence.Item `json:"evidence"`
		}
		json.Unmarshal([]byte(user[i+len(evidenceMarker):]), &bundle)
		ev = bundle.Evidence
	}
	return req.ResponseFormat.JSONSchema.Name, user, ev
}

func newChainModel(t *testing.T) *chainModel {
	t.Helper()
	m := &chainModel{answers: map[string]chainAnswer{
		"RegressionHypotheses": {content: chainHypotheses},
		"ChangeReview":         {content: chainReview},
		"FindingChecks":        {content: chainChecks},
	}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		name, _, ev := chainRequest(b)
		m.mu.Lock()
		m.names = append(m.names, name)
		m.bodies = append(m.bodies, string(b))
		m.inFlight++
		m.maxInFlight = max(m.maxInFlight, m.inFlight)
		ans := m.answers[name]
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.inFlight--
			m.mu.Unlock()
		}()
		if ans.hold != nil {
			select {
			case <-ans.hold:
			case <-r.Context().Done():
				return
			}
		}
		if ans.status != 0 && ans.status != http.StatusOK {
			http.Error(w, "unavailable", ans.status)
			return
		}
		hunk, line := "E0", 0
		for _, e := range ev {
			if e.Type == "diff_hunk" && e.File == "src/pagination.ts" {
				hunk, line = e.ID, e.LineStart
				break
			}
		}
		content := strings.NewReplacer("$HUNK", hunk, "$LINE", strconv.Itoa(line)).Replace(ans.content)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *chainModel) client() *model.Client {
	return &model.Client{BaseURL: m.srv.URL, APIKey: "test", Model: "m", MaxAttempts: 1}
}

func (m *chainModel) set(name string, a chainAnswer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers[name] = a
}

// hold makes every answer to name wait until the returned func is called.
// The test's cleanup calls it too, so no request stays held; call hold after
// newServer, so that cleanup runs before the server waits to become idle.
func (m *chainModel) hold(t *testing.T, name string) func() {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	m.mu.Lock()
	a := m.answers[name]
	a.hold = ch
	m.answers[name] = a
	m.mu.Unlock()
	return release
}

// Names returns the schema names of the requests so far, in arrival order.
func (m *chainModel) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.names...)
}

// waitFor waits until a request named name has arrived.
func (m *chainModel) waitFor(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !slices.Contains(m.Names(), name) {
		if time.Now().After(deadline) {
			t.Fatalf("no %s request arrived; requests so far: %v", name, m.Names())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// user returns the user message of the only request named name.
func (m *chainModel) user(t *testing.T, name string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var users []string
	for _, b := range m.bodies {
		if n, u, _ := chainRequest([]byte(b)); n == name {
			users = append(users, u)
		}
	}
	if len(users) != 1 {
		t.Fatalf("model got %d %s request(s), want 1", len(users), name)
	}
	return users[0]
}

// chainServer is a server over the fixture repository r1 whose model is m
// and whose test command sleeps briefly.
func chainServer(t *testing.T, m *chainModel, mut func(*Config)) (*Server, *httptest.Server, string) {
	t.Helper()
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.Model = m.client()
		if mut != nil {
			mut(c)
		}
	})
	return s, srv, shas["r1"].Baseline
}

// rawAnalysis returns the fields of GET /api/analyses/{id} as raw JSON.
func rawAnalysis(t *testing.T, url, id string) map[string]json.RawMessage {
	t.Helper()
	code, body := rawDo(t, "GET", url+"/api/analyses/"+id, nil)
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil {
		t.Fatalf("GET /api/analyses/%s: %d %v\n%s", id, code, err, body)
	}
	return out
}

// rawRows returns the rows of GET /api/analyses as raw JSON fields.
func rawRows(t *testing.T, url string) []map[string]json.RawMessage {
	t.Helper()
	code, body := rawDo(t, "GET", url+"/api/analyses", nil)
	var out struct {
		Analyses []map[string]json.RawMessage `json:"analyses"`
	}
	if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil {
		t.Fatalf("GET /api/analyses: %d %v\n%s", code, err, body)
	}
	return out.Analyses
}

func verdicts(rv *model.Review) []string {
	out := []string{}
	for _, f := range rv.Findings {
		out = append(out, f.Check.Verdict)
	}
	return out
}

// The hypothesis call runs first, then the review call, then the check
// call, one after another; the analysis is done when the check has ended.
func TestReviewRunsAfterHypothesesThenCheck(t *testing.T) {
	m := newChainModel(t)
	_, srv, base := chainServer(t, m, nil)
	a := plAnalyse(t, srv.URL, "r1", base, "slow")
	if got := strings.Join(m.Names(), ","); got != "RegressionHypotheses,ChangeReview,FindingChecks" {
		t.Fatalf("model calls %s, want RegressionHypotheses,ChangeReview,FindingChecks", got)
	}
	m.mu.Lock()
	most := m.maxInFlight
	m.mu.Unlock()
	if most != 1 {
		t.Errorf("%d model calls were in flight at once, want 1", most)
	}
	if hyp, rev := m.user(t, "RegressionHypotheses"), m.user(t, "ChangeReview"); hyp != rev {
		t.Errorf("the review call did not get the hypothesis call's input:\n%.300s\n---\n%.300s", rev, hyp)
	}
	if a.Status != "done" || a.Error != "" {
		t.Fatalf("status %q error %q, want done", a.Status, a.Error)
	}
	if a.Model == nil || a.Model.Status != "ok" || a.Model.Result == nil || len(a.Model.Result.Hypotheses) != 1 {
		t.Fatalf("model %+v", a.Model)
	}
	rv := a.Review
	if rv == nil || rv.Status != "ok" || rv.ModelID != "m" || rv.Message != "" || rv.CheckStatus != "ok" || rv.CheckMessage != "" || rv.Summary != "one finding" {
		t.Fatalf("review %+v", rv)
	}
	if len(rv.Findings) != 1 || rv.Findings[0].ID != "R1" || rv.Findings[0].Check.Verdict != "confirmed" || rv.Findings[0].Check.Reason != "the hunk shows it" {
		t.Fatalf("findings %+v", rv.Findings)
	}
	if rv.Counts != (model.ReviewCounts{Confirmed: 1}) {
		t.Fatalf("counts %+v, want 1 confirmed", rv.Counts)
	}
}

// A problem in the review chain only sets review.status or check_status:
// the analysis is still done, with no error and its hypotheses.
func TestReviewProblemsKeepAnalysisDone(t *testing.T) {
	cases := []struct {
		name              string
		step              string
		answer            chainAnswer
		status, check     string
		message, checkMsg string // prefixes
		verdicts          []string
	}{
		{"review HTTP 500", "ChangeReview", chainAnswer{status: 500}, "error", "skipped", "model API returned HTTP 500", "no review to check", []string{}},
		{"review content {}", "ChangeReview", chainAnswer{content: "{}"}, "invalid", "skipped", "model output did not match the schema", "no review to check", []string{}},
		{"check HTTP 500", "FindingChecks", chainAnswer{status: 500}, "ok", "error", "", "model API returned HTTP 500", []string{"unchecked"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newChainModel(t)
			m.set(c.step, c.answer)
			_, srv, base := chainServer(t, m, nil)
			a := plAnalyse(t, srv.URL, "r1", base, "slow")
			if a.Status != "done" || a.Error != "" {
				t.Fatalf("status %q error %q, want done and no error", a.Status, a.Error)
			}
			if a.Model == nil || a.Model.Status != "ok" || a.Model.Result == nil || len(a.Model.Result.Hypotheses) != 1 {
				t.Fatalf("hypotheses lost: %+v", a.Model)
			}
			rv := a.Review
			if rv == nil || rv.Status != c.status || rv.CheckStatus != c.check ||
				!strings.HasPrefix(rv.Message, c.message) || (c.message == "" && rv.Message != "") ||
				!strings.HasPrefix(rv.CheckMessage, c.checkMsg) {
				t.Fatalf("review %+v, want status %q (%q...) and check_status %q (%q...)", rv, c.status, c.message, c.check, c.checkMsg)
			}
			if got := verdicts(rv); !slices.Equal(got, c.verdicts) {
				t.Fatalf("verdicts %q, want %q", got, c.verdicts)
			}
			if rv.Counts != (model.ReviewCounts{Unchecked: len(c.verdicts)}) {
				t.Fatalf("counts %+v", rv.Counts)
			}
		})
	}
}

// While the review runs, GET shows the analysis running with its
// hypotheses and review null.
func TestHypothesesShowWhileReviewRuns(t *testing.T) {
	m := newChainModel(t)
	_, srv, base := chainServer(t, m, nil)
	release := m.hold(t, "ChangeReview")
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", base, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	m.waitFor(t, "ChangeReview")
	got := rawAnalysis(t, srv.URL, a.ID)
	var mr struct{ Status string }
	json.Unmarshal(got["model"], &mr)
	if string(got["status"]) != `"running"` || mr.Status != "ok" || string(got["review"]) != "null" {
		t.Fatalf("while the review runs: status %s, model %s, review %s; want running, ok model and null", got["status"], got["model"], got["review"])
	}
	release()
	done := pollDone(t, srv.URL, a.ID)
	if done.Status != "done" || done.Review == nil || done.Review.Status != "ok" || done.Review.CheckStatus != "ok" {
		t.Fatalf("after the review: status %q review %+v", done.Status, done.Review)
	}
}

// Without a model key the review is disabled and no call is made.
func TestReviewDisabledWithoutKey(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
	rv := a.Review
	if a.Status != "done" || rv == nil || rv.Status != "disabled" || rv.Message != model.MsgNotConfigured || rv.ModelID != "muse-spark-1.3" ||
		rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" || len(rv.Findings) != 0 || len(rv.Rejected) != 0 {
		t.Fatalf("status %q review %+v", a.Status, rv)
	}
	got := rawAnalysis(t, srv.URL, a.ID)
	var raw map[string]json.RawMessage
	json.Unmarshal(got["review"], &raw)
	if string(raw["findings"]) != "[]" || string(raw["rejected"]) != "[]" {
		t.Fatalf("findings %s rejected %s, want [] and []", raw["findings"], raw["rejected"])
	}
}

// An analysis that ends before its evidence exists has a skipped review.
func TestReviewSkippedWhenAnalysisEndsEarly(t *testing.T) {
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "ran-once"))
	m := newChainModel(t)
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		cfgFile := filepath.ToSlash(filepath.Join(c.Root, "r1", ".git", "config"))
		js := fmt.Sprintf(`const fs=require("fs");if(fs.existsSync(%q)){fs.appendFileSync(%q,"\n[broken\n")}else{fs.writeFileSync(%q,"1")}`, marker, cfgFile, marker)
		c.Commands = []Command{{ID: "breaker", Argv: []string{"node", "-e", js}, Description: "breaks git on its second run"}}
		c.Model = m.client()
	})
	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "breaker")
	if a.Status != "error" || !strings.HasPrefix(a.Error, "changed-file list failed: ") {
		t.Fatalf("status %q error %q", a.Status, a.Error)
	}
	rv := a.Review
	if rv == nil || rv.Status != "skipped" || rv.Message != msgReviewSkipped || rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" {
		t.Fatalf("review %+v, want skipped with %q", rv, msgReviewSkipped)
	}
	if n := m.Names(); len(n) != 0 {
		t.Fatalf("model calls %v, want none", n)
	}
}

// The review chain stops reviewMargin before the job deadline.
func TestReviewChangeDeadlines(t *testing.T) {
	old := reviewMargin
	reviewMargin = time.Second
	t.Cleanup(func() { reviewMargin = old })
	in := model.Input{Command: "node --test", Evidence: []evidence.Item{{ID: "E1", Type: "diff_hunk", Title: "Change in src/pagination.ts",
		File: "src/pagination.ts", LineStart: 7, LineEnd: 13, Excerpt: "-  const end = start + pageSize;"}}}
	jobCtx := func(t *testing.T, d time.Duration) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("review held", func(t *testing.T) {
		m := newChainModel(t)
		m.hold(t, "ChangeReview")
		s := New(Config{Root: t.TempDir(), Model: m.client()})
		start := time.Now()
		rv := s.reviewChange(jobCtx(t, time.Second+300*time.Millisecond), "a1", in)
		if rv.Status != "error" || rv.Message != msgReviewDeadline || rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" {
			t.Fatalf("review %+v, want error %q", rv, msgReviewDeadline)
		}
		if el := time.Since(start); el > 1200*time.Millisecond {
			t.Fatalf("the review ran %v, past its deadline of about 300ms", el)
		}
	})

	t.Run("check held", func(t *testing.T) {
		m := newChainModel(t)
		m.hold(t, "FindingChecks")
		s := New(Config{Root: t.TempDir(), Model: m.client()})
		rv := s.reviewChange(jobCtx(t, time.Second+300*time.Millisecond), "a1", in)
		if rv.Status != "ok" || rv.Message != "" || rv.CheckStatus != "error" || rv.CheckMessage != msgCheckDeadline {
			t.Fatalf("review %+v, want ok with check_status error %q", rv, msgCheckDeadline)
		}
		if got := verdicts(&rv); !slices.Equal(got, []string{"unchecked"}) || rv.Counts != (model.ReviewCounts{Unchecked: 1}) {
			t.Fatalf("verdicts %q counts %+v, want one unchecked finding", got, rv.Counts)
		}
	})

	t.Run("no time left", func(t *testing.T) {
		m := newChainModel(t)
		s := New(Config{Root: t.TempDir(), Model: m.client()})
		rv := s.reviewChange(jobCtx(t, 500*time.Millisecond), "a1", in)
		if rv.Status != "error" || rv.Message != msgNoTimeLeft || rv.CheckStatus != "skipped" {
			t.Fatalf("review %+v, want error %q", rv, msgNoTimeLeft)
		}
		if n := m.Names(); len(n) != 0 {
			t.Fatalf("model calls %v, want none", n)
		}
	})
}

// drainNow drains s and fails the test if that takes more than 30 s.
func drainNow(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// A shutdown during the review call ends the analysis as interrupted. The
// hypotheses are kept, and the review is an error.
func TestDrainDuringReviewEndsIt(t *testing.T) {
	m := newChainModel(t)
	s, srv, base := chainServer(t, m, nil)
	m.hold(t, "ChangeReview")
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", base, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	m.waitFor(t, "ChangeReview")
	drainNow(t, s)
	var got Analysis
	do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got)
	if got.Status != "error" || got.Error != msgInterrupted {
		t.Fatalf("status %q error %q, want error %q", got.Status, got.Error, msgInterrupted)
	}
	if got.Model == nil || got.Model.Status != "ok" {
		t.Fatalf("hypotheses lost: %+v", got.Model)
	}
	rv := got.Review
	if rv == nil || rv.Status != "error" || rv.Message != msgReviewEnded || rv.CheckStatus != "skipped" || rv.CheckMessage != "no review to check" {
		t.Fatalf("review %+v, want error %q", rv, msgReviewEnded)
	}
}

// A shutdown during the check call keeps the finished review, with every
// finding unchecked.
func TestDrainDuringCheckKeepsReview(t *testing.T) {
	m := newChainModel(t)
	s, srv, base := chainServer(t, m, nil)
	m.hold(t, "FindingChecks")
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", base, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	m.waitFor(t, "FindingChecks")
	drainNow(t, s)
	var got Analysis
	do(t, "GET", srv.URL+"/api/analyses/"+a.ID, nil, &got)
	if got.Status != "error" || got.Error != msgInterrupted || got.Model == nil || got.Model.Status != "ok" {
		t.Fatalf("status %q error %q model %+v", got.Status, got.Error, got.Model)
	}
	rv := got.Review
	if rv == nil || rv.Status != "ok" || rv.Summary != "one finding" || len(rv.Findings) != 1 || rv.CheckStatus != "error" || rv.CheckMessage != msgReviewEnded {
		t.Fatalf("review %+v, want the ok review with check_status error %q", rv, msgReviewEnded)
	}
	if got := verdicts(rv); !slices.Equal(got, []string{"unchecked"}) || rv.Counts != (model.ReviewCounts{Unchecked: 1}) {
		t.Fatalf("verdicts %q counts %+v, want one unchecked finding", got, rv.Counts)
	}
}

// The commits of the change end the evidence, newest first, and both model
// calls receive them.
func TestCommitItemsReachEvidenceAndModels(t *testing.T) {
	m := newChainModel(t)
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.Model = m.client()
	})
	sh := shas["r1"]
	a := plAnalyse(t, srv.URL, "r1", sh.Baseline, "slow")
	if a.Status != "done" {
		t.Fatalf("status %q error %q", a.Status, a.Error)
	}
	want := []string{
		"commit " + sh.Bug[:7] + ": refactor: clarify page end index",
		"commit " + sh.Refactor[:7] + ": feat: collapse whitespace in titles",
	}
	var commits []evidence.Item
	for _, e := range a.Evidence {
		if e.Type == "commit" {
			commits = append(commits, e)
		}
	}
	n := len(a.Evidence)
	if len(commits) != 2 || n < 2 || a.Evidence[n-2].Title != want[0] || a.Evidence[n-1].Title != want[1] {
		t.Fatalf("commit items %+v, want the last two items titled %q", commits, want)
	}
	for _, e := range commits {
		if e.File != "" || e.LineStart != 0 || e.LineEnd != 0 {
			t.Errorf("commit item %s has a location: %+v", e.ID, e)
		}
	}
	for _, name := range []string{"RegressionHypotheses", "ChangeReview"} {
		user := m.user(t, name)
		for _, subject := range []string{"refactor: clarify page end index", "feat: collapse whitespace in titles"} {
			if !strings.Contains(user, subject) {
				t.Errorf("the %s request lacks the commit subject %q", name, subject)
			}
		}
	}
}

// git failing to list the commit messages adds a warning and no commit
// item; output over the cap adds the other warning.
func TestUnreadableCommitsAddAWarning(t *testing.T) {
	old := commitMessages
	t.Cleanup(func() { commitMessages = old })
	var mu sync.Mutex
	var asked []string
	record := func(dir, base, cur string, n int) {
		mu.Lock()
		asked = append(asked, fmt.Sprintf("%s..%s n=%d", base, cur, n))
		mu.Unlock()
	}

	t.Run("unreadable", func(t *testing.T) {
		commitMessages = func(ctx context.Context, dir, base, cur string, n int) ([]gitx.CommitMessage, bool, error) {
			record(dir, base, cur, n)
			return nil, false, errors.New("git log failed")
		}
		_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
		a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
		if a.Status != "done" || a.Error != "" {
			t.Fatalf("status %q error %q, want done", a.Status, a.Error)
		}
		if !slices.Equal(a.Warnings, []string{warnCommitsUnread}) {
			t.Fatalf("warnings %q, want [%q]", a.Warnings, warnCommitsUnread)
		}
		for _, e := range a.Evidence {
			if e.Type == "commit" {
				t.Fatalf("commit item without commit messages: %+v", e)
			}
		}
		mu.Lock()
		got := strings.Join(asked, "|")
		mu.Unlock()
		if want := shas["r1"].Baseline + ".." + shas["r1"].Bug + " n=10"; got != want {
			t.Fatalf("commit messages asked for %q, want %q", got, want)
		}
	})

	t.Run("cut", func(t *testing.T) {
		commitMessages = func(ctx context.Context, dir, base, cur string, n int) ([]gitx.CommitMessage, bool, error) {
			cs, _, err := gitx.CommitMessages(ctx, dir, base, cur, n)
			if len(cs) > 1 {
				cs = cs[:1]
			}
			return cs, true, err
		}
		_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
		a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
		if a.Status != "done" || !slices.Equal(a.Warnings, []string{warnCommitsCut}) {
			t.Fatalf("status %q warnings %q, want done and [%q]", a.Status, a.Warnings, warnCommitsCut)
		}
		var titles []string
		for _, e := range a.Evidence {
			if e.Type == "commit" {
				titles = append(titles, e.Title)
			}
		}
		if want := "commit " + shas["r1"].Bug[:7] + ": refactor: clarify page end index"; len(titles) != 1 || titles[0] != want {
			t.Fatalf("commit items %q, want [%q]", titles, want)
		}
	})
}

// Each row of GET /api/analyses carries the counts of its review.
func TestListRowsCarryReviewCounts(t *testing.T) {
	m := newChainModel(t)
	_, srv, base := chainServer(t, m, nil)
	a := plAnalyse(t, srv.URL, "r1", base, "slow")
	if a.Review == nil {
		t.Fatalf("no review: %+v", a)
	}
	want, _ := json.Marshal(a.Review.Counts)
	for _, row := range rawRows(t, srv.URL) {
		if string(row["id"]) == `"`+a.ID+`"` {
			if string(row["review_counts"]) != string(want) || string(want) != `{"confirmed":1,"disputed":0,"refuted":0,"unchecked":0}` {
				t.Fatalf("review_counts %s, want %s", row["review_counts"], want)
			}
			return
		}
	}
	t.Fatalf("no row for %s", a.ID)
}

// An analysis stored before v4 has no review key. It loads with review
// null, and its row has review_counts null.
func TestStoredAnalysisWithoutReviewLoads(t *testing.T) {
	data := t.TempDir()
	id := store.NewID()
	v3 := fmt.Sprintf(`{"id":%q,"status":"done","repo":"r1","baseline":"%s","current":"%s",`+
		`"command":{"id":"node-test","argv":["node","--test"]},"baseline_run":null,"current_run":null,`+
		`"changed_files":[],"evidence":[],"model":{"status":"disabled","model_id":"muse-spark-1.3","message":"x","rejected":[]},`+
		`"verifications":[],"created_at":"2026-11-03T10:00:00Z","finished_at":"2026-11-03T10:01:00Z","notification":null}`,
		id, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err := store.New(data).Save(id, []byte(v3)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{Root: t.TempDir(), Model: &model.Client{}, DataDir: data}))
	defer srv.Close()
	got := rawAnalysis(t, srv.URL, id)
	if string(got["status"]) != `"done"` || string(got["review"]) != "null" {
		t.Fatalf("stored v3 analysis: status %s review %s, want done and null", got["status"], got["review"])
	}
	rows := rawRows(t, srv.URL)
	if len(rows) != 1 || string(rows[0]["review_counts"]) != "null" {
		t.Fatalf("rows %v, want one row with review_counts null", rows)
	}
}

// reviewNotifier records each payload with the model calls made before it.
type reviewNotifier struct {
	m     *chainModel
	mu    sync.Mutex
	calls []reviewCall
}

type reviewCall struct {
	names   []string
	payload notify.Payload
}

func (n *reviewNotifier) Send(_ context.Context, _, _ string, p notify.Payload) notify.Result {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, reviewCall{n.m.Names(), p})
	return notify.Result{Delivered: true, Attempts: 1, StatusCode: 200}
}

func (n *reviewNotifier) Calls() []reviewCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]reviewCall{}, n.calls...)
}

// The webhook is sent once, after the review chain, and carries the review.
func TestWebhookCarriesReviewOnce(t *testing.T) {
	m := newChainModel(t)
	n := &reviewNotifier{m: m}
	s, srv, base := chainServer(t, m, func(c *Config) {
		c.WebhookSecret = testWebhookSecret
		c.Notifier = n
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", notifyBody("r1", base, "slow", secretNotifyURL, "json"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	waitNotification(t, srv.URL, a.ID, "delivered")
	waitIdle(t, s)
	calls := n.Calls()
	if len(calls) != 1 {
		t.Fatalf("notifier called %d times, want once", len(calls))
	}
	if got := strings.Join(calls[0].names, ","); got != "RegressionHypotheses,ChangeReview,FindingChecks" {
		t.Fatalf("the webhook was sent after the model calls %s, want after all three", got)
	}
	r := calls[0].payload.Review
	if calls[0].payload.Status != "done" || r == nil || r.Status != "ok" || r.Confirmed != 1 || r.Disputed != 0 || r.Refuted != 0 || r.Unchecked != 0 {
		t.Fatalf("payload status %q review %+v", calls[0].payload.Status, r)
	}
	if f := r.TopFinding; f == nil || f.ID != "R1" || f.Lens != "code" || f.Severity != "high" || f.Title != "Last page drops one item" ||
		f.File != "src/pagination.ts" || f.LineStart < 1 || f.LineEnd != f.LineStart || f.Verdict != "confirmed" {
		t.Fatalf("top finding %+v", r.TopFinding)
	}
}

// closeReview gives an analysis that ends without the review chain's result
// the review of the contract's rules 5 to 7, and never changes the outcome
// it was given.
func TestCloseReview(t *testing.T) {
	keyed := New(Config{Root: t.TempDir(), Model: &model.Client{APIKey: "k", Model: "m"}})
	keyless := New(Config{Root: t.TempDir(), Model: &model.Client{Model: "m"}})
	owing := keyed.cfg.Model.NoReview("ok", "")
	owing.CheckStatus, owing.CheckMessage = "", ""
	owing.Findings = []model.Finding{{ID: "R1", Lens: "code", Severity: "high", Title: "t", EvidenceIDs: []string{"E1"},
		Check: model.Check{Verdict: "unchecked", Reason: "", EvidenceIDs: []string{}}}}
	owing.Counts = model.ReviewCounts{Unchecked: 1}
	finished := owing.EndCheck("ok", "")
	finished.Findings[0].Check = model.Check{Verdict: "confirmed", Reason: "r", EvidenceIDs: []string{"E1"}}
	finished.Counts = model.ReviewCounts{Confirmed: 1}

	cases := []struct {
		name          string
		s             *Server
		owed          bool
		done          *model.Review
		status, msg   string
		check, chkMsg string
		verdicts      []string
	}{
		{"no evidence", keyed, false, &finished, "skipped", msgReviewSkipped, "skipped", "no review to check", []string{}},
		{"no key", keyless, true, nil, "disabled", model.MsgNotConfigured, "skipped", "no review to check", []string{}},
		{"review unfinished", keyed, true, nil, "error", msgReviewEnded, "skipped", "no review to check", []string{}},
		{"check unfinished", keyed, true, &owing, "ok", "", "error", msgReviewEnded, []string{"unchecked"}},
		{"finished", keyed, true, &finished, "ok", "", "ok", "", []string{"confirmed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var before []byte
			if c.done != nil {
				before, _ = json.Marshal(c.done)
			}
			rv := c.s.closeReview(&Analysis{Status: "error", reviewOwed: c.owed, reviewDone: c.done})
			if rv == nil || rv.Status != c.status || rv.Message != c.msg || rv.CheckStatus != c.check || rv.CheckMessage != c.chkMsg || rv.ModelID != "m" {
				t.Fatalf("review %+v, want %s %q, check %s %q", rv, c.status, c.msg, c.check, c.chkMsg)
			}
			if got := verdicts(rv); !slices.Equal(got, c.verdicts) {
				t.Fatalf("verdicts %q, want %q", got, c.verdicts)
			}
			if rv == c.done {
				t.Fatal("closeReview returned the outcome it was given, not a copy")
			}
			if c.done != nil {
				if after, _ := json.Marshal(c.done); string(after) != string(before) {
					t.Fatalf("closeReview changed the outcome it was given:\n%s\n%s", before, after)
				}
			}
		})
	}
}
