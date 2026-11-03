// Package api serves the HTTP API described in docs/API_CONTRACT.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"regressionmap/internal/evidence"
	"regressionmap/internal/gitx"
	"regressionmap/internal/model"
	"regressionmap/internal/proc"
)

// Command is one allowlisted test command. The UI selects it by ID only.
type Command struct {
	ID          string   `json:"id"`
	Argv        []string `json:"argv"`
	Description string   `json:"description"`
}

var DefaultCommands = []Command{
	{ID: "node-test", Argv: []string{"node", "--test"}, Description: "Run the node:test suite"},
}

type Config struct {
	Root     string // workspace root; repos are its direct children
	Model    *model.Client
	Commands []Command
	Timeout  time.Duration
}

type CommandRef struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
}

type Verification struct {
	CommandID string      `json:"command_id"`
	Argv      []string    `json:"argv"`
	At        string      `json:"at"`
	Run       proc.Result `json:"run"`
}

type Analysis struct {
	ID            string             `json:"id"`
	Status        string             `json:"status"`
	Error         string             `json:"error,omitempty"`
	Repo          string             `json:"repo"`
	Baseline      string             `json:"baseline"`
	Current       string             `json:"current"`
	Command       CommandRef         `json:"command"`
	BaselineRun   *proc.Result       `json:"baseline_run"`
	CurrentRun    *proc.Result       `json:"current_run"`
	ChangedFiles  []gitx.ChangedFile `json:"changed_files"`
	Evidence      []evidence.Item    `json:"evidence"`
	Model         *model.Result      `json:"model"`
	Verifications []Verification     `json:"verifications"`
}

type Server struct {
	cfg  Config
	mux  *http.ServeMux
	mu   sync.Mutex
	seq  int
	jobs map[string]*Analysis
}

const maxOutput = 256 << 10

func New(cfg Config) *Server {
	if cfg.Commands == nil {
		cfg.Commands = DefaultCommands
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), jobs: map[string]*Analysis{}}
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/repos", s.repos)
	s.mux.HandleFunc("GET /api/repos/{name}/commits", s.commits)
	s.mux.HandleFunc("GET /api/commands", s.commands)
	s.mux.HandleFunc("POST /api/analyses", s.create)
	s.mux.HandleFunc("GET /api/analyses/{id}", s.get)
	s.mux.HandleFunc("POST /api/analyses/{id}/verify", s.verify)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// repoDir returns the directory for a repo name, or an error if the name is
// not a plain child of the workspace root that contains a .git directory.
func (s *Server) repoDir(name string) (string, error) {
	if !repoNameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", errors.New("invalid repository name")
	}
	dir := filepath.Join(s.cfg.Root, name)
	if filepath.Dir(dir) != filepath.Clean(s.cfg.Root) {
		return "", errors.New("invalid repository name")
	}
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !st.IsDir() {
		return "", errors.New("not a git repository in the workspace")
	}
	return dir, nil
}

func (s *Server) command(id string) (Command, bool) {
	for _, c := range s.cfg.Commands {
		if c.ID == id {
			return c, true
		}
	}
	return Command{}, false
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "model_configured": s.cfg.Model.Configured(), "model_id": s.cfg.Model.Model})
}

func (s *Server) repos(w http.ResponseWriter, r *http.Request) {
	ents, err := os.ReadDir(s.cfg.Root)
	if err != nil {
		fail(w, 500, "workspace_unreadable", "cannot read workspace root")
		return
	}
	out := []map[string]string{}
	for _, e := range ents {
		if e.IsDir() {
			if _, err := s.repoDir(e.Name()); err == nil {
				out = append(out, map[string]string{"name": e.Name()})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"repos": out})
}

func (s *Server) commits(w http.ResponseWriter, r *http.Request) {
	dir, err := s.repoDir(r.PathValue("name"))
	if err != nil {
		fail(w, 400, "invalid_repo", err.Error())
		return
	}
	cs, err := gitx.Commits(r.Context(), dir, 50)
	if err != nil {
		fail(w, 500, "git_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"commits": cs})
}

func (s *Server) commands(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"commands": s.cfg.Commands})
}

func (s *Server) snapshot(id string) (Analysis, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.jobs[id]
	if !ok {
		return Analysis{}, false
	}
	c := *a
	c.Verifications = append([]Verification{}, a.Verifications...)
	return c, true
}

func (s *Server) update(id string, f func(a *Analysis)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.jobs[id])
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo      string `json:"repo"`
		Baseline  string `json:"baseline"`
		CommandID string `json:"command_id"`
	}
	if err := decode(w, r, &req); err != nil {
		fail(w, 400, "bad_request", "invalid JSON body")
		return
	}
	dir, err := s.repoDir(req.Repo)
	if err != nil {
		fail(w, 400, "invalid_repo", err.Error())
		return
	}
	cmd, ok := s.command(req.CommandID)
	if !ok {
		fail(w, 400, "unknown_command", "command_id is not in the allowlist")
		return
	}
	base, err := gitx.ResolveCommit(r.Context(), dir, req.Baseline)
	if err != nil {
		fail(w, 400, "invalid_baseline", err.Error())
		return
	}
	cur, err := gitx.ResolveCommit(r.Context(), dir, "HEAD")
	if err != nil {
		fail(w, 400, "invalid_repo", "repository has no HEAD commit")
		return
	}
	s.mu.Lock()
	s.seq++
	a := &Analysis{
		ID: fmt.Sprintf("a%d", s.seq), Status: "running", Repo: req.Repo, Baseline: base, Current: cur,
		Command: CommandRef{ID: cmd.ID, Argv: cmd.Argv}, ChangedFiles: []gitx.ChangedFile{},
		Evidence: []evidence.Item{}, Verifications: []Verification{},
	}
	s.jobs[a.ID] = a
	snap := *a
	s.mu.Unlock()

	go s.run(a.ID, dir, cmd, base, cur)
	writeJSON(w, 202, snap)
}

