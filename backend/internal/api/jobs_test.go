package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"regressionmap/internal/evidence"
	"regressionmap/internal/fixture"
	"regressionmap/internal/gitx"
	"regressionmap/internal/model"
	"regressionmap/internal/proc"
	"regressionmap/internal/store"
)

type apiErr struct {
	Error struct{ Code, Message string }
}

// slowCmd is a fake test command that just sleeps, so a job stays "running".
func slowCmd(ms int) Command {
	return Command{ID: "slow", Argv: []string{"node", "-e", fmt.Sprintf("setTimeout(()=>{},%d)", ms)}, Description: "sleep"}
}

// newServer builds one fixture repo per name and a Server over them. Cleanup
// waits until no analysis or verification is running, so temp dirs are not
// removed under a live git or node process.
func newServer(t *testing.T, repos []string, mut func(*Config)) (*Server, *httptest.Server, string, map[string]fixture.Shas) {
	t.Helper()
	root := t.TempDir()
	shas := map[string]fixture.Shas{}
	for _, r := range repos {
		sh, err := fixture.Build(filepath.Join(root, r))
		if err != nil {
			t.Fatal(err)
		}
		shas[r] = sh
	}
	cfg := Config{Root: root, Model: &model.Client{Model: "muse-spark-1.3"}}
	if mut != nil {
		mut(&cfg)
	}
	s := New(cfg)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { waitIdle(t, s) })
	return s, srv, root, shas
}

func waitIdle(t *testing.T, s *Server) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		idle := s.active == 0 && len(s.verifying) == 0 && s.delivering == 0
		s.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("server did not become idle")
}

func pollDone(t *testing.T, url, id string) Analysis {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var a Analysis
		do(t, "GET", url+"/api/analyses/"+id, nil, &a)
		if a.Status != "running" {
			return a
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("analysis did not finish")
	return Analysis{}
}

func startBody(repo, base, cmd string) map[string]any {
	return map[string]any{"repo": repo, "baseline": base, "command_id": cmd}
}

func TestConfigDefaults(t *testing.T) {
	s := New(Config{Root: t.TempDir(), Model: &model.Client{}})
	if s.cfg.MaxConcurrent != 2 || s.cfg.MaxJobs != 100 || s.cfg.JobTimeout != 5*time.Minute {
		t.Fatalf("defaults: concurrent=%d jobs=%d timeout=%v", s.cfg.MaxConcurrent, s.cfg.MaxJobs, s.cfg.JobTimeout)
	}
	if s.store != nil {
		t.Fatal("empty DataDir must mean in-memory only")
	}
}

func TestSecondAnalysisOnSameRepoIs409(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(1500)} })
	var a Analysis
	before := time.Now().Add(-2 * time.Second)
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("first POST %d", code)
	}
	ts, err := time.Parse(time.RFC3339, a.CreatedAt)
	if err != nil || ts.Before(before) || ts.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("created_at %q: %v", a.CreatedAt, err)
	}
	if !store.ValidID(a.ID) {
		t.Fatalf("id %q is not a store-safe id", a.ID)
	}
	var er apiErr
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &er); code != 409 || er.Error.Code != "analysis_running" {
		t.Fatalf("second POST %d %+v", code, er)
	}
}

func TestGlobalCapIs429(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.Commands = []Command{slowCmd(1500)}
		c.MaxConcurrent = 1
	})
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), nil); code != 202 {
		t.Fatalf("first POST %d", code)
	}
	var er apiErr
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r2", shas["r2"].Baseline, "slow"), &er); code != 429 || er.Error.Code != "too_many_analyses" {
		t.Fatalf("second POST %d %+v", code, er)
	}
}

func TestVerifyWhileAnalysisRunningIs409(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(1500), DefaultCommands[0]} })
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	var er apiErr
	code := do(t, "POST", srv.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "node-test", "approved": true}, &er)
	if code != 409 || er.Error.Code != "repo_busy" {
		t.Fatalf("verify %d %+v", code, er)
	}
}

func TestConcurrentVerificationOnSameRepoIs409(t *testing.T) {
	s, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) { c.Commands = []Command{slowCmd(1500)} })
	s.mu.Lock()
	s.insertLocked(&Analysis{ID: "adone1", Status: "done", Repo: "r1", Verifications: []Verification{}})
	s.mu.Unlock()

	body := []byte(`{"command_id":"slow","approved":true}`)
	first := make(chan int, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/api/analyses/adone1/verify", "application/json", bytes.NewReader(body))
		if err != nil {
			first <- -1
			return
		}
		resp.Body.Close()
		first <- resp.StatusCode
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		busy := s.verifying["r1"]
		s.mu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first verification never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var er apiErr
	if code := do(t, "POST", srv.URL+"/api/analyses/adone1/verify", map[string]any{"command_id": "slow", "approved": true}, &er); code != 409 || er.Error.Code != "repo_busy" {
		t.Fatalf("second verify %d %+v", code, er)
	}
	er = apiErr{}
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &er); code != 409 || er.Error.Code != "repo_busy" {
		t.Fatalf("analysis during verification %d %+v", code, er)
	}
	if code := <-first; code != 200 {
		t.Fatalf("first verify %d", code)
	}
	var a Analysis
	do(t, "GET", srv.URL+"/api/analyses/adone1", nil, &a)
	if len(a.Verifications) != 1 {
		t.Fatalf("verifications %d", len(a.Verifications))
	}
}

