// Package api serves the HTTP API described in docs/API_CONTRACT.md.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"regressionmap/internal/evidence"
	"regressionmap/internal/gitx"
	"regressionmap/internal/model"
	"regressionmap/internal/notify"
	"regressionmap/internal/proc"
	"regressionmap/internal/store"
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
	Timeout  time.Duration // per test process
	// TestEnvAllow (TEST_ENV_ALLOW) names variables that test runs and
	// verification runs receive although their names end in _KEY, _SECRET,
	// _TOKEN or _PASSWORD. Git never receives them.
	TestEnvAllow []string

	MaxConcurrent int           // running analyses across all repos (default 2)
	MaxJobs       int           // analyses kept in memory (default 100)
	JobTimeout    time.Duration // whole-analysis deadline (default 5 min)
	DataDir       string        // where analyses are persisted; "" = memory only

	// WebhookSecret enables completion notifications (notify_url) and is
	// the HMAC key of json deliveries. "" = webhooks disabled.
	WebhookSecret string
	// Notifier delivers them; nil with a secret = a notify.Sender.
	Notifier Notifier
}

// Notifier delivers one completion notification. *notify.Sender implements it.
type Notifier interface {
	Send(ctx context.Context, rawURL, format string, p notify.Payload) notify.Result
}

// Notification is the delivery status shown on an analysis. It holds the
// host only; the full notify URL is never stored here.
type Notification struct {
	Host           string `json:"host"`
	Format         string `json:"format"`
	Status         string `json:"status"` // pending | delivered | failed
	Attempts       int    `json:"attempts"`
	LastStatusCode int    `json:"last_status_code"`
	LastError      string `json:"last_error"`
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
	Warnings      []string           `json:"warnings,omitempty"` // one sentence per input the server cut
	Model         *model.Result      `json:"model"`
	Verifications []Verification     `json:"verifications"`
	CreatedAt     string             `json:"created_at"`
	FinishedAt    string             `json:"finished_at,omitempty"`
	Notification  *Notification      `json:"notification"`
}

// Summary is one row of GET /api/analyses.
type Summary struct {
	ID            string `json:"id"`
	Repo          string `json:"repo"`
	Status        string `json:"status"`
	Baseline      string `json:"baseline"`
	Current       string `json:"current"`
	CreatedAt     string `json:"created_at"`
	CurrentPassed *bool  `json:"current_passed"`
}

func (a *Analysis) summary() Summary {
	s := Summary{ID: a.ID, Repo: a.Repo, Status: a.Status, Baseline: a.Baseline, Current: a.Current, CreatedAt: a.CreatedAt}
	if a.CurrentRun != nil {
		p := a.CurrentRun.Passed
		s.CurrentPassed = &p
	}
	return s
}

type Server struct {
	cfg  Config
	mux  *http.ServeMux
	mu   sync.Mutex
	jobs map[string]*Analysis
	// order is insertion order of jobs, used to evict the oldest finished.
	order []string

	active    int               // running analyses
	running   map[string]string // repo -> id of its running analysis
	verifying map[string]bool   // repo -> a verification is running
	store     *store.Store      // nil when DataDir is empty
	saveMu    sync.Mutex        // serialises persistence so writes land in order
	tempDir   string            // where gitx creates regressionmap-wt-* dirs

	// notifyURLs holds the full notify URL of each analysis until its
	// delivery finishes. It lives only here: never in an Analysis, so it is
	// never returned, logged or persisted.
	notifyURLs map[string]pendingNotify
	// delivering holds the id of every analysis whose notification this
	// process is still sending, so loadStored keeps its stored "pending".
	delivering map[string]struct{}
	// runs holds the cancel func of every analysis whose run goroutine has
	// not returned yet. Drain cancels them and waits for them.
	runs     map[string]context.CancelFunc
	stopping bool          // Drain has begun
	changed  chan struct{} // closed when active, runs or delivering shrinks; nil while nobody waits
}

type pendingNotify struct{ url, format string }

const (
	maxOutput   = 256 << 10
	maxListed   = 50
	staleTmpAge = time.Hour
	msgTimedOut = "analysis timed out"
)