// runAt runs cmd in a temporary worktree checked out at sha. It returns the
// worktree path (needed to relativise stack traces) and its cleanup func.
func (s *Server) runAt(ctx context.Context, dir, sha string, cmd Command) (proc.Result, string, func(), error) {
	wt, cleanup, err := gitx.Worktree(ctx, dir, sha)
	if err != nil {
		return proc.Result{}, "", nil, err
	}
	if real, err := filepath.EvalSymlinks(wt); err == nil {
		wt = real
	}
	res := proc.Run(ctx, proc.Spec{Argv: cmd.Argv, Dir: wt, Timeout: s.cfg.Timeout, MaxOutput: maxOutput})
	return res, wt, cleanup, nil
}

func (s *Server) run(id, dir string, cmd Command, base, cur string) {
	ctx := context.Background()
	failJob := func(err error) {
		s.update(id, func(a *Analysis) { a.Status, a.Error = "error", err.Error() })
	}

	baseRun, _, cleanBase, err := s.runAt(ctx, dir, base, cmd)
	if err != nil {
		failJob(err)
		return
	}
	cleanBase()
	curRun, curWT, cleanCur, err := s.runAt(ctx, dir, cur, cmd)
	if err != nil {
		failJob(err)
		return
	}
	cleanCur()

	files, err := gitx.ChangedFiles(ctx, dir, base, cur)
	if err != nil {
		failJob(err)
		return
	}
	diff, err := gitx.Diff(ctx, dir, base, cur)
	if err != nil {
		failJob(err)
		return
	}
	failures := evidence.ParseNodeTest(curRun.Stdout, curWT)
	ev := evidence.Build(failures, evidence.ParseDiff(diff), func(p string) (string, error) {
		return gitx.ShowFile(ctx, dir, cur, p)
	}, evidence.Limits{MaxExcerpt: 3000, MaxTotal: 40000})

	s.update(id, func(a *Analysis) {
		a.BaselineRun, a.CurrentRun, a.ChangedFiles, a.Evidence = &baseRun, &curRun, files, ev
	})

	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	mr := s.cfg.Model.Analyze(ctx, model.Input{
		Command: strings.Join(cmd.Argv, " "), BaselinePassed: baseRun.Passed, CurrentPassed: curRun.Passed,
		CurrentExit: curRun.ExitCode, ChangedFiles: paths, Evidence: ev,
	})
	s.update(id, func(a *Analysis) { a.Model, a.Status = &mr, "done" })
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	a, ok := s.snapshot(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "no such analysis")
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	a, ok := s.snapshot(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "no such analysis")
		return
	}
	var req struct {
		CommandID string `json:"command_id"`
		Approved  *bool  `json:"approved"`
	}
	if err := decode(w, r, &req); err != nil {
		fail(w, 400, "bad_request", "invalid JSON body")
		return
	}
	if req.Approved == nil || !*req.Approved {
		fail(w, 400, "not_approved", "verification requires explicit user approval (approved: true)")
		return
	}
	cmd, ok := s.command(req.CommandID)
	if !ok {
		fail(w, 400, "unknown_command", "command_id is not in the allowlist")
		return
	}
	dir, err := s.repoDir(a.Repo)
	if err != nil {
		fail(w, 400, "invalid_repo", err.Error())
		return
	}
	res := proc.Run(r.Context(), proc.Spec{Argv: cmd.Argv, Dir: dir, Timeout: s.cfg.Timeout, MaxOutput: maxOutput})
	v := Verification{CommandID: cmd.ID, Argv: cmd.Argv, At: time.Now().UTC().Format(time.RFC3339), Run: res}
	s.update(a.ID, func(a *Analysis) { a.Verifications = append(a.Verifications, v) })
	writeJSON(w, 200, v)
}