func TestEvictsOldestFinishedFirst(t *testing.T) {
	s := New(Config{Root: t.TempDir(), Model: &model.Client{}, MaxJobs: 2})
	add := func(id, status string) {
		s.mu.Lock()
		s.insertLocked(&Analysis{ID: id, Status: status})
		s.mu.Unlock()
	}
	ids := func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := []string{}
		for _, id := range []string{"a1", "a2", "a3", "a4"} {
			if _, ok := s.jobs[id]; ok {
				out = append(out, id)
			}
		}
		return out
	}
	add("a1", "done")
	add("a2", "running")
	add("a3", "done")
	if got := strings.Join(ids(), ","); got != "a2,a3" {
		t.Fatalf("after 3 inserts: %s", got)
	}
	add("a4", "error")
	if got := strings.Join(ids(), ","); got != "a2,a4" {
		t.Fatalf("after 4 inserts: %s (running job must not be evicted)", got)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	if code := do(t, "GET", srv.URL+"/api/analyses/a1", nil, nil); code != 404 {
		t.Fatalf("evicted analysis without store: %d", code)
	}
}

func TestJobTimeoutEndsWithError(t *testing.T) {
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(20000)}
		c.JobTimeout = 400 * time.Millisecond
	})
	var a Analysis
	start := time.Now()
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	a = pollDone(t, srv.URL, a.ID)
	if a.Status != "error" || a.Error != "analysis timed out" {
		t.Fatalf("status %q error %q", a.Status, a.Error)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("timeout took %v", el)
	}
	// The repo slot is released, so a new analysis is accepted.
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("POST after timeout %d", code)
	}
}

func TestPersistenceFallbackAfterRestart(t *testing.T) {
	data := t.TempDir()
	_, srv1, root, shas := newServer(t, []string{"r1"}, func(c *Config) { c.DataDir = data })
	var a Analysis
	if code := do(t, "POST", srv1.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "node-test"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	a = pollDone(t, srv1.URL, a.ID)
	if a.Status != "done" {
		t.Fatalf("status %s %s", a.Status, a.Error)
	}
	if code := do(t, "POST", srv1.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "node-test", "approved": true}, nil); code != 200 {
		t.Fatalf("verify %d", code)
	}
	p := filepath.Join(data, "analyses", a.ID+".json")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("not persisted: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", st.Mode().Perm())
	}

	s2 := New(Config{Root: root, Model: &model.Client{Model: "muse-spark-1.3"}, DataDir: data})
	srv2 := httptest.NewServer(s2)
	defer srv2.Close()
	defer waitIdle(t, s2)
	var b Analysis
	if code := do(t, "GET", srv2.URL+"/api/analyses/"+a.ID, nil, &b); code != 200 {
		t.Fatalf("GET after restart %d", code)
	}
	if b.Status != "done" || b.CreatedAt != a.CreatedAt || len(b.Verifications) != 1 || len(b.Evidence) != len(a.Evidence) || b.BaselineRun == nil || b.Model == nil {
		t.Fatalf("restored analysis differs: %+v", b)
	}
	if code := do(t, "GET", srv2.URL+"/api/analyses/"+store.NewID(), nil, nil); code != 404 {
		t.Fatalf("unknown id %d", code)
	}
	for _, bad := range []string{"..%2F..%2Fescape", "a1%2F..%2F..%2Fx", "A1"} {
		if code := do(t, "GET", srv2.URL+"/api/analyses/"+bad, nil, nil); code != 404 && code != 400 {
			t.Fatalf("traversal id %q => %d", bad, code)
		}
	}
	// A verification on the restored analysis is appended and persisted.
	if code := do(t, "POST", srv2.URL+"/api/analyses/"+a.ID+"/verify", map[string]any{"command_id": "node-test", "approved": true}, nil); code != 200 {
		t.Fatalf("verify after restart %d", code)
	}
	raw, err := store.New(data).Load(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var c Analysis
	if err := json.Unmarshal(raw, &c); err != nil || len(c.Verifications) != 2 {
		t.Fatalf("persisted verifications: %v %d", err, len(c.Verifications))
	}
}