func New(cfg Config) *Server {
	if cfg.Commands == nil {
		cfg.Commands = DefaultCommands
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 100
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 5 * time.Minute
	}
	if cfg.WebhookSecret != "" && cfg.Notifier == nil {
		cfg.Notifier = &notify.Sender{Secret: []byte(cfg.WebhookSecret)}
	}
	s := &Server{
		cfg: cfg, mux: http.NewServeMux(), jobs: map[string]*Analysis{},
		running: map[string]string{}, verifying: map[string]bool{}, tempDir: os.TempDir(),
		notifyURLs: map[string]pendingNotify{},
	}
	if cfg.DataDir != "" {
		s.store = store.New(cfg.DataDir)
	}
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/repos", s.repos)
	s.mux.HandleFunc("GET /api/repos/{name}/commits", s.commits)
	s.mux.HandleFunc("GET /api/commands", s.commands)
	s.mux.HandleFunc("GET /api/analyses", s.list)
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

// repoDir resolves a repo name to its canonical name (the workspace entry
// that holds it) and its directory, or fails if the name is not a plain child
// of the workspace root that contains a .git directory. Aliases that reach the
// same directory (different case or a trailing dot on Windows) resolve to the
// same canonical name, so busy state keyed by it covers every spelling.
func (s *Server) repoDir(name string) (string, string, error) {
	errInvalid := errors.New("invalid repository name")
	errNotRepo := errors.New("not a git repository in the workspace")
	if !repoNameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", "", errInvalid
	}
	root := filepath.Clean(s.cfg.Root)
	target := filepath.Join(root, name)
	if filepath.Dir(target) != root {
		return "", "", errInvalid
	}
	want, err := os.Stat(target)
	if err != nil {
		return "", "", errNotRepo
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return "", "", errNotRepo
	}
	canon := ""
	for _, e := range ents {
		if !e.IsDir() || !repoNameRe.MatchString(e.Name()) {
			continue
		}
		if st, err := os.Stat(filepath.Join(root, e.Name())); err == nil && os.SameFile(want, st) {
			canon = e.Name()
			break
		}
	}
	if canon == "" {
		// Not a real directory entry (e.g. a symlink): only the exact
		// entry name is accepted.
		for _, e := range ents {
			if e.Name() == name {
				canon = name
				break
			}
		}
	}
	if canon == "" {
		return "", "", errNotRepo
	}
	dir := filepath.Join(root, canon)
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !st.IsDir() {
		return "", "", errNotRepo
	}
	return canon, dir, nil
}

// repoNames lists the valid repositories directly under the workspace root.
func (s *Server) repoNames() ([]string, error) {
	ents, err := os.ReadDir(s.cfg.Root)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range ents {
		if e.IsDir() {
			if canon, _, err := s.repoDir(e.Name()); err == nil && canon == e.Name() {
				out = append(out, e.Name())
			}
		}
	}
	return out, nil
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
	names, err := s.repoNames()
	if err != nil {
		fail(w, 500, "workspace_unreadable", "cannot read workspace root")
		return
	}
	out := []map[string]string{}
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	writeJSON(w, 200, map[string]any{"repos": out})
}

