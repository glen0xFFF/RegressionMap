package api

// Tests of the analysis pipeline: size limits and warnings, the model
// request, git failures after the test runs, and the test environment.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"regressionmap/internal/model"
)

// plGit runs git in dir with a fixed identity, feeding it stdin.
func plGit(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// plCommit writes files into the repository at dir and commits them on top
// of HEAD, so they are part of the baseline..HEAD range.
func plCommit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plGit(t, dir, "", "add", "-A")
	plGit(t, dir, "", "commit", "--quiet", "-m", "pipeline test")
}

// plModel is a fake chat-completions endpoint that records every request
// body and answers with content.
type plModel struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newPLModel(t *testing.T, content string) *plModel {
	m := &plModel{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.bodies = append(m.bodies, string(b))
		m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *plModel) client() *model.Client {
	return &model.Client{BaseURL: m.srv.URL, APIKey: "test", Model: "m", MaxAttempts: 1}
}

// request returns the only request body and its user message.
func (m *plModel) request(t *testing.T) (body, user string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) != 1 {
		t.Fatalf("model got %d requests, want 1", len(m.bodies))
	}
	var req struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal([]byte(m.bodies[0]), &req); err != nil || len(req.Messages) != 2 {
		t.Fatalf("model request %v: %s", err, m.bodies[0])
	}
	return m.bodies[0], req.Messages[1].Content
}

func plAnalyse(t *testing.T, url, repo, base, cmd string) Analysis {
	t.Helper()
	var a Analysis
	if code := do(t, "POST", url+"/api/analyses", startBody(repo, base, cmd), &a); code != 202 {
		t.Fatalf("POST /api/analyses: %d", code)
	}
	return pollDone(t, url, a.ID)
}

// The changed_files list sent to the model uses the evidence's path
// redaction, so no secret-looking segment leaves the server and a hypothesis
// that cites the file as the evidence names it is kept. The API keeps the
// real path.
func TestPipelineRedactsChangedFilesSentToModel(t *testing.T) {
	const secretPath = "config/token=supersecretvalue123/app.ts"
	const redacted = "config/token=[REDACTED]/app.ts"
	hyp := map[string]any{"rank": 1, "title": "t", "file": redacted, "line_start": 0, "line_end": 0,
		"reason": "r", "supporting_evidence_ids": []string{"E1"}, "counter_evidence_ids": []string{},
		"verification_suggestion": "v", "confidence_label": "low"}
	content, _ := json.Marshal(map[string]any{"summary": "s", "hypotheses": []any{hyp}, "limitations": []string{}})
	m := newPLModel(t, string(content))
	_, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.Model = m.client()
	})
	plCommit(t, filepath.Join(root, "r1"), map[string]string{secretPath: "export const x = 1;\n"})

	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
	if a.Status != "done" {
		t.Fatalf("status %s: %s", a.Status, a.Error)
	}
	body, user := m.request(t)
	if strings.Contains(body, "supersecretvalue123") {
		t.Errorf("model request carries the secret path segment:\n%s", user)
	}
	if !strings.Contains(user, `"`+redacted+`"`) {
		t.Errorf("model request lacks the redacted changed file:\n%s", user)
	}
	if a.Model == nil || a.Model.Status != "ok" || len(a.Model.Result.Hypotheses) != 1 || a.Model.Result.Hypotheses[0].File != redacted {
		t.Errorf("hypothesis citing the redacted file was not kept: %+v", a.Model)
	}
	found := false
	for _, f := range a.ChangedFiles {
		found = found || f.Path == secretPath
	}
	if !found {
		t.Errorf("API changed_files lost the real path: %+v", a.ChangedFiles)
	}
}