func TestStoredRunningAnalysisIsReportedInterrupted(t *testing.T) {
	data := t.TempDir()
	id := store.NewID()
	b, _ := json.Marshal(Analysis{ID: id, Status: "running", Repo: "r1", CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := store.New(data).Save(id, b); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Root: t.TempDir(), Model: &model.Client{}, DataDir: data})
	srv := httptest.NewServer(s)
	defer srv.Close()
	var a Analysis
	if code := do(t, "GET", srv.URL+"/api/analyses/"+id, nil, &a); code != 200 || a.Status != "error" || !strings.Contains(a.Error, "interrupted") {
		t.Fatalf("%d %+v", code, a)
	}
}

func TestListAnalysesNewestFirstMax50SummaryOnly(t *testing.T) {
	data := t.TempDir()
	st := store.New(data)
	var ids []string
	for i := 0; i < 55; i++ {
		id := store.NewID()
		ids = append(ids, id)
		a := Analysis{ID: id, Status: "done", Repo: "r1", Baseline: "b", Current: "c",
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			CurrentRun: &proc.Result{Passed: i%2 == 0}, Evidence: []evidence.Item{{ID: "E1"}}}
		b, _ := json.Marshal(a)
		if err := st.Save(id, b); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Config{Root: t.TempDir(), Model: &model.Client{}, DataDir: data})
	mem := &Analysis{ID: store.NewID(), Status: "running", Repo: "r2", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	// The in-memory copy of a stored analysis wins over the file.
	shadow := &Analysis{ID: ids[54], Status: "error", Repo: "r1", Error: "x"}
	s.mu.Lock()
	s.insertLocked(shadow)
	s.insertLocked(mem)
	s.mu.Unlock()
	srv := httptest.NewServer(s)
	defer srv.Close()

	var out struct {
		Analyses []map[string]json.RawMessage `json:"analyses"`
	}
	if code := do(t, "GET", srv.URL+"/api/analyses", nil, &out); code != 200 {
		t.Fatalf("GET list %d", code)
	}
	if len(out.Analyses) != 50 {
		t.Fatalf("len %d, want 50", len(out.Analyses))
	}
	id := func(i int) string {
		var s string
		json.Unmarshal(out.Analyses[i]["id"], &s)
		return s
	}
	if id(0) != mem.ID || id(1) != ids[54] || id(2) != ids[53] || id(49) != ids[6] {
		t.Fatalf("order: %s %s %s %s", id(0), id(1), id(2), id(49))
	}
	for i := 1; i < 50; i++ {
		if id(i) >= id(i-1) {
			t.Fatalf("not newest first at %d", i)
		}
	}
	allowed := map[string]bool{"id": true, "repo": true, "status": true, "baseline": true, "current": true, "created_at": true, "current_passed": true}
	for _, a := range out.Analyses {
		for k := range a {
			if !allowed[k] {
				t.Fatalf("non-summary field %q in list", k)
			}
		}
		if len(a) != len(allowed) {
			t.Fatalf("missing summary fields: %v", a)
		}
	}
	if string(out.Analyses[0]["current_passed"]) != "null" || string(out.Analyses[1]["status"]) != `"error"` || string(out.Analyses[2]["current_passed"]) != "false" || string(out.Analyses[3]["current_passed"]) != "true" {
		t.Fatalf("summary values: %s %s %s %s", out.Analyses[0]["current_passed"], out.Analyses[1]["status"], out.Analyses[2]["current_passed"], out.Analyses[3]["current_passed"])
	}

	// Memory-only server with nothing in it returns an empty array, not null.
	empty := httptest.NewServer(New(Config{Root: t.TempDir(), Model: &model.Client{}}))
	defer empty.Close()
	resp, err := http.Get(empty.URL + "/api/analyses")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(buf.String()) != `{"analyses":[]}` {
		t.Fatalf("empty list body %s", buf.String())
	}
}

func TestCleanupPrunesWorktreesAndOldTempDirs(t *testing.T) {
	s, _, root, shas := newServer(t, []string{"r1"}, nil)
	tmp := t.TempDir()
	s.tempDir = tmp
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Join(p, "wt"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(p, "wt", "f.txt"), []byte("x"), 0o600)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("regressionmap-wt-111", old)
	fresh := mk("regressionmap-wt-222", time.Now())
	other := mk("unrelated-333", old)

	ctx := context.Background()
	repo := filepath.Join(root, "r1")
	wt, _, err := gitx.Worktree(ctx, repo, shas["r1"].Baseline)
	if err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Dir(wt)) // crash leftover: registration without a directory

	if err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp dir kept: %v", err)
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "worktree "); n != 1 {
		t.Fatalf("worktrees after Cleanup: %d\n%s", n, out)
	}
}