func (s *Server) commits(w http.ResponseWriter, r *http.Request) {
	_, dir, err := s.repoDir(r.PathValue("name"))
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

// ---- job table -------------------------------------------------------------

// copyLocked returns a copy whose slices the caller may keep. s.mu must be held.
func copyLocked(a *Analysis) Analysis {
	c := *a
	c.Verifications = append([]Verification{}, a.Verifications...)
	return c
}

// insertLocked adds a to the in-memory table and evicts the oldest finished
// analyses beyond MaxJobs. Running analyses are never evicted. s.mu must be held.
func (s *Server) insertLocked(a *Analysis) {
	if _, ok := s.jobs[a.ID]; !ok {
		s.order = append(s.order, a.ID)
	}
	s.jobs[a.ID] = a
	for len(s.jobs) > s.cfg.MaxJobs {
		victim := -1
		for i, id := range s.order {
			if j := s.jobs[id]; j != nil && j.Status != "running" {
				victim = i
				break
			}
		}
		if victim < 0 {
			return
		}
		delete(s.jobs, s.order[victim])
		s.order = append(s.order[:victim], s.order[victim+1:]...)
	}
}

// loadStored reads an analysis that is not in memory from the store. An
// analysis persisted as running belongs to a previous process that died.
func (s *Server) loadStored(id string) (Analysis, bool) {
	if s.store == nil || !store.ValidID(id) {
		return Analysis{}, false
	}
	b, err := s.store.Load(id)
	if err != nil {
		return Analysis{}, false
	}
	var a Analysis
	if err := json.Unmarshal(b, &a); err != nil || a.ID != id {
		return Analysis{}, false
	}
	if a.Status == "running" {
		a.Status, a.Error = "error", "analysis interrupted: the server stopped while it was running"
	}
	if a.ChangedFiles == nil {
		a.ChangedFiles = []gitx.ChangedFile{}
	}
	if a.Evidence == nil {
		a.Evidence = []evidence.Item{}
	}
	if a.Verifications == nil {
		a.Verifications = []Verification{}
	}
	// A stored "pending" is a delivery that a previous process did not
	// finish, unless this process is still sending it.
	if n := a.Notification; n != nil && n.Status == "pending" {
		s.mu.Lock()
		_, inFlight := s.delivering[id]
		s.mu.Unlock()
		if !inFlight {
			n.Status, n.LastError = "failed", msgDeliveryInterrupted
		}
	}
	return a, true
}

func (s *Server) snapshot(id string) (Analysis, bool) {
	s.mu.Lock()
	if a, ok := s.jobs[id]; ok {
		c := copyLocked(a)
		s.mu.Unlock()
		return c, true
	}
	s.mu.Unlock()
	return s.loadStored(id)
}

// mutate applies f to the in-memory analysis and persists the result. f
// returns false to skip the change. When the analysis is not in memory
// (evicted, or created by an earlier process) and fallback is not nil, the
// stored copy is loaded and adopted; fallback, the caller's possibly older
// snapshot, is adopted only when there is no stored copy, so it cannot
// overwrite a change saved since it was taken (a delivery outcome, say).
func (s *Server) mutate(id string, fallback *Analysis, f func(a *Analysis) bool) {
	var load func() (Analysis, bool)
	if fallback != nil {
		load = func() (Analysis, bool) {
			if a, ok := s.loadStored(id); ok {
				return a, true
			}
			return *fallback, true
		}
	}
	s.mutateWith(id, load, f)
}

// mutateWith is mutate with the fallback produced by load, which runs only
// when the analysis is not in memory. The memory check, the load and the
// change happen in one saveMu critical section; every save and eviction
// also holds saveMu, so the analysis cannot leave memory in between and the
// change cannot be lost. load is called without s.mu held.
func (s *Server) mutateWith(id string, load func() (Analysis, bool), f func(a *Analysis) bool) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	a, ok := s.jobs[id]
	if !ok {
		if load == nil {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		c, found := load()
		s.mu.Lock()
		if a, ok = s.jobs[id]; !ok {
			if !found {
				s.mu.Unlock()
				return
			}
			a = &c
			s.insertLocked(a)
		}
	}
	if !f(a) {
		s.mu.Unlock()
		return
	}
	b, err := json.Marshal(a)
	s.mu.Unlock()
	if err == nil {
		s.persist(id, b)
	}
}

func (s *Server) persist(id string, b []byte) {
	if s.store == nil {
		return
	}
	if err := s.store.Save(id, b); err != nil {
		log.Printf("persist analysis %s: %v", id, err)
	}
}

// progress updates a running analysis; it is a no-op once the job finished.
func (s *Server) progress(id string, f func(a *Analysis)) {
	s.mutate(id, nil, func(a *Analysis) bool {
		if a.Status != "running" {
			return false
		}
		f(a)
		return true
	})
}

// finish moves a running analysis to its final state and frees its repo and
// global slots in the same critical section, so a client that sees the final
// status can immediately start new work on that repo. It then starts the
// completion notification, if one was requested, without waiting for it.
func (s *Server) finish(id string, f func(a *Analysis)) {
	var (
		pn      pendingNotify
		payload notify.Payload
		host    string
		send    bool
	)
	s.mutate(id, nil, func(a *Analysis) bool {
		if a.Status != "running" {
			return false
		}
		f(a)
		a.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		if s.running[a.Repo] == id {
			delete(s.running, a.Repo)
			s.active--
			s.signalLocked()
		}
		if p, ok := s.notifyURLs[id]; ok && a.Notification != nil {
			delete(s.notifyURLs, id)
			pn, payload, host, send = p, payloadOf(a), a.Notification.Host, true
			// In the same critical section as active--, so Drain never
			// sees the analysis as neither running nor delivering.
			if s.delivering == nil {
				s.delivering = map[string]struct{}{}
			}
			s.delivering[id] = struct{}{}
		}
		return true
	})
	if send {
		go s.deliver(id, pn, payload, host)
	}
}

// payloadOf builds the webhook payload. It carries no evidence and no output.
func payloadOf(a *Analysis) notify.Payload {
	p := notify.Payload{
		ID: a.ID, Repo: a.Repo, Status: a.Status, Error: a.Error, Baseline: a.Baseline, Current: a.Current,
		CreatedAt: a.CreatedAt, FinishedAt: a.FinishedAt,
	}
	if a.BaselineRun != nil {
		p.BaselineRan, p.BaselinePassed = true, a.BaselineRun.Passed
	}
	if a.CurrentRun != nil {
		p.CurrentRan, p.CurrentPassed = true, a.CurrentRun.Passed
	}
	if a.Model != nil {
		p.ModelStatus = a.Model.Status
		if a.Model.Result != nil {
			var top *model.Hypothesis
			for i := range a.Model.Result.Hypotheses {
				if h := &a.Model.Result.Hypotheses[i]; top == nil || h.Rank < top.Rank {
					top = h
				}
			}
			if top != nil {
				p.TopHypothesis = &notify.Hypothesis{Rank: top.Rank, Title: top.Title, File: top.File,
					LineStart: top.LineStart, LineEnd: top.LineEnd, ConfidenceLabel: top.ConfidenceLabel}
			}
		}
	}
	return p
}

const (
	deliveryDeadline = 2 * time.Minute

	msgInterrupted         = "analysis interrupted: the server stopped while it was running"
	msgDeliveryInterrupted = "delivery interrupted: the server stopped before it finished"
)

// deliver sends one notification and records its outcome on the analysis.
// Only the host is ever logged or stored.
func (s *Server) deliver(id string, pn pendingNotify, p notify.Payload, host string) {
	// Leaves the in-flight set only after the outcome is recorded, so a
	// read of the stored analysis in between still sees "pending".
	defer func() {
		s.mu.Lock()
		delete(s.delivering, id)
		s.signalLocked()
		s.mu.Unlock()
	}()
	var res notify.Result
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("notification panicked", "analysis", id, "host", host, "panic", scrubURL(fmt.Sprint(r), pn.url))
				res = notify.Result{Err: "internal error"}
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), deliveryDeadline)
		defer cancel()
		res = s.cfg.Notifier.Send(ctx, pn.url, pn.format, p)
	}()
	res.Err = scrubURL(res.Err, pn.url)
	status := "failed"
	if res.Delivered {
		status, res.Err = "delivered", ""
	}
	// An analysis evicted from memory meanwhile is loaded from the store
	// inside the same critical section that records the outcome.
	s.mutateWith(id, func() (Analysis, bool) { return s.loadStored(id) }, func(a *Analysis) bool {
		if a.Notification == nil {
			return false
		}
		n := *a.Notification
		n.Status, n.Attempts, n.LastStatusCode, n.LastError = status, res.Attempts, res.StatusCode, res.Err
		a.Notification = &n
		return true
	})
	if !res.Delivered {
		slog.Warn("notification failed", "analysis", id, "host", host, "attempts", res.Attempts,
			"status_code", res.StatusCode, "error", res.Err)
	}
}