// A diff over 8 MiB and a changed file over 1 MiB no longer fail the
// analysis: both are cut, the analysis says so in warnings, the model input
// states each cut, and the big file keeps its source item, marked as cut.
func TestPipelineCutsBigDiffAndBigFile(t *testing.T) {
	m := newPLModel(t, `{"summary":"s","hypotheses":[],"limitations":[]}`)
	_, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(50)}
		c.Model = m.client()
	})
	line := strings.Repeat("abcdefghij", 9) + "\n" // 91 bytes
	plCommit(t, filepath.Join(root, "r1"), map[string]string{"vendor/big.txt": strings.Repeat(line, (9<<20)/len(line))})

	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
	if a.Status != "done" {
		t.Fatalf("status %s: %s", a.Status, a.Error)
	}
	want := []string{
		"diff larger than 8 MiB: only the first 8 MiB were analysed",
		"1 file(s) larger than 1 MiB were read only in part",
	}
	if strings.Join(a.Warnings, "|") != strings.Join(want, "|") {
		t.Errorf("warnings %q, want %q", a.Warnings, want)
	}
	var src, hunk bool
	for _, e := range a.Evidence {
		switch {
		case e.Type == "source" && e.File == "vendor/big.txt":
			src = e.LineStart == 1 && e.LineEnd == (1<<20)/len(line) && strings.HasSuffix(e.Excerpt, "…[truncated]")
			if !src {
				t.Errorf("source item of the big file: lines %d-%d, excerpt ends %q", e.LineStart, e.LineEnd, e.Excerpt[max(0, len(e.Excerpt)-20):])
			}
		case e.Type == "diff_hunk" && e.File == "vendor/big.txt":
			hunk = true
		}
	}
	if !src || !hunk {
		t.Errorf("evidence for the big file: source=%v hunk=%v", src, hunk)
	}
	_, user := m.request(t)
	for _, w := range want {
		if !strings.Contains(user, "- "+w+"\n") {
			t.Errorf("model input does not state %q:\n%.400s", w, user)
		}
	}
}

// A changed-file list over 1 MiB keeps the entries that fit and the
// analysis goes on. The files are empty, so their diff has no hunks and the
// patch stays far below its own cap.
func TestPipelineCutsLongChangedFileList(t *testing.T) {
	// Long paths keep the file count, and so the checkout, small. Windows
	// needs core.longpaths for them; GIT_CONFIG_* reaches every git call,
	// including the server's, without touching any config file.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.longpaths")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	_, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
	dir := filepath.Join(root, "r1")
	empty := plGit(t, dir, "", "hash-object", "-w", "--stdin")
	prefix := strings.Repeat("a", 200) + "/" + strings.Repeat("b", 200) + "/" + strings.Repeat("c", 200) + "/"
	const n = 2000 // about 1.2 MB of "A\t<path>\n"
	var idx strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&idx, "100644 %s\t%sf%04d.txt\n", empty, prefix, i)
	}
	plGit(t, dir, idx.String(), "update-index", "--add", "--index-info")
	plGit(t, dir, "", "commit", "--quiet", "-m", "many files")

	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
	if a.Status != "done" {
		t.Fatalf("status %s: %s", a.Status, a.Error)
	}
	if want := "changed-file list larger than 1 MiB: only the files that fit were analysed"; len(a.Warnings) != 1 || a.Warnings[0] != want {
		t.Errorf("warnings %q, want [%q]", a.Warnings, want)
	}
	if len(a.ChangedFiles) < 1500 || len(a.ChangedFiles) >= n {
		t.Fatalf("changed_files has %d entries, want the ones that fit in 1 MiB", len(a.ChangedFiles))
	}
	for _, f := range a.ChangedFiles {
		if f.Status != "A" || len(f.Path) != len(prefix)+len("f0000.txt") {
			t.Fatalf("partial entry kept: %+v", f)
		}
	}
	if a.BaselineRun == nil || a.CurrentRun == nil {
		t.Fatal("test runs missing")
	}
}

// A git step that fails after the test runs names its step, and the runs
// the server already made are kept. The test command breaks the
// repository's config on its second run, so every later git call fails.
func TestPipelineGitFailureKeepsRunsAndNamesStep(t *testing.T) {
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "ran-once"))
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		cfgFile := filepath.ToSlash(filepath.Join(c.Root, "r1", ".git", "config"))
		js := fmt.Sprintf(`const fs=require("fs");if(fs.existsSync(%q)){fs.appendFileSync(%q,"\n[broken\n")}else{fs.writeFileSync(%q,"1")}`, marker, cfgFile, marker)
		c.Commands = []Command{{ID: "breaker", Argv: []string{"node", "-e", js}, Description: "breaks git on its second run"}}
	})
	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "breaker")
	if a.Status != "error" || !strings.HasPrefix(a.Error, "changed-file list failed: ") {
		t.Fatalf("status %s error %q, want error starting %q", a.Status, a.Error, "changed-file list failed: ")
	}
	if a.BaselineRun == nil || a.CurrentRun == nil || !a.BaselineRun.Passed || !a.CurrentRun.Passed {
		t.Fatalf("test runs lost after a git failure: baseline=%+v current=%+v", a.BaselineRun, a.CurrentRun)
	}
}