// Aliases of one repository directory (different case, a trailing dot on
// Windows) must share its busy state: they name the same working tree.
func TestRepoAliasesShareBusyState(t *testing.T) {
	s, srv, root, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.Commands = []Command{slowCmd(2000)}
		c.MaxConcurrent = 5
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("first POST %d", code)
	}
	checked := 0
	for _, alias := range []string{"R1", "r1."} {
		if _, err := os.Stat(filepath.Join(root, alias, ".git")); err != nil {
			continue // case-sensitive filesystem: the alias is simply another (missing) repo
		}
		checked++
		var er apiErr
		if code := do(t, "POST", srv.URL+"/api/analyses", startBody(alias, shas["r1"].Baseline, "slow"), &er); code != 409 || er.Error.Code != "analysis_running" {
			t.Fatalf("POST alias %q while r1 running: %d %+v", alias, code, er)
		}
		s.mu.Lock()
		s.insertLocked(&Analysis{ID: "aalias" + fmt.Sprint(len(alias)), Status: "done", Repo: alias, Verifications: []Verification{}})
		s.mu.Unlock()
		er = apiErr{}
		if code := do(t, "POST", srv.URL+"/api/analyses/aalias"+fmt.Sprint(len(alias))+"/verify", map[string]any{"command_id": "slow", "approved": true}, &er); code != 409 || er.Error.Code != "repo_busy" {
			t.Fatalf("verify on alias %q while r1 running: %d %+v", alias, code, er)
		}
	}
	if runtime.GOOS == "windows" && checked != 2 {
		t.Fatalf("expected both aliases to resolve on Windows, checked %d", checked)
	}
}

func loadStoredAnalysis(t *testing.T, data, id string) (Analysis, bool) {
	t.Helper()
	raw, err := store.New(data).Load(id)
	if err != nil {
		return Analysis{}, false
	}
	var a Analysis
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("stored analysis %s: %v", id, err)
	}
	return a, true
}

// The final state is persisted when the job finishes, not only when a later
// verification rewrites the record.
func TestFinishPersistsDoneAndError(t *testing.T) {
	data := t.TempDir()
	_, srv, _, shas := newServer(t, []string{"r1", "r2"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(100), slowCmd(20000)}
		c.Commands[1].ID = "stuck"
		c.JobTimeout = 3 * time.Second
	})
	var ok, stuck Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &ok); code != 202 {
		t.Fatalf("POST ok %d", code)
	}
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r2", shas["r2"].Baseline, "stuck"), &stuck); code != 202 {
		t.Fatalf("POST stuck %d", code)
	}
	if got := pollDone(t, srv.URL, ok.ID); got.Status != "done" {
		t.Fatalf("ok analysis %s %s", got.Status, got.Error)
	}
	if got := pollDone(t, srv.URL, stuck.ID); got.Status != "error" || got.Error != msgTimedOut {
		t.Fatalf("stuck analysis %s %s", got.Status, got.Error)
	}
	if a, found := loadStoredAnalysis(t, data, ok.ID); !found || a.Status != "done" || a.Model == nil || a.CurrentRun == nil {
		t.Fatalf("persisted done analysis: found=%v %+v", found, a)
	}
	if a, found := loadStoredAnalysis(t, data, stuck.ID); !found || a.Status != "error" || a.Error != msgTimedOut {
		t.Fatalf("persisted timed-out analysis: found=%v %+v", found, a)
	}
}

// Progress (test runs and evidence) is persisted while the model step is
// still running, so a crash at that point keeps the collected evidence.
func TestProgressIsPersistedBeforeModelStep(t *testing.T) {
	release := make(chan struct{})
	modelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "unavailable", 500)
	}))
	defer modelSrv.Close()
	var once bool
	defer func() {
		if !once {
			close(release)
		}
	}()
	data := t.TempDir()
	_, srv, _, shas := newServer(t, []string{"r1"}, func(c *Config) {
		c.DataDir = data
		c.Commands = []Command{slowCmd(50)}
		c.Model = &model.Client{BaseURL: modelSrv.URL, APIKey: "test", Model: "m", MaxAttempts: 1}
	})
	var a Analysis
	if code := do(t, "POST", srv.URL+"/api/analyses", startBody("r1", shas["r1"].Baseline, "slow"), &a); code != 202 {
		t.Fatalf("POST %d", code)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if st, found := loadStoredAnalysis(t, data, a.ID); found && st.Status == "running" && st.CurrentRun != nil && st.BaselineRun != nil {
			break
		}
		if time.Now().After(deadline) {
			st, _ := loadStoredAnalysis(t, data, a.ID)
			t.Fatalf("progress never persisted while model step runs: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	once = true
	close(release)
	pollDone(t, srv.URL, a.ID)
}