// scrubURL removes the notify URL, its path and its query from msg, before
// and after removing control characters (a decoded path can hold one, and
// one inserted into a part would otherwise hide it), as a second guard
// behind the Notifier, and caps its length at 500 characters.
func scrubURL(msg, raw string) string {
	parts := []string{raw}
	if u, err := url.Parse(raw); err == nil {
		parts = append(parts, u.RawQuery, u.EscapedPath(), u.Path)
	}
	remove := func(msg string) string {
		for _, p := range parts {
			if len(p) > 1 {
				msg = strings.ReplaceAll(msg, p, "")
			}
		}
		return msg
	}
	msg = remove(notify.StripControl(remove(msg)))
	if r := []rune(msg); len(r) > 500 {
		msg = string(r[:500])
	}
	return msg
}

// Drain is the second half of a graceful shutdown, after the HTTP server
// has stopped taking requests. It ends every running analysis with status
// error and msgInterrupted, which starts its notification, stops their test
// processes, and then waits until no analysis is running and no
// notification is being delivered. If ctx ends first it returns an error
// that wraps ctx.Err() and counts the analysis runs still stopping and the
// notifications still being sent; a delivery cut off then stays "pending"
// in DATA_DIR, which the next process reports as failed with
// msgDeliveryInterrupted.
func (s *Server) Drain(ctx context.Context) error {
	for {
		s.mu.Lock()
		s.stopping = true
		ids := make([]string, 0, len(s.running))
		for _, id := range s.running {
			ids = append(ids, id)
		}
		s.mu.Unlock()
		// Ended before their runs are cancelled, so a cancelled run cannot
		// end its analysis first with "analysis timed out".
		for _, id := range ids {
			s.interrupt(id)
		}
		s.mu.Lock()
		cancels := make([]context.CancelFunc, 0, len(s.runs))
		for _, cancel := range s.runs {
			cancels = append(cancels, cancel)
		}
		idle := s.active == 0 && len(s.runs) == 0 && len(s.delivering) == 0
		if !idle && s.changed == nil {
			s.changed = make(chan struct{})
		}
		changed := s.changed
		s.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		if idle {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			s.mu.Lock()
			runs, deliveries := len(s.runs), len(s.delivering)
			s.mu.Unlock()
			return fmt.Errorf("%d analysis run(s) still stopping, %d notification(s) still being sent: %w", runs, deliveries, ctx.Err())
		}
	}
}