// The same holds one step earlier: when the HEAD worktree cannot be made,
// the error names that step and the finished baseline run is kept.
func TestPipelineWorktreeFailureKeepsBaselineRun(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		cfgFile := filepath.ToSlash(filepath.Join(c.Root, "r1", ".git", "config"))
		js := fmt.Sprintf(`require("fs").appendFileSync(%q,"\n[broken\n")`, cfgFile)
		c.Commands = []Command{{ID: "breaker", Argv: []string{"node", "-e", js}, Description: "breaks git"}}
	})
	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "breaker")
	if a.Status != "error" || !strings.HasPrefix(a.Error, "HEAD worktree failed: ") {
		t.Fatalf("status %s error %q, want error starting %q", a.Status, a.Error, "HEAD worktree failed: ")
	}
	if a.BaselineRun == nil || !a.BaselineRun.Passed || a.CurrentRun != nil {
		t.Fatalf("baseline run lost or current run invented: baseline=%+v current=%+v", a.BaselineRun, a.CurrentRun)
	}
}

// A failing patch step names itself too. A textconv driver that always
// fails breaks "git diff -U3" for .ts files but not the name-status list.
func TestPipelineDiffFailureNamesStep(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.rmfail.textconv")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	_, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(50)} })
	if err := os.WriteFile(filepath.Join(root, "r1", ".gitattributes"), []byte("*.ts diff=rmfail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "slow")
	if a.Status != "error" || !strings.HasPrefix(a.Error, "diff failed: ") {
		t.Fatalf("status %s error %q, want error starting %q", a.Status, a.Error, "diff failed: ")
	}
	if a.BaselineRun == nil || a.CurrentRun == nil {
		t.Fatalf("test runs lost after a git failure: baseline=%+v current=%+v", a.BaselineRun, a.CurrentRun)
	}
}

// TEST_ENV_ALLOW names reach test runs and verification runs although the
// suffix rule would withhold them; other secret-suffixed names stay
// withheld, and git (here a post-checkout hook) never receives either.
func TestPipelineTestEnvAllowReachesTestRunsNotGit(t *testing.T) {
	t.Setenv("RMPIPE_DB_PASSWORD", "allowed-value-123")
	t.Setenv("RMPIPE_OTHER_TOKEN", "withheld-value-456")
	js := `process.exit(process.env.RMPIPE_DB_PASSWORD==="allowed-value-123"&&process.env.RMPIPE_OTHER_TOKEN===undefined?0:3)`
	_, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{{ID: "envcheck", Argv: []string{"node", "-e", js}, Description: "checks its environment"}}
		c.TestEnvAllow = []string{"RMPIPE_DB_PASSWORD"}
	})
	dump := filepath.Join(t.TempDir(), "hook-env.txt")
	hook := "#!/bin/sh\nenv >> '" + filepath.ToSlash(dump) + "'\n"
	if err := os.WriteFile(filepath.Join(root, "r1", ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	a := plAnalyse(t, srv.URL, "r1", shas["r1"].Baseline, "envcheck")
	if a.Status != "done" || a.BaselineRun == nil || a.CurrentRun == nil {
		t.Fatalf("status %s: %s", a.Status, a.Error)
	}
	if !a.BaselineRun.Passed || !a.CurrentRun.Passed {
		t.Errorf("test runs did not get the allowed name: baseline exit %d, current exit %d", a.BaselineRun.ExitCode, a.CurrentRun.ExitCode)
	}
	var v Verification
	if code := do(t, "POST", srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "envcheck", "approved": true}, &v); code != 200 || !v.Run.Passed {
		t.Errorf("verification run did not get the allowed name: http %d exit %d", code, v.Run.ExitCode)
	}
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("post-checkout hook did not run: %v", err)
	}
	if env := string(b); strings.Contains(env, "RMPIPE_DB_PASSWORD") || strings.Contains(env, "RMPIPE_OTHER_TOKEN") {
		t.Errorf("git hook received a withheld variable")
	}
}
