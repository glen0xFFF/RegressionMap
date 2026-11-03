package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"regressionmap/internal/fixture"
	"regressionmap/internal/model"
)

type env struct {
	srv  *httptest.Server
	root string
	shas fixture.Shas
}

func setup(t *testing.T, mc *model.Client) env {
	t.Helper()
	root := t.TempDir()
	s, err := fixture.Build(filepath.Join(root, "pagination-demo"))
	if err != nil {
		t.Fatal(err)
	}
	if mc == nil {
		mc = &model.Client{Model: "muse-spark-1.3"}
	}
	h := New(Config{Root: root, Model: mc})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return env{srv, root, s}
}

func do(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func waitDone(t *testing.T, e env, id string) Analysis {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var a Analysis
		do(t, "GET", e.srv.URL+"/api/analyses/"+id, nil, &a)
		if a.Status != "running" {
			return a
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("analysis did not finish")
	return Analysis{}
}

func TestHealthNeverReturnsKey(t *testing.T) {
	e := setup(t, &model.Client{APIKey: "secret-key-123", Model: "muse-spark-1.3"})
	resp, _ := http.Get(e.srv.URL + "/api/health")
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if strings.Contains(buf.String(), "secret-key-123") || !strings.Contains(buf.String(), `"model_configured":true`) {
		t.Fatal(buf.String())
	}
}

func TestReposCommitsCommands(t *testing.T) {
	e := setup(t, nil)
	var repos struct{ Repos []struct{ Name string } }
	do(t, "GET", e.srv.URL+"/api/repos", nil, &repos)
	if len(repos.Repos) != 1 || repos.Repos[0].Name != "pagination-demo" {
		t.Fatalf("%+v", repos)
	}
	var cs struct{ Commits []struct{ SHA string } }
	if code := do(t, "GET", e.srv.URL+"/api/repos/pagination-demo/commits", nil, &cs); code != 200 || len(cs.Commits) != 3 {
		t.Fatalf("%d %+v", code, cs)
	}
	if code := do(t, "GET", e.srv.URL+"/api/repos/..%2F..%2Fwindows/commits", nil, nil); code != 400 && code != 404 {
		t.Fatalf("traversal code %d", code)
	}
	var cmds struct{ Commands []struct{ ID string } }
	do(t, "GET", e.srv.URL+"/api/commands", nil, &cmds)
	if len(cmds.Commands) == 0 || cmds.Commands[0].ID != "node-test" {
		t.Fatalf("%+v", cmds)
	}
}

func TestCreateAnalysisRejectsBadInput(t *testing.T) {
	e := setup(t, nil)
	cases := []map[string]any{
		{"repo": "../pagination-demo", "baseline": e.shas.Baseline, "command_id": "node-test"},
		{"repo": "nope", "baseline": e.shas.Baseline, "command_id": "node-test"},
		{"repo": "pagination-demo", "baseline": "--exec=calc", "command_id": "node-test"},
		{"repo": "pagination-demo", "baseline": e.shas.Baseline, "command_id": "rm -rf /"},
	}
	for _, c := range cases {
		var er struct{ Error struct{ Code string } }
		if code := do(t, "POST", e.srv.URL+"/api/analyses", c, &er); code != 400 || er.Error.Code == "" {
			t.Errorf("%v => %d %+v", c, code, er)
		}
	}
}

func TestFullFlowWithModelDisabled(t *testing.T) {
	e := setup(t, nil)
	var a Analysis
	code := do(t, "POST", e.srv.URL+"/api/analyses", map[string]any{"repo": "pagination-demo", "baseline": e.shas.Baseline[:8], "command_id": "node-test"}, &a)
	if code != 202 || a.ID == "" {
		t.Fatalf("%d %+v", code, a)
	}
	a = waitDone(t, e, a.ID)
	if a.Status != "done" {
		t.Fatalf("status %s: %s", a.Status, a.Error)
	}
	if !a.BaselineRun.Passed || a.CurrentRun.Passed || a.CurrentRun.ExitCode == 0 {
		t.Fatalf("baseline=%+v current exit=%d", a.BaselineRun.Passed, a.CurrentRun.ExitCode)
	}
	if a.Current != e.shas.Bug || a.Baseline != e.shas.Baseline {
		t.Fatalf("shas %s %s", a.Baseline, a.Current)
	}
	var hasHunk, hasFailure, hasFrame bool
	for _, ev := range a.Evidence {
		hasHunk = hasHunk || (ev.Type == "diff_hunk" && ev.File == "src/pagination.ts")
		hasFailure = hasFailure || (ev.Type == "test_failure" && ev.File == "test/pagination.test.ts" && ev.LineStart == 8)
		hasFrame = hasFrame || (ev.Type == "stack_frame" && strings.Contains(ev.Excerpt, "paginate(items, 1, 3)"))
	}
	if !hasHunk || !hasFailure || !hasFrame {
		t.Fatalf("evidence hunk=%v failure=%v frame=%v: %+v", hasHunk, hasFailure, hasFrame, a.Evidence)
	}
	if a.Model.Status != "disabled" {
		t.Fatalf("model %+v", a.Model)
	}

	// verification requires literal approval
	var er struct{ Error struct{ Code string } }
	if code := do(t, "POST", e.srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "node-test"}, &er); code != 400 || er.Error.Code != "not_approved" {
		t.Fatalf("unapproved: %d %+v", code, er)
	}
	if code := do(t, "POST", e.srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "bash", "approved": true}, nil); code != 400 {
		t.Fatalf("unknown command: %d", code)
	}

	var v Verification
	do(t, "POST", e.srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "node-test", "approved": true}, &v)
	if v.Run.Passed {
		t.Fatal("unfixed tree must fail")
	}
	if err := fixture.ApplyFix(filepath.Join(e.root, "pagination-demo")); err != nil {
		t.Fatal(err)
	}
	do(t, "POST", e.srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "node-test", "approved": true}, &v)
	if !v.Run.Passed || v.Run.ExitCode != 0 {
		t.Fatalf("fixed tree must pass: %+v", v.Run)
	}
	a = waitDone(t, e, a.ID)
	if len(a.Verifications) != 2 {
		t.Fatalf("verifications %d", len(a.Verifications))
	}
}

func TestModelHypothesesCiteRealEvidence(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"summary":"s","limitations":[],"hypotheses":[{"rank":1,"title":"end index off by one","file":"src/pagination.ts","line_start":11,"line_end":11,"reason":"r","supporting_evidence_ids":["E1"],"counter_evidence_ids":[],"verification_suggestion":"rerun","confidence_label":"high"},{"rank":2,"title":"made up","file":"src/pagination.ts","line_start":1,"line_end":1,"reason":"r","supporting_evidence_ids":["E404"],"counter_evidence_ids":[],"verification_suggestion":"x","confidence_label":"low"}]}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer fake.Close()
	e := setup(t, &model.Client{BaseURL: fake.URL, APIKey: "k", Model: "muse-spark-1.3"})
	var a Analysis
	do(t, "POST", e.srv.URL+"/api/analyses", map[string]any{"repo": "pagination-demo", "baseline": e.shas.Baseline, "command_id": "node-test"}, &a)
	a = waitDone(t, e, a.ID)
	if a.Model.Status != "ok" || len(a.Model.Result.Hypotheses) != 1 || len(a.Model.Rejected) != 1 {
		t.Fatalf("%+v", a.Model)
	}
}