// interrupt ends a running analysis because the server is stopping.
func (s *Server) interrupt(id string) {
	s.finish(id, func(a *Analysis) { a.Status, a.Error = "error", msgInterrupted })
}

// signalLocked wakes Drain after active, runs or delivering shrank. s.mu
// must be held.
func (s *Server) signalLocked() {
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// ---- handlers ------------------------------------------------------------

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo         string `json:"repo"`
		Baseline     string `json:"baseline"`
		CommandID    string `json:"command_id"`
		NotifyURL    string `json:"notify_url"`
		NotifyFormat string `json:"notify_format"`
	}
	if err := decode(w, r, &req); err != nil {
		fail(w, 400, "bad_request", "invalid JSON body")
		return
	}
	// The notification checks, in the order of the v3 addendum.
	var notifyHost string
	if req.NotifyURL != "" {
		if s.cfg.WebhookSecret == "" || s.cfg.Notifier == nil {
			fail(w, 400, "webhooks_disabled", "completion notifications are disabled on this server (WEBHOOK_SECRET is not set)")
			return
		}
		u, err := notify.Validate(req.NotifyURL)
		if err != nil {
			fail(w, 400, "invalid_notify_url", err.Error())
			return
		}
		notifyHost = strings.ToLower(u.Hostname())
	}
	format := req.NotifyFormat
	if format == "" {
		format = notify.FormatJSON
	}
	if !notify.ValidFormat(format) {
		fail(w, 400, "bad_request", "notify_format must be json or slack")
		return
	}
	repo, dir, err := s.repoDir(req.Repo)
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

	s.saveMu.Lock()
	s.mu.Lock()
	switch {
	case s.running[repo] != "":
		s.mu.Unlock()
		s.saveMu.Unlock()
		fail(w, 409, "analysis_running", "an analysis of this repository is already running")
		return
	case s.verifying[repo]:
		s.mu.Unlock()
		s.saveMu.Unlock()
		fail(w, 409, "repo_busy", "a verification of this repository is running")
		return
	case s.active >= s.cfg.MaxConcurrent:
		s.mu.Unlock()
		s.saveMu.Unlock()
		fail(w, 429, "too_many_analyses", fmt.Sprintf("at most %d analyses can run at once", s.cfg.MaxConcurrent))
		return
	}
	a := &Analysis{
		ID: store.NewID(), Status: "running", Repo: repo, Baseline: base, Current: cur,
		Command: CommandRef{ID: cmd.ID, Argv: cmd.Argv}, ChangedFiles: []gitx.ChangedFile{},
		Evidence: []evidence.Item{}, Verifications: []Verification{},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if notifyHost != "" {
		a.Notification = &Notification{Host: notifyHost, Format: format, Status: "pending"}
		s.notifyURLs[a.ID] = pendingNotify{url: req.NotifyURL, format: format}
	}
	s.insertLocked(a)
	s.running[repo] = a.ID
	s.active++
	snap := copyLocked(a)
	b, err := json.Marshal(a)
	s.mu.Unlock()
	if err == nil {
		s.persist(a.ID, b)
	}
	s.saveMu.Unlock()

	go s.supervise(a.ID, dir, cmd, base, cur)
	writeJSON(w, 202, snap)
}

// supervise runs one analysis under JobTimeout. If the deadline passes first
// the job ends as an error; the run goroutine sees the cancelled context and
// its later updates are ignored because the job is no longer running.
func (s *Server) supervise(id, dir string, cmd Command, base, cur string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.JobTimeout)
	defer cancel()
	// Registered until the run goroutine returns, so Drain can stop it and
	// wait for it.
	s.mu.Lock()
	if s.runs == nil {
		s.runs = map[string]context.CancelFunc{}
	}
	s.runs[id] = cancel
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		// Created after Drain began: end it before it runs anything.
		s.interrupt(id)
		cancel()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			s.mu.Lock()
			delete(s.runs, id)
			s.signalLocked()
			s.mu.Unlock()
		}()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("analysis panicked", "analysis", id, "panic", notify.StripControl(fmt.Sprint(p)))
				s.finish(id, func(a *Analysis) { a.Status, a.Error = "error", "internal error" })
			}
		}()
		s.run(ctx, id, dir, cmd, base, cur)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	msg := "analysis ended without a result"
	if ctx.Err() != nil {
		msg = msgTimedOut
	}
	s.finish(id, func(a *Analysis) { a.Status, a.Error = "error", msg })
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
	res := proc.Run(ctx, proc.Spec{Argv: cmd.Argv, Dir: wt, Timeout: s.cfg.Timeout, MaxOutput: maxOutput, Allow: s.cfg.TestEnvAllow})
	return res, wt, cleanup, nil
}

func (s *Server) run(ctx context.Context, id, dir string, cmd Command, base, cur string) {
	// stop ends the job with err (or the timeout) and reports whether the
	// caller must return.
	stop := func(err error) bool {
		if ctx.Err() != nil {
			err = errors.New(msgTimedOut)
		}
		if err == nil {
			return false
		}
		s.finish(id, func(a *Analysis) { a.Status, a.Error = "error", err.Error() })
		return true
	}

	// step names the git step that failed, so the analysis error says which.
	step := func(name string, err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}

	// Each test run is saved as soon as it has finished, so a later git
	// failure keeps it.
	baseRun, _, cleanBase, err := s.runAt(ctx, dir, base, cmd)
	if stop(step("baseline worktree", err)) {
		if cleanBase != nil {
			cleanBase()
		}
		return
	}
	cleanBase()
	s.progress(id, func(a *Analysis) { a.BaselineRun = &baseRun })
	curRun, curWT, cleanCur, err := s.runAt(ctx, dir, cur, cmd)
	if stop(step("HEAD worktree", err)) {
		if cleanCur != nil {
			cleanCur()
		}
		return
	}
	cleanCur()
	s.progress(id, func(a *Analysis) { a.CurrentRun = &curRun })

	// Input over a size limit is cut, never fatal; each cut adds a warning
	// (no paths: they could hold secrets) that the model is told too.
	var warnings []string
	files, cut, err := gitx.ChangedFiles(ctx, dir, base, cur)
	if stop(step("changed-file list", err)) {
		return
	}
	if cut {
		warnings = append(warnings, "changed-file list larger than 1 MiB: only the files that fit were analysed")
	}
	diff, cut, err := gitx.DiffLimited(ctx, dir, base, cur)
	if stop(step("diff", err)) {
		return
	}
	if cut {
		warnings = append(warnings, "diff larger than 8 MiB: only the first 8 MiB were analysed")
	}
	partial := map[string]bool{} // files read only in part
	failures := evidence.ParseNodeTest(curRun.Stdout, curWT)
	ev := evidence.Build(failures, evidence.ParseDiff(diff), func(p string) (string, bool, error) {
		src, cut, err := gitx.ShowFile(ctx, dir, cur, p)
		if cut {
			partial[p] = true
		}
		return src, cut, err
	}, evidence.Limits{MaxExcerpt: 3000, MaxTotal: 40000})
	if len(partial) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d file(s) larger than 1 MiB were read only in part", len(partial)))
	}

	s.progress(id, func(a *Analysis) {
		a.ChangedFiles, a.Evidence, a.Warnings = files, ev, warnings
	})

	// The model gets the same path redaction as the evidence, so no secret
	// in a path leaves the server and cited paths match Item.File. The API's
	// changed_files keeps the real paths.
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = evidence.RedactPath(f.Path)
	}
	mr := s.cfg.Model.Analyze(ctx, model.Input{
		Command: strings.Join(cmd.Argv, " "), BaselinePassed: baseRun.Passed, CurrentPassed: curRun.Passed,
		CurrentExit: curRun.ExitCode, ChangedFiles: paths, Evidence: ev, Warnings: warnings,
	})
	if stop(nil) {
		return
	}
	s.finish(id, func(a *Analysis) { a.Model, a.Status = &mr, "done" })
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	a, ok := s.snapshot(r.PathValue("id"))
	if !ok {
		fail(w, 404, "not_found", "no such analysis")
		return
	}
	writeJSON(w, 200, a)
}

// list returns summaries of in-memory and stored analyses, newest first.
// IDs are time-sortable, so string order is creation order.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	rows := map[string]Summary{}
	s.mu.Lock()
	for id, a := range s.jobs {
		rows[id] = a.summary()
	}
	s.mu.Unlock()
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	if s.store != nil {
		stored, err := s.store.List()
		if err != nil {
			log.Printf("list stored analyses: %v", err)
		}
		for _, id := range stored {
			if _, ok := rows[id]; !ok {
				ids = append(ids, id)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := []Summary{}
	for _, id := range ids {
		if len(out) == maxListed {
			break
		}
		if row, ok := rows[id]; ok {
			out = append(out, row)
		} else if a, ok := s.loadStored(id); ok {
			out = append(out, a.summary())
		}
	}
	writeJSON(w, 200, map[string]any{"analyses": out})
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
	repo, dir, err := s.repoDir(a.Repo)
	if err != nil {
		fail(w, 400, "invalid_repo", err.Error())
		return
	}
	s.mu.Lock()
	if s.running[repo] != "" || s.verifying[repo] {
		s.mu.Unlock()
		fail(w, 409, "repo_busy", "an analysis or verification of this repository is running")
		return
	}
	s.verifying[repo] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.verifying, repo)
		s.mu.Unlock()
	}()

	res := proc.Run(r.Context(), proc.Spec{Argv: cmd.Argv, Dir: dir, Timeout: s.cfg.Timeout, MaxOutput: maxOutput, Allow: s.cfg.TestEnvAllow})
	v := Verification{CommandID: cmd.ID, Argv: cmd.Argv, At: time.Now().UTC().Format(time.RFC3339), Run: res}
	s.mutate(a.ID, &a, func(j *Analysis) bool {
		j.Verifications = append(j.Verifications, v)
		return true
	})
	writeJSON(w, 200, v)
}

// Cleanup is startup hygiene: it removes regressionmap-wt-* temp dirs older
// than an hour (left by a crashed process) and then runs "git worktree prune"
// in every workspace repo that is not busy, dropping their registrations.
func (s *Server) Cleanup(ctx context.Context) error {
	var errs []error
	if ents, err := os.ReadDir(s.tempDir); err == nil {
		cutoff := time.Now().Add(-staleTmpAge)
		for _, e := range ents {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "regressionmap-wt-") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(s.tempDir, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	} else {
		errs = append(errs, err)
	}
	names, err := s.repoNames()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, n := range names {
		s.mu.Lock()
		busy := s.running[n] != "" || s.verifying[n]
		s.mu.Unlock()
		if busy {
			continue
		}
		_, dir, err := s.repoDir(n)
		if err == nil {
			err = gitx.Prune(ctx, dir)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
		}
	}
	return errors.Join(errs...)
}
